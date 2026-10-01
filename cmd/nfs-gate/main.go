package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/martinmares/nfs-gate/internal/backend"
	"github.com/martinmares/nfs-gate/internal/server"
)

var version = "dev"

func main() {
	cfg := server.Config{}
	flag.StringVar(&cfg.Backend, "backend", "local", "storage backend: local or s3")
	flag.StringVar(&cfg.S3.Bucket, "s3-bucket", "", "S3 bucket (requires --backend=s3)")
	flag.StringVar(&cfg.S3.Prefix, "s3-prefix", "", "S3 object prefix to export")
	flag.StringVar(&cfg.S3.Region, "s3-region", "", "S3 region (defaults to AWS configuration)")
	flag.StringVar(&cfg.S3.Endpoint, "s3-endpoint", "", "optional S3-compatible HTTP(S) endpoint")
	flag.BoolVar(&cfg.S3.PathStyle, "s3-path-style", false, "use path-style S3 bucket addressing")
	flag.Int64Var(&cfg.S3.MaxFileSize, "s3-max-file-size", backend.DefaultS3MaxFileSize, "maximum writable S3 file size in bytes")
	flag.DurationVar(&cfg.S3.Timeout, "s3-timeout", 30*time.Second, "timeout per S3 API request")
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:12049", "NFS TCP listen address")
	flag.StringVar(&cfg.Root, "root", "/data", "POSIX directory to export")
	flag.StringVar(&cfg.HealthListen, "health-listen", ":8080", "health HTTP listen address")
	flag.StringVar(&cfg.UIListen, "ui-listen", "127.0.0.1:8081", "read-only UI HTTP listen address")
	flag.BoolVar(&cfg.AllowUnauthenticatedUI, "allow-unauthenticated-ui", false, "allow UI to listen outside loopback without authentication")
	debug := flag.Bool("debug", false, "enable debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("nfs-gate", version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	cfg.Version = version
	cfg.Logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := server.Run(ctx, cfg); err != nil {
		cfg.Logger.Error("nfs-gate failed", "error", err)
		os.Exit(1)
	}
}
