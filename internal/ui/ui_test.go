package ui

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/martinmares/nfs-gate/internal/activity"
	"github.com/martinmares/nfs-gate/internal/backend"
)

func TestFilesEscapeAndNoDownload(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "<script>.txt"), []byte("secret contents"), 0600); err != nil {
		t.Fatal(err)
	}
	events := activity.New(10)
	fs, err := backend.Open(root, events)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	ready := &atomic.Bool{}
	ready.Store(true)
	handler := New(fs, events, root, "test", ready)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/ui/files", nil))
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "<script>.txt") || !strings.Contains(response.Body.String(), "&lt;script&gt;.txt") {
		t.Fatal("filename was not escaped")
	}
	if strings.Contains(response.Body.String(), "secret contents") {
		t.Fatal("file contents leaked")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/ui/files?path=../", nil))
	if response.Code != 404 {
		t.Fatalf("traversal status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/ui/files/download", nil))
	if response.Code == 200 {
		t.Fatal("unexpected download route")
	}
}

func TestDirectoryPagination(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 101; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	events := activity.New(1)
	fs, err := backend.Open(root, events)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	ready := &atomic.Bool{}
	handler := New(fs, events, root, "test", ready)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest("GET", "/ui/files", nil))
	if first.Code != 200 || !strings.Contains(first.Body.String(), "Next") {
		t.Fatalf("first page: %d", first.Code)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest("GET", "/ui/files?page=1", nil))
	if second.Code != 200 || !strings.Contains(second.Body.String(), "Previous") || strings.Count(second.Body.String(), "<td>File</td>") != 1 {
		t.Fatalf("second page: %d", second.Code)
	}
}
