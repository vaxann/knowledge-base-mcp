// Package app assembles a multi-user server from its parts: state store,
// repositories, email sign-in, the authorization server, the MCP tools and
// the admin panel. The command wires it to a listener; tests use it whole.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/admin"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/mail"
	"github.com/vaxann/knowledge-base-mcp/internal/mcpserver"
	"github.com/vaxann/knowledge-base-mcp/internal/oauth"
	"github.com/vaxann/knowledge-base-mcp/internal/signin"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

// Options configure a multi-user server.
type Options struct {
	Config  config.Config
	Log     *slog.Logger
	Version string
	// Sender delivers sign-in emails; the mail provider from the
	// configuration is used when nil.
	Sender mail.Sender
	// FailureDelay overrides how long a wrong code is held back (tests).
	FailureDelay time.Duration
	// AllowLocalRemotes lets tests register local bare repositories.
	AllowLocalRemotes bool
	// Now overrides the clock of admin sessions (tests).
	Now func() time.Time
}

// Multi is a multi-user server ready to be served.
type Multi struct {
	Store     *store.Store
	Workspace *workspace.Workspace
	SignIn    *signin.Flow
	OAuth     *oauth.Server
	Server    *mcpserver.Server
	Admin     *admin.Panel
	HTTP      mcpserver.HTTPOptions
}

// StatePath returns where a configuration keeps its state file.
func StatePath(cfg config.Config) string {
	return filepath.Join(cfg.DataDirOrDefault(), "state.json")
}

// classicOAuthState is where a classic server kept its registered clients.
func classicOAuthState(cfg config.Config) string {
	if p := cfg.Server.HTTP.OAuthState; p != "" {
		return p
	}
	return filepath.Join(cfg.Search.IndexDir, "oauth-state.json")
}

// NewMulti opens the state, makes sure the administrator account exists,
// brings the repositories up and wires sign-in and tools together. ctx bounds
// the background work of the repositories.
func NewMulti(ctx context.Context, o Options) (*Multi, error) {
	cfg := o.Config
	if !cfg.MultiUser() {
		return nil, fmt.Errorf("multi-user mode is not configured")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	st, err := store.Open(StatePath(cfg))
	if err != nil {
		return nil, err
	}
	if err := st.Update(func(s *store.State) error {
		_, err := s.EnsureAdmin(cfg.Server.Accounts.AdminEmail, time.Now())
		return err
	}); err != nil {
		return nil, err
	}
	if err := oauth.ImportClients(classicOAuthState(cfg), st); err != nil {
		return nil, fmt.Errorf("import classic OAuth clients: %w", err)
	}
	ws, err := workspace.NewMulti(ctx, workspace.Options{Config: cfg, Store: st, Log: o.Log, Version: o.Version, AllowLocalRemotes: o.AllowLocalRemotes})
	if err != nil {
		return nil, err
	}
	sender := o.Sender
	if sender == nil {
		sender = &mail.Resend{APIKey: cfg.Server.Accounts.ResendAPIKey, From: cfg.Server.Accounts.MailFrom, UserAgent: "knowledge-base-mcp/" + o.Version}
	}
	flow := signin.NewFlow(st, sender, o.Log)
	h := cfg.Server.HTTP
	oa, err := oauth.New(oauth.Config{PublicURL: h.PublicURL, StaticToken: h.Token, Storage: oauth.StoreStorage{Store: st},
		SignIn: flow, Accounts: st, FailureDelay: o.FailureDelay, Logger: o.Log})
	if err != nil {
		_ = ws.Close()
		return nil, err
	}
	if h.OAuthPassword != "" {
		o.Log.Warn("KB_OAUTH_PASSWORD is ignored in multi-user mode: accounts sign in with a code sent by email")
	}
	m := &Multi{Store: st, Workspace: ws, SignIn: flow, OAuth: oa, Server: mcpserver.NewWorkspace(ws, o.Version, o.Log)}
	m.HTTP = mcpserver.HTTPOptions{Listen: h.Listen, Token: h.Token, TLSCert: h.TLSCert, TLSKey: h.TLSKey, PublicURL: h.PublicURL, OAuth: oa}
	if m.HTTP.PublicURL == "" && mcpserver.IsLoopback(h.Listen) {
		m.HTTP.PublicURL = "http://" + h.Listen
	}
	// The session cookie is HTTPS-only unless the panel can only be reached
	// over plain HTTP on this machine.
	plainLoopback := mcpserver.IsLoopback(h.Listen) && h.TLSCert == "" && !strings.HasPrefix(h.PublicURL, "https://")
	m.Admin = admin.New(admin.Config{Store: st, Workspace: ws, OAuth: oa, SignIn: flow, StaticToken: h.Token,
		Secure: !plainLoopback, FailureDelay: o.FailureDelay, Log: o.Log, Now: o.Now})
	m.HTTP.Mount = m.Admin.Mount
	return m, nil
}

// Handler returns the HTTP handler of the whole server.
func (m *Multi) Handler() http.Handler { return m.Server.HandlerWith(m.HTTP) }

// Run serves until ctx is done.
func (m *Multi) Run(ctx context.Context) error { return m.Server.RunHTTP(ctx, m.HTTP) }

// Close stops the repositories and waits for pending sign-in emails.
func (m *Multi) Close() error {
	m.SignIn.Wait()
	return m.Workspace.Close()
}
