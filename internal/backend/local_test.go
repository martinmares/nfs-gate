package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/martinmares/nfs-gate/internal/activity"
)

func TestConfinementAndActivity(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "export")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret"), []byte("hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	events := activity.New(5)
	fs, err := Open(root, events)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if _, err := fs.Open("../secret"); err == nil {
		t.Fatal("traversal escaped export")
	}
	if _, err := fs.Open("escape"); err == nil {
		t.Fatal("symlink escaped export")
	}
	if _, _, err := fs.ListPage("escape", 0, 100); err == nil {
		t.Fatal("UI followed symlink")
	}
	file, err := fs.Create("hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "hello"))
	if err != nil || string(content) != "world" {
		t.Fatalf("backend content: %q %v", content, err)
	}
	items, more, err := fs.ListPage(".", 0, 1)
	if err != nil || len(items) != 1 || !more {
		t.Fatalf("page: %v %v %v", items, more, err)
	}
	history, counts, _ := events.Snapshot()
	if len(history) > 5 || counts["write"] != 1 {
		t.Fatalf("activity: %#v %#v", history, counts)
	}
}
