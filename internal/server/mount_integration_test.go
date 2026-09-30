//go:build integration && linux

package server_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLinuxKernelMount(t *testing.T) {
	if os.Getenv("NFS_GATE_INTEGRATION") != "1" {
		t.Skip("set NFS_GATE_INTEGRATION=1 to run privileged mount test")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root and CAP_SYS_ADMIN")
	}
	if _, err := exec.LookPath("mount.nfs"); err != nil {
		t.Skip("mount.nfs unavailable")
	}
	root, clientA, clientB := t.TempDir(), t.TempDir(), t.TempDir()
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	portListener.Close()
	bin := filepath.Join(t.TempDir(), "nfs-gate")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/nfs-gate")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	server := exec.Command(bin, "--root", root, "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--health-listen", "127.0.0.1:0", "--ui-listen", "127.0.0.1:0")
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if i == 49 {
			t.Fatal("server did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	mount := func(path string) {
		t.Helper()
		options := fmt.Sprintf("nfsvers=3,proto=tcp,port=%d,mountvers=3,mountport=%d,mountproto=tcp,nolock", port, port)
		cmd := exec.Command("mount", "-t", "nfs", "-o", options, "127.0.0.1:/", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mount: %v: %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("umount", path).Run() })
	}
	mount(clientA)
	mount(clientB)
	if err := os.WriteFile(filepath.Join(clientA, "foo"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(clientB, "foo")); err != nil || string(got) != "hello" {
		t.Fatalf("B read: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "bar"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(clientA, "bar")); err != nil || string(got) != "world" {
		t.Fatalf("A read: %q %v", got, err)
	}
	if err := os.Mkdir(filepath.Join(clientB, "dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(clientA, "foo"), filepath.Join(clientA, "dir", "foo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(clientB, "dir", "foo"), 2); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "dir", "foo")); err != nil || string(got) != "he" {
		t.Fatalf("truncated backend: %q %v", got, err)
	}
	if err := os.Remove(filepath.Join(clientB, "dir", "foo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(clientA, "dir")); err != nil {
		t.Fatal(err)
	}
}
