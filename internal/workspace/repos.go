package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/secret"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
)

// importConfiguredVault registers the vault from the classic settings as the
// first repository, once, on the first start with an empty registry. The
// clone, the index and the Git credentials stay exactly where they are; the
// administrator gets one ordinary rule so the migrated vault is not empty
// for its owner.
func (w *Workspace) importConfiguredVault() error {
	cfg := w.cfg
	st := w.store.Snapshot()
	if st.VaultImported || len(st.Repos) > 0 || cfg.Vault.Path == "" {
		return nil
	}
	// A path that holds no clone and has no remote to clone from is a
	// default of the container image, not a vault somebody configured.
	if !isClone(cfg.Vault.Path) && cfg.Vault.Remote == "" {
		return nil
	}
	return w.store.Update(func(st *store.State) error {
		r := &store.Repo{Alias: cfg.Vault.Alias, URL: cfg.Vault.Remote, Branch: cfg.Git.Branch, Credential: store.CredHost,
			VaultPath: cfg.Vault.Path, IndexDir: cfg.Search.IndexDir, CreatedAt: time.Now()}
		if err := st.AddRepo(r); err != nil {
			return err
		}
		st.VaultImported = true
		for _, a := range st.Accounts {
			if a.Admin {
				if err := st.SetRule(a.ID, r.ID, "", acl.Write); err != nil {
					return err
				}
			}
		}
		w.log.Info("imported the configured vault as the first repository", "alias", r.Alias)
		return nil
	})
}

func (w *Workspace) dirs(r *store.Repo) (vaultPath, indexDir string) {
	if r.VaultPath != "" {
		return r.VaultPath, r.IndexDir
	}
	base := filepath.Join(w.dataDir, "repos", r.ID)
	return filepath.Join(base, "vault"), filepath.Join(base, "index")
}

func (w *Workspace) repoConfig(r *store.Repo) config.Config {
	c := w.cfg
	c.Vault.Path, c.Search.IndexDir = w.dirs(r)
	c.Vault.Remote = r.URL
	c.Git.Branch = r.Branch
	if r.Credential != store.CredHost {
		c.Git.Token = ""
	}
	return c
}

// gitOptions returns the credentials of a repository for Git, or nil when
// the process environment provides them. A decrypted SSH key is written to
// the private key directory.
func (w *Workspace) gitOptions(r *store.Repo) (*gitrepo.Options, string, error) {
	switch r.Credential {
	case store.CredHost:
		return nil, "", nil
	case store.CredToken:
		tok, err := w.box.Open(r.Secret, []byte(r.ID))
		if err != nil {
			return nil, "", err
		}
		return &gitrepo.Options{Token: string(tok), Username: w.cfg.Git.Username}, "", nil
	case store.CredSSHKey:
		key, err := w.box.Open(r.Secret, []byte(r.ID))
		if err != nil {
			return nil, "", err
		}
		file := filepath.Join(w.keyDir, r.ID)
		if err := os.WriteFile(file, key, 0o600); err != nil {
			return nil, "", err
		}
		return &gitrepo.Options{SSHKeyFile: file, KnownHostsFile: w.knownHosts, StrictHostKeys: w.strictHost}, file, nil
	}
	return nil, "", fmt.Errorf("unknown credential kind %q", r.Credential)
}

