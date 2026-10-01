package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/go-git/go-billy/v5"
	"github.com/martinmares/nfs-gate/internal/activity"
	nfsfile "github.com/willscott/go-nfs/file"
)

const DefaultS3MaxFileSize int64 = 64 << 20

// S3Config never contains credentials. The SDK uses its normal credential chain.
type S3Config struct {
	Bucket, Prefix, Region, Endpoint string
	PathStyle                        bool
	MaxFileSize                      int64
	Timeout                          time.Duration
}

type s3API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type s3State struct {
	mu       sync.Mutex // All operations, including read/modify/write, share this lock.
	client   s3API
	ctx      context.Context
	cfg      S3Config
	events   *activity.Store
	started  time.Time
	dirTimes map[string]time.Time
}

// S3 translates a restricted filesystem to plain objects and directory markers.
// Every mutation is uploaded synchronously. There is no dirty local cache.
type S3 struct {
	state  *s3State
	prefix string
}

var _ Filesystem = (*S3)(nil)
var _ billy.Capable = (*S3)(nil)

func s3Path(name string) (string, error) {
	if strings.ContainsAny(name, "\\\x00") {
		return "", os.ErrPermission
	}
	return clean(name)
}

func OpenS3(ctx context.Context, cfg S3Config, events *activity.Store) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("--s3-bucket is required")
	}
	p, err := s3Path(cfg.Prefix)
	if err != nil {
		return nil, fmt.Errorf("S3 prefix: %w", err)
	}
	if p == "." {
		p = ""
	} else {
		p += "/"
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = DefaultS3MaxFileSize
	}
	if cfg.MaxFileSize < 1 || cfg.MaxFileSize > 5_000_000_000 {
		return nil, errors.New("S3 max file size must be between 1 byte and 5 GB")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("S3 timeout must be positive")
	}
	if cfg.Endpoint != "" {
		u, e := url.Parse(cfg.Endpoint)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("S3 endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	opts := []func(*config.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("S3 configuration: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("set --s3-region or AWS_REGION")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		// Optional SDK checksums are not supported by every S3-compatible service.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	f := &S3{state: &s3State{client: client, ctx: ctx, cfg: cfg, events: events, started: time.Now(), dirTimes: make(map[string]time.Time)}, prefix: p}
	if err := f.Check(ctx); err != nil {
		return nil, fmt.Errorf("S3 export unavailable: %w", err)
	}
	return f, nil
}

func (f *S3) Root() string {
	return "s3://" + f.state.cfg.Bucket + "/" + strings.TrimSuffix(f.prefix, "/")
}
func (f *S3) Kind() string                   { return "s3" }
func (f *S3) Close() error                   { return nil }
func (f *S3) Capabilities() billy.Capability { return billy.AllCapabilities &^ billy.LockCapability }
func (f *S3) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, f.state.cfg.Timeout)
	defer cancel()
	_, err := f.state.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.state.cfg.Bucket), Prefix: aws.String(f.prefix), MaxKeys: aws.Int32(1)})
	return s3Error(err)
}
func (f *S3) request() (context.Context, context.CancelFunc) {
	return context.WithTimeout(f.state.ctx, f.state.cfg.Timeout)
}
func s3Error(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return os.ErrNotExist
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return os.ErrPermission
		}
	}
	return err
}
func (f *S3) record(op, name string, err error) {
	if f.state.events != nil {
		f.state.events.Add(op, name, err)
	}
}
func (f *S3) key(name string) string {
	if name == "." {
		return f.prefix
	}
	return f.prefix + name
}
func (f *S3) head(key string) (*s3.HeadObjectOutput, error) {
	ctx, cancel := f.request()
	defer cancel()
	out, err := f.state.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(f.state.cfg.Bucket), Key: aws.String(key)})
	return out, s3Error(err)
}
func (f *S3) put(key string, data []byte, meta map[string]string) error {
	ctx, cancel := f.request()
	defer cancel()
	_, err := f.state.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.state.cfg.Bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), Metadata: meta})
	return s3Error(err)
}
func (f *S3) delete(key string) error {
	ctx, cancel := f.request()
	defer cancel()
	_, err := f.state.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(f.state.cfg.Bucket), Key: aws.String(key)})
	return s3Error(err)
}
func (f *S3) list(prefix, delimiter string, token *string, max int32) (*s3.ListObjectsV2Output, error) {
	ctx, cancel := f.request()
	defer cancel()
	out, err := f.state.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.state.cfg.Bucket), Prefix: aws.String(prefix), Delimiter: aws.String(delimiter), ContinuationToken: token, MaxKeys: aws.Int32(max)})
	return out, s3Error(err)
}

