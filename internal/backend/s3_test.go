package backend

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/martinmares/nfs-gate/internal/activity"
	nfs "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	nfsfile "github.com/willscott/go-nfs/file"
	"github.com/willscott/go-nfs/helpers"
)

type storedObject struct {
	data     []byte
	meta     map[string]string
	modified time.Time
}
type objectStore struct {
	mu                     sync.Mutex
	objects                map[string]storedObject
	failPut, failList      bool
	putStarted, putRelease chan struct{}
}

func newS3Fixture(t *testing.T, max int64) (*S3, *objectStore, S3Config) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	store := &objectStore{objects: map[string]storedObject{}}
	srv := httptest.NewServer(http.HandlerFunc(store.serve))
	t.Cleanup(srv.Close)
	cfg := S3Config{Bucket: "test-bucket", Prefix: "export", Region: "us-east-1", Endpoint: srv.URL, PathStyle: true, MaxFileSize: max, Timeout: time.Second}
	f, err := OpenS3(context.Background(), cfg, activity.New(100))
	if err != nil {
		t.Fatal(err)
	}
	return f, store, cfg
}
func (s *objectStore) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(code int, name string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(code)
		fmt.Fprintf(w, "<Error><Code>%s</Code><Message>test failure</Message></Error>", name)
	}
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	if r.URL.Query().Get("list-type") == "2" {
		if s.failList {
			fail(503, "SlowDown")
			return
		}
		prefix, delimiter := r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter")
		type entry struct {
			key string
			dir bool
		}
		entries := map[string]entry{}
		for k := range s.objects {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if delimiter != "" {
				rest := strings.TrimPrefix(k, prefix)
				if at := strings.Index(rest, delimiter); at >= 0 {
					d := prefix + rest[:at+1]
					entries[d] = entry{d, true}
					continue
				}
			}
			entries[k] = entry{k, false}
		}
		sorted := []entry{}
		for _, e := range entries {
			sorted = append(sorted, e)
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].key < sorted[j].key })
		start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
		max, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
		if max > 2 {
			max = 2
		}
		end := start + max
		if end > len(sorted) {
			end = len(sorted)
		}
		type content struct {
			Key          string
			Size         int
			LastModified string
		}
		type common struct{ Prefix string }
		out := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			IsTruncated           bool
			NextContinuationToken string `xml:",omitempty"`
			Contents              []content
			CommonPrefixes        []common
		}{IsTruncated: end < len(sorted)}
		if out.IsTruncated {
			out.NextContinuationToken = strconv.Itoa(end)
		}
		for _, e := range sorted[start:end] {
			if e.dir {
				out.CommonPrefixes = append(out.CommonPrefixes, common{e.key})
			} else {
				o := s.objects[e.key]
				out.Contents = append(out.Contents, content{e.key, len(o.data), o.modified.UTC().Format(time.RFC3339)})
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(out)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if s.failPut {
			fail(503, "SlowDown")
			return
		}
		if s.putStarted != nil {
			close(s.putStarted)
			<-s.putRelease
			s.putStarted = nil
		}
		data, e := io.ReadAll(r.Body)
		if e != nil {
			fail(400, "InvalidRequest")
			return
		}
		m := map[string]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				m[strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")] = v[0]
			}
		}
		s.objects[key] = storedObject{data: data, meta: m, modified: time.Now()}
		w.Header().Set("ETag", "\"test-etag\"")
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(204)
	case http.MethodHead, http.MethodGet:
		o, ok := s.objects[key]
		if !ok {
			fail(404, "NoSuchKey")
			return
		}
		for k, v := range o.meta {
			w.Header().Set("X-Amz-Meta-"+k, v)
		}
		w.Header().Set("Last-Modified", o.modified.UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", "\"test-etag\"")
		data := o.data
		if raw := r.Header.Get("Range"); raw != "" {
			var a, b int
			_, _ = fmt.Sscanf(raw, "bytes=%d-%d", &a, &b)
			if a >= len(data) {
				fail(416, "InvalidRange")
				return
			}
			if b >= len(data) {
				b = len(data) - 1
			}
			data = data[a : b+1]
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(206)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	default:
		fail(405, "InvalidRequest")
	}
}
func readFile(t *testing.T, f *S3, name string) []byte {
	t.Helper()
	h, e := f.Open(name)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	b, e := io.ReadAll(h)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func writeFile(t *testing.T, f *S3, name string, data []byte) {
	t.Helper()
	h, e := f.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0640)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if _, e = h.Write(data); e != nil {
		t.Fatal(e)
	}
}

