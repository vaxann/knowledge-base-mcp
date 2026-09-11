// Command knowledge-base-mcp serves a Git-backed Markdown knowledge base over
// the Model Context Protocol on stdio.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/mcpserver"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	var (
		cfgPath     = flag.String("config", "", "path to config.yaml (or set KB_CONFIG)")
		showVersion = flag.Bool("version", false, "print version and exit")
		check       = flag.Bool("check", false, "validate configuration and vault, then exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: knowledge-base-mcp [flags]\n\nServes MCP on stdio, or over streamable HTTP when KB_HTTP_LISTEN is set.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	cfg, err := config.Load(*cfgPath, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	log := newLogger(cfg.Server.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc, err := kb.Open(ctx, cfg, log, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup error:", err)
		return 1
	}
	defer func() { _ = svc.Close() }()
	if *check {
		info := svc.GetInfo(ctx)
		fmt.Fprintf(os.Stderr, "ok: %d notes on branch %s (%s)\n", info.NoteCount, info.Branch, info.SyncState)
		return 0
	}
	svc.Start(ctx)
	srv := mcpserver.New(svc, version, log)
	log.Info("starting", "version", version, "vault", cfg.Vault.Path, "read_only", cfg.Server.ReadOnly)
	var runErr error
	if h := cfg.Server.HTTP; h.Listen != "" {
		runErr = srv.RunHTTP(ctx, mcpserver.HTTPOptions{Listen: h.Listen, Token: h.Token, TLSCert: h.TLSCert, TLSKey: h.TLSKey})
	} else {
		runErr = srv.Run(ctx)
	}
	if err := runErr; err != nil && ctx.Err() == nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	// Logs go to stderr only: stdout carries the MCP protocol.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
