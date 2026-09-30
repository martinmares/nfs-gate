package server_test

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/martinmares/nfs-gate/internal/activity"
	"github.com/martinmares/nfs-gate/internal/backend"
	nfsserver "github.com/willscott/go-nfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs/helpers"
)

func TestTwoClientsSharePOSIXDirectory(t *testing.T) {
	root := t.TempDir()
	fs, err := backend.Open(root, activity.New(100))
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = nfsserver.Serve(listener, helpers.NewCachingHandler(helpers.NewNullAuthHandler(fs), 1000)) }()
	client := func() *nfsclient.Target {
		t.Helper()
		rpcClient, err := rpc.DialTCP("tcp", listener.Addr().String(), false)
		if err != nil {
			t.Fatal(err)
		}
		mount := &nfsclient.Mount{Client: rpcClient}
		target, err := mount.Mount("/", rpc.AuthNull)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { target.Close() })
		return target
	}
	a, b := client(), client()
	file, err := a.OpenFile("foo", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create("foo", 0644); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "foo"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("backend content: %q, %v", content, err)
	}
	other, err := b.Open("foo")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(other, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("client B read %q", buf)
	}
	if _, err := b.Mkdir("dir", 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := a.Rename("foo", "dir/bar"); err != nil {
		t.Fatal(err)
	}
	if err := b.Setattr("dir/bar", nfsclient.Sattr3{Size: nfsclient.SetSize{SetIt: true, Size: 2}}); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(root, "dir/bar"))
	if err != nil || string(content) != "he" {
		t.Fatalf("truncated content: %q, %v", content, err)
	}
	if err := os.WriteFile(filepath.Join(root, "world"), []byte("from backend"), 0644); err != nil {
		t.Fatal(err)
	}
	world, err := a.Open("world")
	if err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 12)
	if _, err := io.ReadFull(world, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "from backend" {
		t.Fatalf("client read %q", buf)
	}
	if err := b.Remove("dir/bar"); err != nil {
		t.Fatal(err)
	}
	if err := a.RmDir("dir"); err != nil {
		t.Fatal(err)
	}
}