func TestS3FilesystemThroughSDK(t *testing.T) {
	f, store, cfg := newS3Fixture(t, 128)
	if e := f.MkdirAll("dir/sub", 0750); e != nil {
		t.Fatal(e)
	}
	writeFile(t, f, "dir/sub/file", []byte("hello"))
	// UNCHECKED NFS CREATE must preserve existing content.
	h, e := f.Create("dir/sub/file")
	if e != nil {
		t.Fatal(e)
	}
	h.Close()
	if got := readFile(t, f, "dir/sub/file"); string(got) != "hello" {
		t.Fatalf("create truncated: %q", got)
	}
	h, e = f.OpenFile("dir/sub/file", os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.Seek(8, io.SeekStart); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Write([]byte("!")); e != nil {
		t.Fatal(e)
	}
	if got := readFile(t, f, "dir/sub/file"); !bytes.Equal(got, []byte{'h', 'e', 'l', 'l', 'o', 0, 0, 0, '!'}) {
		t.Fatalf("sparse write: %q", got)
	}
	if e = h.Truncate(2); e != nil {
		t.Fatal(e)
	}
	h.Close()
	mt := time.Unix(1700000000, 123456000)
	if e = f.Chmod("dir/sub/file", 0600); e != nil {
		t.Fatal(e)
	}
	if e = f.Chown("dir/sub/file", 42, 43); e != nil {
		t.Fatal(e)
	}
	if e = f.Chtimes("dir/sub/file", mt, mt); e != nil {
		t.Fatal(e)
	}
	if e = f.Rename("dir/sub/file", "dir/renamed"); e != nil {
		t.Fatal(e)
	}
	// A fresh backend reads persistent objects and attributes, without a cache.
	again, e := OpenS3(context.Background(), cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got := readFile(t, again, "dir/renamed"); string(got) != "he" {
		t.Fatalf("restart data %q", got)
	}
	i, e := again.Stat("dir/renamed")
	if e != nil {
		t.Fatal(e)
	}
	meta := i.Sys().(nfsfile.FileInfo)
	if i.Mode().Perm() != 0600 || !i.ModTime().Equal(mt) || meta.UID != 42 || meta.GID != 43 {
		t.Fatalf("metadata %+v %+v", i, meta)
	}
	if e = f.Remove("dir"); !errors.Is(e, syscall.ENOTEMPTY) {
		t.Fatalf("remove nonempty: %v", e)
	}
	if e = f.Rename("dir", "new-dir"); !errors.Is(e, syscall.ENOTSUP) {
		t.Fatalf("dir rename: %v", e)
	}
	if e = f.Symlink("dir", "link"); !errors.Is(e, syscall.ENOTSUP) {
		t.Fatalf("symlink: %v", e)
	}
	if e = f.Remove("dir/renamed"); e != nil {
		t.Fatal(e)
	}
	if e = f.Remove("dir/sub"); e != nil {
		t.Fatal(e)
	}
	if e = f.Remove("dir"); e != nil {
		t.Fatal(e)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) != 0 {
		t.Fatalf("objects left: %+v", store.objects)
	}
}

func TestS3FailuresBoundsAndConcurrentHandles(t *testing.T) {
	f, store, _ := newS3Fixture(t, 16)
	writeFile(t, f, "shared", []byte("0000"))
	a, e := f.OpenFile("shared", os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := f.OpenFile("shared", os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	_, _ = b.Seek(2, io.SeekStart)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, e := a.Write([]byte("aa")); e != nil {
			t.Error(e)
		}
	}()
	go func() {
		defer wg.Done()
		if _, e := b.Write([]byte("bb")); e != nil {
			t.Error(e)
		}
	}()
	wg.Wait()
	if got := readFile(t, f, "shared"); string(got) != "aabb" {
		t.Fatalf("lost concurrent write: %q", got)
	}
	store.mu.Lock()
	store.failPut = true
	store.mu.Unlock()
	if n, e := a.Write([]byte("bad")); e == nil || n != 0 {
		t.Fatalf("failed upload reported success: %d %v", n, e)
	}
	if got := readFile(t, f, "shared"); string(got) != "aabb" {
		t.Fatalf("failed upload changed data %q", got)
	}
	store.mu.Lock()
	store.failPut = false
	store.mu.Unlock()
	if e := a.Truncate(17); !errors.Is(e, syscall.EFBIG) {
		t.Fatalf("size bound %v", e)
	}
	_, _ = a.Seek(16, io.SeekStart)
	if _, e := a.Write([]byte("x")); !errors.Is(e, syscall.EFBIG) {
		t.Fatalf("offset bound %v", e)
	}
	if e := a.Truncate(-1); !errors.Is(e, os.ErrInvalid) {
		t.Fatalf("negative truncate %v", e)
	}
	for _, name := range []string{"../escape", "/absolute", "dir/../escape", "bad\\path", "bad\x00path"} {
		if _, e := f.Create(name); !errors.Is(e, os.ErrPermission) {
			t.Fatalf("path %q: %v", name, e)
		}
	}
	if _, e := f.OpenFile("shared", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600); !errors.Is(e, os.ErrExist) {
		t.Fatalf("exclusive create %v", e)
	}
	store.mu.Lock()
	store.objects["export/huge"] = storedObject{data: make([]byte, 17), modified: time.Now()}
	store.failList = true
	store.mu.Unlock()
	if e := f.Check(context.Background()); e == nil {
		t.Fatal("readiness accepted unavailable S3")
	}
	store.mu.Lock()
	store.failList = false
	store.mu.Unlock()
	h, e := f.OpenFile("huge", os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if _, e := h.Write([]byte("x")); !errors.Is(e, syscall.EFBIG) {
		t.Fatalf("external object bound %v", e)
	}
}

func TestS3WriteWaitsForUpload(t *testing.T) {
	f, s, _ := newS3Fixture(t, 32)
	writeFile(t, f, "file", []byte("old"))
	h, e := f.OpenFile("file", os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	started, release := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.putStarted = started
	s.putRelease = release
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, e := h.Write([]byte("new")); done <- e }()
	<-started
	select {
	case e := <-done:
		t.Fatalf("write returned before S3 response: %v", e)
	default:
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if got := readFile(t, f, "file"); string(got) != "new" {
		t.Fatalf("saved %q", got)
	}
}

func TestS3ListingPaginationAndPrefixIsolation(t *testing.T) {
	f, s, _ := newS3Fixture(t, 32)
	s.mu.Lock()
	for _, key := range []string{"export/a", "export/b", "export/c", "export/nested/file", "elsewhere/private", "export/../escape"} {
		s.objects[key] = storedObject{data: []byte("data"), modified: time.Now()}
	}
	s.mu.Unlock()
	items, more, e := f.ListPage(".", 0, 2)
	if e != nil || len(items) != 2 || !more {
		t.Fatalf("page1 %v %v %v", items, more, e)
	}
	items, more, e = f.ListPage(".", 1, 2)
	if e != nil || len(items) != 2 || more || items[0].Name() != "c" || !items[1].IsDir() {
		t.Fatalf("page2 %v %v %v", items, more, e)
	}
	sub, e := f.Chroot("nested")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := sub.Stat("file"); e != nil {
		t.Fatal(e)
	}
	if _, e := sub.Stat("../a"); !errors.Is(e, os.ErrPermission) {
		t.Fatalf("chroot escape %v", e)
	}
}

func TestS3TwoNFSClientsAndUploadFailure(t *testing.T) {
	f, s, _ := newS3Fixture(t, 1024)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() { _ = nfs.Serve(listener, helpers.NewCachingHandler(helpers.NewNullAuthHandler(f), 1000)) }()
	client := func() *nfsclient.Target {
		t.Helper()
		c, e := rpc.DialTCP("tcp", listener.Addr().String(), false)
		if e != nil {
			t.Fatal(e)
		}
		m := &nfsclient.Mount{Client: c}
		target, e := m.Mount("/", rpc.AuthNull)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { target.Close() })
		return target
	}
	a, b := client(), client()
	h, e := a.OpenFile("shared.txt", 0644)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.Write([]byte("from A")); e != nil {
		t.Fatal(e)
	}
	if e = h.Close(); e != nil {
		t.Fatal(e)
	}
	other, e := b.Open("shared.txt")
	if e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, 6)
	if _, e = io.ReadFull(other, buf); e != nil {
		t.Fatal(e)
	}
	other.Close()
	if string(buf) != "from A" {
		t.Fatalf("B read %q", buf)
	}
	if _, e = a.Mkdir("dir", 0755); e != nil {
		t.Fatal(e)
	}
	if e = a.Rename("shared.txt", "dir/moved.txt"); e != nil {
		t.Fatal(e)
	}
	if e = b.Setattr("dir/moved.txt", nfsclient.Sattr3{Size: nfsclient.SetSize{SetIt: true, Size: 4}}); e != nil {
		t.Fatal(e)
	}
	if got := readFile(t, f, "dir/moved.txt"); string(got) != "from" {
		t.Fatalf("NFS truncate %q", got)
	}
	h, e = a.OpenFile("dir/moved.txt", 0644)
	if e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	s.failPut = true
	s.mu.Unlock()
	if _, e = h.Write([]byte("bad")); e == nil {
		t.Fatal("NFS acknowledged failed S3 upload")
	}
	h.Close()
	s.mu.Lock()
	s.failPut = false
	s.mu.Unlock()
	if e = b.Remove("dir/moved.txt"); e != nil {
		t.Fatal(e)
	}
	if e = a.RmDir("dir"); e != nil {
		t.Fatal(e)
	}
}

func TestS3DirectoryTimesInvalidateClientCaches(t *testing.T) {
	f, _, _ := newS3Fixture(t, 32)
	rootBefore, err := f.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.MkdirAll("dir", 0755); err != nil {
		t.Fatal(err)
	}
	rootAfter, _ := f.Stat(".")
	if !rootAfter.ModTime().After(rootBefore.ModTime()) {
		t.Fatal("mkdir did not change root mtime")
	}
	before, _ := f.Stat("dir")
	writeFile(t, f, "dir/file", []byte("x"))
	after, _ := f.Stat("dir")
	if !after.ModTime().After(before.ModTime()) {
		t.Fatal("create did not change parent mtime")
	}
	if err := f.Remove("dir/file"); err != nil {
		t.Fatal(err)
	}
	removed, _ := f.Stat("dir")
	if !removed.ModTime().After(after.ModTime()) {
		t.Fatal("remove did not change parent mtime")
	}
}
