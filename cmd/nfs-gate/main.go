package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/martinmares/nfs-gate/internal/server"
)

var version = "dev"

func main() {
	cfg := server.Config{}
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