func isClone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// explain turns a clone or open failure into a reason for the administrator.
// Git's stderr may quote the remote URL but never a credential: tokens reach
// Git through the environment and URLs with credentials are refused.
func explain(r *store.Repo, err error) string {
	msg := err.Error()
	if hostKeyProblem(msg) {
		host := "the remote"
		if _, h, perr := parseRemote(r.URL, true); perr == nil && h != "" {
			host = h
		}
		return "the SSH host key of " + host + " is not trusted or has changed; nothing was sent to it"
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return msg
}

// start brings a repository up in the background: decrypt its credential,
// clone it if needed, open it and start its sync loops.
func (w *Workspace) start(r *store.Repo) {
	mctx, cancel := context.WithCancel(w.ctx)
	m := &mount{id: r.ID, alias: r.Alias, status: StatusCloning, done: make(chan struct{}), cancel: cancel}
	if r.KeyPending {
		m.status = StatusKeyPending
		close(m.done)
	}
	w.mu.Lock()
	w.mounts[r.ID] = m
	w.mu.Unlock()
	if r.KeyPending {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer close(m.done)
		svc, keyFile, status, reason, bad := w.open(mctx, r)
		w.mu.Lock()
		if w.mounts[r.ID] != m || w.ctx.Err() != nil {
			// Removed or restarted while this attempt was running.
			w.mu.Unlock()
			if svc != nil {
				_ = svc.Close()
			}
			return
		}
		m.svc, m.keyFile, m.status, m.reason, m.badCredential = svc, keyFile, status, reason, bad
		w.mu.Unlock()
		if svc != nil {
			svc.Start(w.ctx)
			w.log.Info("repository ready", "alias", r.Alias)
		} else {
			w.log.Warn("repository not available", "alias", r.Alias, "status", status, "reason", reason)
		}
	}()
}

func (w *Workspace) open(ctx context.Context, r *store.Repo) (svc *kb.Service, keyFile, status, reason string, badCredential bool) {
	vaultPath, _ := w.dirs(r)
	opts, keyFile, err := w.gitOptions(r)
	if err != nil {
		if !errors.Is(err, secret.ErrOpen) {
			return nil, "", StatusError, err.Error(), false
		}
		if !isClone(vaultPath) {
			return nil, "", StatusCredentialInvalid, "the stored credential cannot be decrypted with the current secret key; enter it again", true
		}
		// Keep serving the last clone, like an offline repository.
		opts, badCredential = &gitrepo.Options{}, true
	}
	if r.VaultPath == "" && !isClone(vaultPath) {
		// Leftovers of a failed clone would make the next one fail too.
		_ = os.RemoveAll(vaultPath)
	}
	svc, err = kb.OpenWith(ctx, w.repoConfig(r), w.log, w.version, kb.Options{Mount: r.Alias, Git: opts})
	if err != nil {
		return nil, keyFile, StatusError, explain(r, err), badCredential
	}
	return svc, keyFile, "", "", badCredential
}

// stop takes a repository out of service and waits for a running start
// attempt to finish.
func (w *Workspace) stop(id string) {
	w.mu.Lock()
	m := w.mounts[id]
	delete(w.mounts, id)
	w.mu.Unlock()
	if m == nil {
		return
	}
	m.cancel() // aborts a clone in progress
	<-m.done
	if m.svc != nil {
		_ = m.svc.Close()
	}
	if m.keyFile != "" {
		_ = os.Remove(m.keyFile)
	}
	_ = os.Remove(filepath.Join(w.keyDir, id))
}

// WaitReady blocks until no repository is still starting.
func (w *Workspace) WaitReady(ctx context.Context) error {
	if w.flat != nil {
		return nil
	}
	for {
		w.mu.RLock()
		var pending chan struct{}
		for _, m := range w.mounts {
			select {
			case <-m.done:
			default:
				pending = m.done
			}
		}
		w.mu.RUnlock()
		if pending == nil {
			return nil
		}
		select {
		case <-pending:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ---- administration ----

// RepoView is what the administrator sees of a repository. It never carries
// a token or a private key.
type RepoView struct {
	ID          string
	Alias       string
	URL         string
	Branch      string
	Credential  string
	Hint        string // last four characters of a token
	PublicKey   string
	Fingerprint string
	Status      string
	Reason      string
	LastSync    time.Time
	Unpushed    int
}

// Repos lists registered repositories with their status.
func (w *Workspace) Repos(ctx context.Context) []RepoView {
	var out []RepoView
	for _, r := range w.store.Snapshot().RepoList() {
		out = append(out, w.repoView(ctx, r))
	}
	return out
}

// Repo returns one repository by alias.
func (w *Workspace) Repo(ctx context.Context, alias string) (RepoView, bool) {
	r := w.store.Snapshot().RepoByAlias(alias)
	if r == nil {
		return RepoView{}, false
	}
	return w.repoView(ctx, r), true
}

func (w *Workspace) repoView(ctx context.Context, r *store.Repo) RepoView {
	v := RepoView{ID: r.ID, Alias: r.Alias, URL: r.URL, Branch: r.Branch, Credential: r.Credential, Hint: r.Hint,
		PublicKey: r.PublicKey, Fingerprint: r.Fingerprint, Status: StatusCloning}
	w.mu.RLock()
	m := w.mounts[r.ID]
	var snap mount
	if m != nil {
		snap = *m
	}
	w.mu.RUnlock()
	if m == nil {
		return v
	}
	v.Status, v.Reason = snap.status, snap.reason
	if snap.svc == nil {
		return v
	}
	st, _ := snap.svc.Status(ctx)
	v.Status, v.Reason, v.Unpushed = st.State, st.LastError, st.Ahead
	v.LastSync = st.LastPull
	if st.LastPush.After(v.LastSync) {
		v.LastSync = st.LastPush
	}
	switch {
	case snap.badCredential:
		v.Status, v.Reason = StatusCredentialInvalid, "the stored credential cannot be decrypted with the current secret key; enter it again"
	case st.State == StatusOffline && hostKeyProblem(st.LastError):
		v.Status, v.Reason = StatusError, explain(r, errors.New(st.LastError))
	}
	return v
}

// AddRepoRequest describes a repository to register.
type AddRepoRequest struct {
	Alias  string
	URL    string
	Branch string
	// Exactly one credential: a token for an HTTPS URL; for an SSH URL either
	// a private key or GenerateKey.
	Token       string
	SSHKey      string
	GenerateKey bool
}

// credential validates the credential of a request against the URL kind and
// fills the corresponding fields of r. The secret is sealed with the
// repository id, so r.ID must be set.
func (w *Workspace) credential(ctx context.Context, r *store.Repo, kind string, req AddRepoRequest) error {
	r.Secret, r.Hint, r.PublicKey, r.Fingerprint, r.KeyPending = nil, "", "", "", false
	token := strings.TrimSpace(req.Token)
	hasKey := strings.TrimSpace(req.SSHKey) != "" || req.GenerateKey
	var plain []byte
	switch kind {
	case remoteHTTPS, remoteLocal:
		if hasKey {
			return errors.New("an HTTPS URL takes an access token, not an SSH key")
		}
		if token == "" && kind == remoteHTTPS {
			return errors.New("an access token is required for an HTTPS URL")
		}
		r.Credential, plain = store.CredToken, []byte(token)
		if len(token) >= 8 {
			r.Hint = token[len(token)-4:]
		}
	case remoteSSH:
		if token != "" {
			return errors.New("an SSH URL takes an SSH key, not an access token")
		}
		var key *sshKey
		var err error
		switch {
		case req.GenerateKey && strings.TrimSpace(req.SSHKey) != "":
			return errors.New("either paste a private key or ask for one to be generated, not both")
		case req.GenerateKey:
			key, err = generateSSHKey(ctx, "knowledge-base-mcp:"+r.Alias)
			r.KeyPending = true
		case strings.TrimSpace(req.SSHKey) != "":
			key, err = inspectSSHKey(ctx, req.SSHKey)
		default:
			return errors.New("an SSH URL needs an SSH key: paste one or let the server generate it")
		}
		if err != nil {
			return err
		}
		r.Credential, plain = store.CredSSHKey, key.Private
		r.PublicKey, r.Fingerprint = key.Public, key.Fingerprint
	}
	sealed, err := w.box.Seal(plain, []byte(r.ID))
	if err != nil {
		return err
	}
	r.Secret = sealed
	return nil
}

// AddRepo registers a repository and starts cloning it in the background.
// It creates no access rules: the repository is visible to nobody until the
// administrator grants access.
func (w *Workspace) AddRepo(ctx context.Context, req AddRepoRequest) (RepoView, error) {
	req.Alias, req.URL, req.Branch = strings.TrimSpace(req.Alias), strings.TrimSpace(req.URL), strings.TrimSpace(req.Branch)
	if req.Branch == "" {
		req.Branch = "main"
	}
	if strings.HasPrefix(req.Branch, "-") || strings.ContainsAny(req.Branch, " \t\r\n\x00~^:?*[\\") {
		return RepoView{}, fmt.Errorf("%q is not a valid branch name", req.Branch)
	}
	if !store.ValidAlias(req.Alias) {
		return RepoView{}, fmt.Errorf("alias %q must be 1-32 lowercase letters, digits or hyphens and start with a letter or digit", req.Alias)
	}
	kind, _, err := parseRemote(req.URL, w.allowLocal)
	if err != nil {
		return RepoView{}, err
	}
	if w.store.Snapshot().RepoByAlias(req.Alias) != nil {
		return RepoView{}, fmt.Errorf("a repository with the alias %q already exists", req.Alias)
	}
	r := &store.Repo{ID: store.NewID("repo_"), Alias: req.Alias, URL: req.URL, Branch: req.Branch, CreatedAt: time.Now()}
	if err := w.credential(ctx, r, kind, req); err != nil {
		return RepoView{}, err
	}
	if err := w.store.Update(func(st *store.State) error { return st.AddRepo(r) }); err != nil {
		return RepoView{}, err
	}
	w.log.Info("repository added", "alias", r.Alias, "credential", r.Credential)
	w.start(r)
	return w.repoView(ctx, r), nil
}

// ReplaceCredential stores a new token or key for a repository and retries.
func (w *Workspace) ReplaceCredential(ctx context.Context, alias string, req AddRepoRequest) error {
	cur := w.store.Snapshot().RepoByAlias(alias)
	if cur == nil {
		return errNoRepo
	}
	if cur.Credential == store.CredHost {
		return errors.New("this repository uses the credentials of the server environment; change them there")
	}
	kind, _, err := parseRemote(cur.URL, w.allowLocal)
	if err != nil {
		return err
	}
	next := *cur
	if err := w.credential(ctx, &next, kind, req); err != nil {
		return err
	}
	if err := w.store.Update(func(st *store.State) error {
		r := st.Repos[cur.ID]
		if r == nil {
			return errNoRepo
		}
		r.Credential, r.Secret, r.Hint, r.PublicKey, r.Fingerprint, r.KeyPending = next.Credential, next.Secret, next.Hint, next.PublicKey, next.Fingerprint, next.KeyPending
		return nil
	}); err != nil {
		return err
	}
	w.log.Info("repository credential replaced", "alias", alias)
	return w.restart(cur.ID)
}

// Connect confirms that a generated key was registered with the Git host
// (or simply retries a repository in the error state) and starts the clone.
func (w *Workspace) Connect(alias string) error {
	cur := w.store.Snapshot().RepoByAlias(alias)
	if cur == nil {
		return errNoRepo
	}
	if cur.KeyPending {
		if err := w.store.Update(func(st *store.State) error {
			if r := st.Repos[cur.ID]; r != nil {
				r.KeyPending = false
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return w.restart(cur.ID)
}

func (w *Workspace) restart(id string) error {
	w.stop(id)
	r := w.store.Snapshot().Repos[id]
	if r == nil {
		return errNoRepo
	}
	w.start(r)
	return nil
}

// SyncRepo pulls and pushes one repository now, on the administrator's
// request and regardless of access rules. It returns no content.
func (w *Workspace) SyncRepo(ctx context.Context, alias string) error {
	m, err := w.readyMount(alias)
	if err != nil {
		return err
	}
	_, err = m.svc.SyncNow(ctx)
	return err
}

// UnpushedError refuses a removal that would lose local commits.
type UnpushedError struct{ Commits int }

func (e *UnpushedError) Error() string {
	return fmt.Sprintf("%d local commit(s) could not be pushed; removing the repository would lose them", e.Commits)
}

// RemoveRepo stops serving a repository and deletes its local clone, index
// and access rules. The remote is never touched. Local commits are pushed
// first; if some cannot be pushed the removal is refused unless
// discardUnpushed is set.
func (w *Workspace) RemoveRepo(ctx context.Context, alias string, discardUnpushed bool) error {
	r := w.store.Snapshot().RepoByAlias(alias)
	if r == nil {
		return errNoRepo
	}
	if m, ok := w.mountByAlias(alias); ok && m.svc != nil {
		_ = m.svc.PushNow(ctx)
		if n, _ := m.svc.Unpushed(ctx); n > 0 && !discardUnpushed {
			return &UnpushedError{Commits: n}
		}
	}
	w.stop(r.ID)
	if err := w.store.Update(func(st *store.State) error { return st.DeleteRepo(r.ID) }); err != nil {
		return err
	}
	if r.VaultPath != "" {
		// The imported vault may sit on mount points: empty them instead of
		// removing the directories themselves.
		// The index directory may also be the data directory, so only the
		// index itself goes.
		emptyDir(r.VaultPath)
		_ = os.RemoveAll(filepath.Join(r.IndexDir, "bleve"))
	} else {
		_ = os.RemoveAll(filepath.Join(w.dataDir, "repos", r.ID))
	}
	w.log.Info("repository removed", "alias", alias)
	return nil
}

// emptyDir deletes everything inside a directory, keeping the directory.
func emptyDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
