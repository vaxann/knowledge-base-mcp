// Package workspace is what the MCP tools talk to. For a classic server it
// is a thin pass-through to one vault. For a multi-user server it holds the
// registry of repositories, presents them as one tree whose first path
// segment is the repository alias, and gives every call the scope of the
// account that makes it.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/secret"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// Caller is the account a request acts as.
type Caller struct {
	AccountID string
	Email     string
	Admin     bool
}

type callerKey struct{}

// WithCaller attaches the acting account to a context.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerOf returns the acting account of a context.
func CallerOf(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Repository statuses reported to the administrator.
const (
	StatusKeyPending        = "key_pending"
	StatusCloning           = "cloning"
	StatusOK                = "ok"
	StatusOffline           = "offline"
	StatusConflict          = "conflict"
	StatusError             = "error"
	StatusCredentialInvalid = "credential_invalid" //nolint:gosec // a status name, not a credential
)

// mount is the runtime side of one registered repository.
type mount struct {
	id    string
	alias string
	// svc is set once the clone is open; until then the repository serves
	// nothing.
	svc    *kb.Service
	status string // key_pending, cloning, error or credential_invalid; "" while svc decides
	reason string
	// badCredential marks a repository that runs on its last clone because
	// its stored credential cannot be decrypted.
	badCredential bool
	keyFile       string
	done          chan struct{} // closed when the current start attempt ends
	cancel        context.CancelFunc
}

// Options configure a multi-user workspace.
type Options struct {
	Config  config.Config
	Store   *store.Store
	Log     *slog.Logger
	Version string
	// AllowLocalRemotes accepts absolute paths as repository URLs. Tests
	// use it to work against local bare repositories; never set it in a server.
	AllowLocalRemotes bool
}

// Workspace routes tool calls to repositories.
type Workspace struct {
	log     *slog.Logger
	version string
	cfg     config.Config
	links   *linkSigner

	flat *kb.Service // classic mode: the one vault

	// multi-user mode
	store      *store.Store
	box        *secret.Box
	dataDir    string
	keyDir     string
	knownHosts string
	strictHost bool
	allowLocal bool
	lock       *kb.DirLock
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	mu     sync.RWMutex
	mounts map[string]*mount // repository id -> runtime
}

// NewClassic wraps the single vault of a classic server.
func NewClassic(svc *kb.Service, log *slog.Logger) *Workspace {
	if log == nil {
		log = slog.Default()
	}
	return &Workspace{flat: svc, log: log, links: newLinkSigner(filepath.Join(svc.IndexDir(), "link-secret"))}
}

// NewMulti opens the registry of a multi-user server: it imports the
// configured vault on the very first start and brings every registered
// repository up in the background. ctx bounds the background work.
func NewMulti(ctx context.Context, o Options) (*Workspace, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	cfg := o.Config
	dataDir := cfg.DataDirOrDefault()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	lock, err := kb.LockDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	box, err := secret.New(cfg.SecretKeyBytes())
	if err != nil {
		lock.Release()
		return nil, err
	}
	// Decrypted SSH keys live outside the data volume so that a backup of
	// the volume never contains them. The location is fixed per data
	// directory, which lets a start after a crash clean up what was left.
	sum := sha256.Sum256([]byte(dataDir))
	keyDir := filepath.Join(os.TempDir(), "kb-keys-"+hex.EncodeToString(sum[:8]))
	_ = os.RemoveAll(keyDir)
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		lock.Release()
		return nil, err
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &Workspace{log: o.Log, version: o.Version, cfg: cfg, store: o.Store, box: box, dataDir: dataDir, keyDir: keyDir,
		knownHosts: filepath.Join(dataDir, "known_hosts"), allowLocal: o.AllowLocalRemotes, lock: lock,
		ctx: wctx, cancel: cancel, mounts: map[string]*mount{}, links: newLinkSigner(filepath.Join(dataDir, "link-secret"))}
	if hosts := strings.TrimSpace(cfg.Git.SSHKnownHosts); hosts != "" {
		// Configured host keys are the only ones trusted.
		if err := os.WriteFile(w.knownHosts, []byte(hosts+"\n"), 0o600); err != nil {
			_ = w.Close()
			return nil, err
		}
		w.strictHost = true
	}
	if err := w.importConfiguredVault(); err != nil {
		_ = w.Close()
		return nil, err
	}
	for _, r := range o.Store.Snapshot().RepoList() {
		w.start(r)
	}
	return w, nil
}

// Close stops every repository and removes decrypted keys.
func (w *Workspace) Close() error {
	if w.flat != nil {
		return w.flat.Close()
	}
	w.cancel()
	w.wg.Wait()
	w.mu.Lock()
	mounts := w.mounts
	w.mounts = map[string]*mount{}
	w.mu.Unlock()
	var first error
	for _, m := range mounts {
		if m.svc != nil {
			if err := m.svc.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	_ = os.RemoveAll(w.keyDir)
	w.lock.Release()
	return first
}

// Multi reports whether this is a multi-user workspace.
func (w *Workspace) Multi() bool { return w.flat == nil }

// ReadOnly reports whether writes are disabled server-wide.
func (w *Workspace) ReadOnly() bool {
	if w.flat != nil {
		return w.flat.ReadOnly()
	}
	return w.cfg.Server.ReadOnly
}

// MaxUploadBytes returns the configured upload limit.
func (w *Workspace) MaxUploadBytes() int64 {
	if w.flat != nil {
		return w.flat.MaxUploadBytes()
	}
	return w.cfg.Server.MaxUploadBytes()
}

// Store returns the state store (nil for a classic workspace).
func (w *Workspace) Store() *store.Store { return w.store }

// Caller builds the acting account for an account id, or false when the
// account does not exist or is disabled.
func (w *Workspace) Caller(accountID string) (Caller, bool) {
	if w.store == nil {
		return Caller{}, false
	}
	a := w.store.Snapshot().Accounts[accountID]
	if a == nil || a.Disabled {
		return Caller{}, false
	}
	return Caller{AccountID: a.ID, Email: a.Email, Admin: a.Admin}, true
}

// ---- routing ----

func notFound(p string) *kb.Error { return kb.E(kb.CodeNotFound, "not_found: %s", p) }

// target is a repository together with the context a call into it must use.
type target struct {
	svc   *kb.Service
	ctx   context.Context
	alias string
	id    string
}

// activeCaller returns the caller if its account still exists and is active.
func (w *Workspace) activeCaller(ctx context.Context, st *store.State) (Caller, bool) {
	c, ok := CallerOf(ctx)
	if !ok {
		return Caller{}, false
	}
	a := st.Accounts[c.AccountID]
	if a == nil || a.Disabled {
		return Caller{}, false
	}
	return Caller{AccountID: a.ID, Email: a.Email, Admin: a.Admin}, true
}

// scoped builds the target for a mounted repository as seen by the caller,
// or false when the repository is not ready or holds nothing for the caller.
func (w *Workspace) scoped(ctx context.Context, st *store.State, c Caller, m mount) (target, bool) {
	if m.svc == nil {
		return target{}, false
	}
	rules := st.RulesFor(c.AccountID, m.id)
	if !rules.Any() {
		return target{}, false
	}
	sctx := kb.WithUser(kb.WithScope(ctx, rules), c.Email)
	return target{svc: m.svc, ctx: sctx, alias: m.alias, id: m.id}, true
}

// mountByAlias returns a snapshot of a mount; mounts change under w.mu.
func (w *Workspace) mountByAlias(alias string) (mount, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, m := range w.mounts {
		if m.alias == alias {
			return *m, true
		}
	}
	return mount{}, false
}

// mountList returns snapshots of all mounts sorted by alias.
func (w *Workspace) mountList() []mount {
	w.mu.RLock()
	out := make([]mount, 0, len(w.mounts))
	for _, m := range w.mounts {
		out = append(out, *m)
	}
	w.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].alias < out[j].alias })
	return out
}