type objectInfo struct {
	name     string
	size     int64
	mode     os.FileMode
	modified time.Time
	uid, gid uint32
	id       uint64
}

func (i objectInfo) Name() string       { return i.name }
func (i objectInfo) Size() int64        { return i.size }
func (i objectInfo) Mode() os.FileMode  { return i.mode }
func (i objectInfo) ModTime() time.Time { return i.modified }
func (i objectInfo) IsDir() bool        { return i.mode.IsDir() }
func (i objectInfo) Sys() any {
	return nfsfile.FileInfo{Nlink: 1, UID: i.uid, GID: i.gid, Fileid: i.id}
}
func (f *S3) info(name string, h *s3.HeadObjectOutput, dir bool) objectInfo {
	i := objectInfo{name: path.Base(name), mode: 0644, uid: 65532, gid: 65532, modified: f.state.started}
	if dir {
		i.mode = 0755 | os.ModeDir
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(f.key(name)))
	i.id = hash.Sum64()
	if h != nil {
		if !dir {
			i.size = aws.ToInt64(h.ContentLength)
		}
		i.modified = aws.ToTime(h.LastModified)
		if mode, e := strconv.ParseUint(h.Metadata["nfs-mode"], 8, 32); e == nil {
			i.mode = i.mode.Type() | os.FileMode(mode)&os.ModePerm
		}
		if t, e := time.Parse(time.RFC3339Nano, h.Metadata["nfs-mtime"]); e == nil {
			i.modified = t
		}
		if v, e := strconv.ParseUint(h.Metadata["nfs-uid"], 10, 32); e == nil {
			i.uid = uint32(v)
		}
		if v, e := strconv.ParseUint(h.Metadata["nfs-gid"], 10, 32); e == nil {
			i.gid = uint32(v)
		}
	}
	if dir {
		if t := f.state.dirTimes[strings.TrimSuffix(f.key(name), "/")]; t.After(i.modified) {
			i.modified = t
		}
	}
	return i
}
func (f *S3) touchParent(name string) {
	f.state.dirTimes[strings.TrimSuffix(f.key(path.Dir(name)), "/")] = time.Now()
}
func metadata(mode os.FileMode) map[string]string {
	return map[string]string{"nfs-mode": strconv.FormatUint(uint64(mode.Perm()), 8), "nfs-mtime": time.Now().UTC().Format(time.RFC3339Nano), "nfs-uid": "65532", "nfs-gid": "65532"}
}
func (f *S3) stat(name string) (os.FileInfo, error) {
	if name == "." {
		return f.info(name, nil, true), nil
	}
	h, err := f.head(f.key(name))
	if err == nil {
		return f.info(name, h, false), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	h, err = f.head(f.key(name) + "/")
	if err == nil {
		return f.info(name, h, true), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out, err := f.list(f.key(name)+"/", "", nil, 1)
	if err != nil {
		return nil, err
	}
	if len(out.Contents) > 0 {
		return f.info(name, nil, true), nil
	}
	return nil, os.ErrNotExist
}
func (f *S3) Stat(name string) (os.FileInfo, error) {
	p, err := s3Path(name)
	if err != nil {
		return nil, err
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	return f.stat(p)
}
func (f *S3) Lstat(name string) (os.FileInfo, error) { return f.Stat(name) }
func (f *S3) Join(elem ...string) string             { return path.Join(elem...) }
func (f *S3) Readlink(string) (string, error)        { return "", syscall.ENOTSUP }
func (f *S3) Symlink(string, string) error           { return syscall.ENOTSUP }
func (f *S3) Chroot(name string) (billy.Filesystem, error) {
	i, err := f.Stat(name)
	if err != nil {
		return nil, err
	}
	if !i.IsDir() {
		return nil, syscall.ENOTDIR
	}
	p, _ := s3Path(name)
	prefix := f.key(p)
	if !strings.HasSuffix(prefix, "/") && prefix != "" {
		prefix += "/"
	}
	return &S3{state: f.state, prefix: prefix}, nil
}
func (f *S3) parent(name string) error {
	i, err := f.stat(path.Dir(name))
	if err != nil {
		return err
	}
	if !i.IsDir() {
		return syscall.ENOTDIR
	}
	return nil
}
func (f *S3) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_CREATE|os.O_RDWR, 0666)
}
func (f *S3) Open(name string) (billy.File, error) { return f.OpenFile(name, os.O_RDONLY, 0) }
func (f *S3) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	p, err := s3Path(name)
	if err != nil {
		return nil, err
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	i, err := f.stat(p)
	if err == nil {
		if i.IsDir() {
			return nil, syscall.EISDIR
		}
		if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
			return nil, os.ErrExist
		}
	} else if errors.Is(err, os.ErrNotExist) && flag&os.O_CREATE != 0 {
		if err = f.parent(p); err != nil {
			return nil, err
		}
		err = f.put(f.key(p), nil, metadata(perm))
		if err == nil {
			f.touchParent(p)
		}
		f.record("create", p, err)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if flag&os.O_TRUNC != 0 {
		if !writable {
			return nil, os.ErrPermission
		}
		err = f.mutate(p, func(_ []byte, m map[string]string) ([]byte, error) {
			m["nfs-mtime"] = time.Now().UTC().Format(time.RFC3339Nano)
			return nil, nil
		})
		f.record("truncate", p, err)
		if err != nil {
			return nil, err
		}
	}
	f.record("open", p, nil)
	return &s3File{fs: f, name: p, flag: flag}, nil
}

// load enforces a memory bound even if an external client uploads a huge object.
func (f *S3) load(key string) ([]byte, map[string]string, error) {
	ctx, cancel := f.request()
	defer cancel()
	out, err := f.state.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.state.cfg.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, nil, s3Error(err)
	}
	defer out.Body.Close()
	if aws.ToInt64(out.ContentLength) > f.state.cfg.MaxFileSize {
		return nil, nil, syscall.EFBIG
	}
	data, err := io.ReadAll(io.LimitReader(out.Body, f.state.cfg.MaxFileSize+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > f.state.cfg.MaxFileSize {
		return nil, nil, syscall.EFBIG
	}
	m := out.Metadata
	if m == nil {
		m = metadata(0644)
	}
	return data, m, nil
}
func (f *S3) mutate(name string, fn func([]byte, map[string]string) ([]byte, error)) error {
	data, m, err := f.load(f.key(name))
	if err != nil {
		return err
	}
	data, err = fn(data, m)
	if err != nil {
		return err
	}
	if int64(len(data)) > f.state.cfg.MaxFileSize {
		return syscall.EFBIG
	}
	return f.put(f.key(name), data, m)
}
func (f *S3) MkdirAll(name string, perm os.FileMode) error {
	p, err := s3Path(name)
	if err != nil {
		return err
	}
	if p == "." {
		return nil
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	current := ""
	for _, part := range strings.Split(p, "/") {
		current = path.Join(current, part)
		i, e := f.stat(current)
		if e == nil {
			if !i.IsDir() {
				return syscall.ENOTDIR
			}
			continue
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if err = f.put(f.key(current)+"/", nil, metadata(perm)); err != nil {
			f.record("mkdir", current, err)
			return err
		}
		f.touchParent(current)
		f.record("mkdir", current, nil)
	}
	return nil
}
func (f *S3) Remove(name string) error {
	p, err := s3Path(name)
	if err != nil {
		return err
	}
	if p == "." {
		return os.ErrPermission
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	i, err := f.stat(p)
	if err != nil {
		return err
	}
	key := f.key(p)
	if i.IsDir() {
		out, e := f.list(key+"/", "", nil, 2)
		if e != nil {
			return e
		}
		for _, o := range out.Contents {
			if aws.ToString(o.Key) != key+"/" {
				return syscall.ENOTEMPTY
			}
		}
		key += "/"
	}
	err = f.delete(key)
	if err == nil {
		f.touchParent(p)
	}
	f.record("remove", p, err)
	return err
}
func (f *S3) Rename(from, to string) error {
	a, err := s3Path(from)
	if err != nil {
		return err
	}
	b, err := s3Path(to)
	if err != nil {
		return err
	}
	if a == "." || b == "." {
		return os.ErrPermission
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	i, err := f.stat(a)
	if err != nil {
		return err
	}
	if a == b {
		return nil
	}
	if i.IsDir() {
		return syscall.ENOTSUP
	}
	if err = f.parent(b); err != nil {
		return err
	}
	dst, err := f.stat(b)
	if err == nil && dst.IsDir() {
		return syscall.EISDIR
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, m, err := f.load(f.key(a))
	if err != nil {
		return err
	}
	if err = f.put(f.key(b), data, m); err == nil {
		f.touchParent(b)
		err = f.delete(f.key(a))
		if err == nil {
			f.touchParent(a)
		}
	}
	f.record("rename", a+" → "+b, err)
	return err
}
func (f *S3) ReadDir(name string) ([]os.FileInfo, error) {
	p, err := s3Path(name)
	if err != nil {
		return nil, err
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	return f.readDir(p, true)
}
func (f *S3) readDir(name string, detailed bool) ([]os.FileInfo, error) {
	i, err := f.stat(name)
	if err != nil {
		return nil, err
	}
	if !i.IsDir() {
		return nil, syscall.ENOTDIR
	}
	prefix := f.key(name)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	items := []os.FileInfo{}
	var token *string
	for {
		out, e := f.list(prefix, "/", token, 1000)
		if e != nil {
			return nil, e
		}
		for _, o := range out.Contents {
			key := aws.ToString(o.Key)
			if key == prefix {
				continue
			}
			relative := strings.TrimPrefix(key, f.prefix)
			// Keys which cannot be represented safely as filesystem paths are hidden.
			if p, e := s3Path(relative); e != nil || p != relative || strings.Contains(relative, "\\") {
				continue
			}
			h := &s3.HeadObjectOutput{ContentLength: o.Size, LastModified: o.LastModified}
			if detailed {
				h, e = f.head(key)
				if e != nil {
					return nil, e
				}
			}
			items = append(items, f.info(relative, h, false))
		}
		for _, d := range out.CommonPrefixes {
			relative := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(d.Prefix), f.prefix), "/")
			if p, e := s3Path(relative); e != nil || p != relative || strings.Contains(relative, "\\") {
				continue
			}
			var h *s3.HeadObjectOutput
			if detailed {
				h, e = f.head(f.key(relative) + "/")
				if e != nil && !errors.Is(e, os.ErrNotExist) {
					return nil, e
				}
			}
			items = append(items, f.info(relative, h, true))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		if out.NextContinuationToken == nil {
			return nil, errors.New("S3 listing missing continuation token")
		}
		token = out.NextContinuationToken
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name() < items[j].Name() })
	return items, nil
}
func (f *S3) ListPage(name string, page, limit int) ([]os.FileInfo, bool, error) {
	if page < 0 || page > 10000 || limit < 1 || limit > 200 {
		return nil, false, os.ErrInvalid
	}
	p, err := s3Path(name)
	if err != nil {
		return nil, false, err
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	items, err := f.readDir(p, false)
	if err != nil {
		return nil, false, err
	}
	start := page * limit
	if start >= len(items) {
		return nil, false, nil
	}
	end := start + limit
	more := end < len(items)
	if end > len(items) {
		end = len(items)
	}
	selected := items[start:end]
	for i, item := range selected {
		selected[i], err = f.stat(path.Join(p, item.Name()))
		if err != nil {
			return nil, false, err
		}
	}
	return selected, more, nil
}
func (f *S3) TempFile(dir, prefix string) (billy.File, error) {
	if strings.ContainsAny(prefix, "/\\") {
		return nil, os.ErrInvalid
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	return f.OpenFile(path.Join(dir, prefix+hex.EncodeToString(random[:])), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}
func (f *S3) change(name, op string, fn func(map[string]string)) error {
	p, err := s3Path(name)
	if err != nil {
		return err
	}
	if p == "." {
		return syscall.ENOTSUP
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	i, err := f.stat(p)
	if err != nil {
		return err
	}
	if i.IsDir() {
		key := f.key(p) + "/"
		h, e := f.head(key)
		m := metadata(i.Mode())
		if e == nil {
			m = h.Metadata
			if m == nil {
				m = metadata(i.Mode())
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		fn(m)
		err = f.put(key, nil, m)
	} else {
		err = f.mutate(p, func(data []byte, m map[string]string) ([]byte, error) { fn(m); return data, nil })
	}
	f.record(op, p, err)
	return err
}
func (f *S3) Chmod(name string, mode os.FileMode) error {
	return f.change(name, "chmod", func(m map[string]string) { m["nfs-mode"] = strconv.FormatUint(uint64(mode.Perm()), 8) })
}
func (f *S3) Chown(name string, uid, gid int) error {
	return f.change(name, "chown", func(m map[string]string) {
		if uid >= 0 {
			m["nfs-uid"] = strconv.Itoa(uid)
		}
		if gid >= 0 {
			m["nfs-gid"] = strconv.Itoa(gid)
		}
	})
}
func (f *S3) Lchown(name string, uid, gid int) error { return f.Chown(name, uid, gid) }
func (f *S3) Chtimes(name string, atime, mtime time.Time) error {
	return f.change(name, "chtimes", func(m map[string]string) { m["nfs-mtime"] = mtime.UTC().Format(time.RFC3339Nano) })
}
