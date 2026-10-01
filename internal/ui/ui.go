package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/martinmares/nfs-gate/internal/activity"
	"github.com/martinmares/nfs-gate/internal/backend"
)

//go:embed static templates
var assets embed.FS

// StaticHandler serves the embedded Tabler, HTMX, and theme assets.
func StaticHandler() http.Handler {
	static, _ := fs.Sub(assets, "static")
	return http.StripPrefix("/static/", http.FileServer(http.FS(static)))
}

type page struct {
	Backend  string
	Title    string
	Active   string
	Version  string
	Root     string
	Uptime   string
	Ready    bool
	Events   []activity.Event
	Counters []counter
	Errors   uint64
	Path     string
	Parent   string
	Files    []fileRow
	Page     int
	More     bool
	PrevURL  string
	NextURL  string
	Error    string
}
type counter struct {
	Name  string
	Count uint64
}
type fileRow struct{ Name, Type, Size, Modified, Link string }
type Handler struct {
	fs            backend.Filesystem
	events        *activity.Store
	root, version string
	ready         *atomic.Bool
	started       time.Time
	tpl           *template.Template
}

func New(filesystem backend.Filesystem, events *activity.Store, root, version string, ready *atomic.Bool) http.Handler {
	h := &Handler{fs: filesystem, events: events, root: root, version: version, ready: ready, started: time.Now()}
	h.tpl = template.Must(template.New("page.html").ParseFS(assets, "templates/*.html"))
	mux := http.NewServeMux()
	mux.Handle("GET /static/", StaticHandler())
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /ui/", h.overview)
	mux.HandleFunc("GET /ui/files", h.files)
	mux.HandleFunc("GET /ui/activity", h.activity)
	mux.HandleFunc("GET /ui/fragments/activity", h.activityFragment)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'sha256-bsV5JivYxvGywDAZ22EZJKBFip65Ng9xoJVLbBg7bdo='; script-src 'self'; font-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}

func (h *Handler) base(title, active string) page {
	events, counts, errors := h.events.Snapshot()
	rows := make([]counter, 0, len(counts))
	for name, count := range counts {
		rows = append(rows, counter{name, count})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if len(events) > 30 {
		events = events[:30]
	}
	return page{Backend: h.fs.Kind(), Title: title, Active: active, Version: h.version, Root: h.root, Uptime: time.Since(h.started).Truncate(time.Second).String(), Ready: h.ready.Load(), Events: events, Counters: rows, Errors: errors}
}
func (h *Handler) render(w http.ResponseWriter, data page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(w, "page.html", data); err != nil {
		http.Error(w, "render failed", 500)
	}
}
func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ui/" {
		http.NotFound(w, r)
		return
	}
	h.render(w, h.base("Overview", "overview"))
}
func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	h.render(w, h.base("Activity", "activity"))
}
func (h *Handler) activityFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.tpl.ExecuteTemplate(w, "activity_table", h.base("Activity", "activity"))
}
func (h *Handler) files(w http.ResponseWriter, r *http.Request) {
	data := h.base("Files", "files")
	name := r.URL.Query().Get("path")
	if name == "" {
		name = "."
	}
	data.Path = name
	if name != "." {
		parent := filepath.Dir(name)
		if parent == "." {
			data.Parent = "/ui/files"
		} else {
			data.Parent = "/ui/files?path=" + url.QueryEscape(parent)
		}
	}
	if raw := r.URL.Query().Get("page"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 10000 {
			http.Error(w, "invalid page", 400)
			return
		}
		data.Page = value
	}
	items, more, err := h.fs.ListPage(name, data.Page, 100)
	if err != nil {
		if os.IsPermission(err) || os.IsNotExist(err) {
			http.Error(w, "directory unavailable", 404)
			return
		}
		data.Error = err.Error()
	} else {
		data.More = more
		if data.Page > 0 {
			data.PrevURL = PathQuery(name, data.Page-1)
		}
		if more {
			data.NextURL = PathQuery(name, data.Page+1)
		}
		for _, info := range items {
			row := fileRow{Name: info.Name(), Size: humanSize(info.Size()), Modified: info.ModTime().Format("2006-01-02 15:04:05")}
			switch {
			case info.IsDir():
				row.Type = "Directory"
				row.Link = "/ui/files?path=" + url.QueryEscape(filepath.Join(name, info.Name()))
			case info.Mode()&os.ModeSymlink != 0:
				row.Type = "Symlink"
			case info.Mode().IsRegular():
				row.Type = "File"
			default:
				row.Type = "Other"
			}
			data.Files = append(data.Files, row)
		}
	}
	h.render(w, data)
}
func humanSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	unit := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(size)
	for _, u := range unit {
		v /= 1024
		if v < 1024 {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", v/1024)
}

// PathQuery constructs a query for paged directory navigation.
func PathQuery(name string, number int) string {
	parts := []string{"page=" + strconv.Itoa(number)}
	if name != "." {
		parts = append(parts, "path="+url.QueryEscape(name))
	}
	return "/ui/files?" + strings.Join(parts, "&")
}