// route finds the repository a path addresses. Anything the caller cannot
// reach (an unknown alias, a repository that is not ready, one in which the
// caller holds no rule) answers not_found for the path, all alike.
func (w *Workspace) route(ctx context.Context, p string) (target, error) {
	if w.flat != nil {
		return target{svc: w.flat, ctx: ctx}, nil
	}
	clean, err := vault.CleanRel(p)
	if err != nil {
		return target{}, kb.E(kb.CodeInvalidPath, "%s", err.Error())
	}
	st := w.store.Snapshot()
	c, ok := w.activeCaller(ctx, st)
	if !ok || clean == "" {
		return target{}, notFound(clean)
	}
	alias, _, _ := strings.Cut(clean, "/")
	m, ok := w.mountByAlias(alias)
	if !ok {
		return target{}, notFound(clean)
	}
	t, ok := w.scoped(ctx, st, c, m)
	if !ok {
		return target{}, notFound(clean)
	}
	return t, nil
}

// routeWrite is route for paths that name a file to change. A path without
// a repository alias in front can only be a repository root, where files
// cannot live.
func (w *Workspace) routeWrite(ctx context.Context, p string) (target, error) {
	if w.flat == nil {
		if clean, err := vault.CleanRel(p); err == nil && !strings.Contains(clean, "/") {
			return target{}, kb.E(kb.CodeInvalidPath, "%q does not name a file inside a repository: start the path with a repository alias, e.g. <alias>/%s", clean, clean)
		}
	}
	return w.route(ctx, p)
}

