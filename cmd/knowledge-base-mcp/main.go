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
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vaxann/knowledge-base-mcp/internal/app"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/mcpserver"
	"github.com/vaxann/knowledge-base-mcp/internal/oauth"
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

	if cfg.Server.Accounts.AdminEmail != "" && !cfg.MultiUser() {
		log.Warn("KB_ADMIN_EMAIL is ignored: multi-user mode needs the HTTP transport (KB_HTTP_LISTEN); serving one vault over stdio")
	}
	if cfg.MultiUser() {
		return runMulti(ctx, cfg, log, *check)
	}

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
		opts := mcpserver.HTTPOptions{Listen: h.Listen, Token: h.Token, TLSCert: h.TLSCert, TLSKey: h.TLSKey, PublicURL: h.PublicURL}
		if opts.PublicURL == "" && mcpserver.IsLoopback(h.Listen) {
			opts.PublicURL = "http://" + h.Listen
		}
		if pw := firstNonEmpty(h.OAuthPassword, h.Token); pw != "" {
			statePath := h.OAuthState
			if statePath == "" {
				statePath = filepath.Join(cfg.Search.IndexDir, "oauth-state.json")
			}
			opts.OAuth, err = oauth.New(oauth.Config{PublicURL: h.PublicURL, Password: pw, StaticToken: h.Token, StatePath: statePath, Logger: log})
			if err != nil {
				fmt.Fprintln(os.Stderr, "startup error:", err)
				return 1
			}
			log.Info("OAuth sign-in enabled", "public_url", h.PublicURL, "state", statePath)
		}
		runErr = srv.RunHTTP(ctx, opts)
	} else {
		runErr = srv.Run(ctx)
	}
	if err := runErr; err != nil && ctx.Err() == nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

// runMulti serves the multi-user server: accounts, several repositories and
// access rules behind the HTTP transport.
func runMulti(ctx context.Context, cfg config.Config, log *slog.Logger, check bool) int {
	m, err := app.NewMulti(ctx, app.Options{Config: cfg, Log: log, Version: version})
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup error:", err)
		return 1
	}
	defer func() { _ = m.Close() }()
	if check {
		if err := m.Workspace.WaitReady(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			return 1
		}
		repos := m.Workspace.Repos(ctx)
		fmt.Fprintf(os.Stderr, "ok: multi-user mode, %d repositories\n", len(repos))
		for _, r := range repos {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", r.Alias, r.Status)
		}
		return 0
	}
	log.Info("starting", "version", version, "mode", "multi-user", "data_dir", cfg.DataDirOrDefault(), "read_only", cfg.Server.ReadOnly)
	if err := m.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
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
