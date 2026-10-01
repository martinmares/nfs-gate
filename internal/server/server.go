package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/martinmares/nfs-gate/internal/activity"
	"github.com/martinmares/nfs-gate/internal/backend"
	"github.com/martinmares/nfs-gate/internal/ui"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers"
)

type Config struct {
	Backend                string
	S3                     backend.S3Config
	Listen                 string
	Root                   string
	HealthListen           string
	UIListen               string
	AllowUnauthenticatedUI bool
	Version                string
	Logger                 *slog.Logger
}

func ValidateUIBind(addr string, allow bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if allow {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("unauthenticated UI listener %q must use loopback or --allow-unauthenticated-ui", addr)
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := ValidateUIBind(cfg.UIListen, cfg.AllowUnauthenticatedUI); err != nil {
		return err
	}
	events := activity.New(500)
	var fs backend.Filesystem
	var err error
	switch cfg.Backend {
	case "", "local":
		if cfg.S3.Bucket != "" || cfg.S3.Prefix != "" || cfg.S3.Endpoint != "" {
			return fmt.Errorf("S3 settings require --backend=s3")
		}
		fs, err = backend.Open(cfg.Root, events)
	case "s3":
		fs, err = backend.OpenS3(ctx, cfg.S3, events)
	default:
		return fmt.Errorf("unknown backend %q; use local or s3", cfg.Backend)
	}
	if err != nil {
		return err
	}
	defer fs.Close()
	if err := fs.Check(ctx); err != nil {
		return fmt.Errorf("export unavailable: %w", err)
	}
	nfsListener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("NFS listener: %w", err)
	}
	tracked := newTrackingListener(nfsListener)
	defer tracked.Close()
	healthListener, err := net.Listen("tcp", cfg.HealthListen)
	if err != nil {
		return fmt.Errorf("health listener: %w", err)
	}
	defer healthListener.Close()
	uiListener, err := net.Listen("tcp", cfg.UIListen)
	if err != nil {
		return fmt.Errorf("UI listener: %w", err)
	}
	defer uiListener.Close()

	var ready atomic.Bool
	ready.Store(true)
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "OK") })
	healthMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "NFS listener unavailable", http.StatusServiceUnavailable)
			return
		}
		probeCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := fs.Check(probeCtx); err != nil {
			http.Error(w, "export backend unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "OK")
	})
	healthServer := &http.Server{Handler: healthMux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	uiServer := &http.Server{Handler: ui.New(fs, events, fs.Root(), cfg.Version, &ready), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	handler := helpers.NewCachingHandler(helpers.NewNullAuthHandler(fs), 65536)
	nfsServer := &nfs.Server{Handler: handler, Context: ctx}
	errs := make(chan error, 3)
	go func() { errs <- nfsServer.Serve(tracked) }()
	go func() { errs <- healthServer.Serve(healthListener) }()
	go func() { errs <- uiServer.Serve(uiListener) }()
	cfg.Logger.Info("nfs-gate starting", "root", fs.Root(), "backend", fs.Kind())
	cfg.Logger.Info("nfs listener active", "address", nfsListener.Addr().String())
	cfg.Logger.Info("health listener active", "address", healthListener.Addr().String())
	cfg.Logger.Info("UI listener active", "address", uiListener.Addr().String())
	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) && !strings.Contains(err.Error(), "use of closed network connection") {
			ready.Store(false)
			return err
		}
	}
	ready.Store(false)
	tracked.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = healthServer.Shutdown(shutdownCtx)
	_ = uiServer.Shutdown(shutdownCtx)
	cfg.Logger.Info("nfs-gate stopped")
	return nil
}

type trackingListener struct {
	net.Listener
	mu     sync.Mutex
	conns  map[*trackingConn]struct{}
	closed bool
}
type trackingConn struct {
	net.Conn
	owner *trackingListener
	once  sync.Once
}

func newTrackingListener(l net.Listener) *trackingListener {
	return &trackingListener{Listener: l, conns: map[*trackingConn]struct{}{}}
}
func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		c.Close()
		return nil, net.ErrClosed
	}
	t := &trackingConn{Conn: c, owner: l}
	l.conns[t] = struct{}{}
	return t, nil
}
func (l *trackingListener) Close() error {
	l.mu.Lock()
	l.closed = true
	for c := range l.conns {
		c.Conn.Close()
	}
	l.conns = map[*trackingConn]struct{}{}
	l.mu.Unlock()
	return l.Listener.Close()
}
func (c *trackingConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.mu.Lock(); delete(c.owner.conns, c); c.owner.mu.Unlock() })
	return err
}