// reachable returns every repository the caller can read something in,
// sorted by alias.
func (w *Workspace) reachable(ctx context.Context) []target {
	if w.flat != nil {
		return []target{{svc: w.flat, ctx: ctx}}
	}
	st := w.store.Snapshot()
	c, ok := w.activeCaller(ctx, st)
	if !ok {
		return nil
	}
	var out []target
	for _, m := range w.mountList() {
		if t, ok := w.scoped(ctx, st, c, m); ok {
			out = append(out, t)
		}
	}
	return out
}

// one resolves the repository for tools that need exactly one and take it
// from an optional path: with a path it is routed, without one the caller
// must have a single repository to choose from.
func (w *Workspace) one(ctx context.Context, p, what string) (target, error) {
	if w.flat != nil || p != "" {
		return w.route(ctx, p)
	}
	ts := w.reachable(ctx)
	switch len(ts) {
	case 1:
		return ts[0], nil
	case 0:
		return target{}, kb.E(kb.CodeNotFound, "no repository is available to this account")
	}
	aliases := make([]string, len(ts))
	for i, t := range ts {
		aliases[i] = t.alias
	}
	return target{}, kb.E(kb.CodeInvalidArgument, "%s must start with a repository alias; available: %s", what, strings.Join(aliases, ", ")).With("repositories", aliases)
}

// byAlias resolves an optional repository argument of the sync tools: one
// repository when named, every reachable one otherwise.
func (w *Workspace) byAlias(ctx context.Context, alias string) ([]target, error) {
	if alias == "" {
		return w.reachable(ctx), nil
	}
	t, err := w.route(ctx, alias)
	if err != nil {
		return nil, err
	}
	return []target{t}, nil
}

// ---- admin-side reads that ignore scopes ----

var errNoRepo = errors.New("unknown repository")

func (w *Workspace) readyMount(alias string) (mount, error) {
	m, ok := w.mountByAlias(alias)
	if !ok {
		return mount{}, errNoRepo
	}
	if m.svc == nil {
		return mount{}, fmt.Errorf("repository %s is not available yet (%s)", alias, m.status)
	}
	return m, nil
}

// ChildFolders lists the names of the direct sub-folders of a folder in a
// repository. It is for the admin panel: it ignores access rules and never
// returns file names.
func (w *Workspace) ChildFolders(alias, rel string) ([]string, error) {
	m, err := w.readyMount(alias)
	if err != nil {
		return nil, err
	}
	return m.svc.ChildFolders(rel)
}

// FolderExists reports whether a folder exists in a repository. It returns
// true when the repository cannot be inspected, so that rules are not
// reported as orphaned merely because a clone is not ready.
func (w *Workspace) FolderExists(alias, rel string) bool {
	m, err := w.readyMount(alias)
	if err != nil {
		return true
	}
	return m.svc.FolderExists(rel)
}

// RulesOf returns the caller-independent rule set of an account in a
// repository (for the admin panel's effective-level column).
func RulesOf(st *store.State, accountID, repoID string) acl.Set {
	return st.RulesFor(accountID, repoID)
}
