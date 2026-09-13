package kb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// Service owns one vault.
type Service struct {
	cfg     config.Config
	v       *vault.Vault
	repo    *gitrepo.Repo
	cat     *search.Catalog
	idx     search.Indexer
	log     *slog.Logger
	version string
	remote  string // "origin" or "" when no remote

	mu sync.Mutex // serialises writes and sync operations

	stateMu       sync.RWMutex
	state         string // ok | offline | conflict
	lastErr       string
	lastPull      time.Time
	lastPush      time.Time
	indexUpdated  time.Time
	pushRequested chan struct{}
	instanceID    string
	lock          *instanceLock
	linkMu        sync.Mutex
	linkKey       []byte
	stopOnce      sync.Once
	stop          chan struct{}
	wg            sync.WaitGroup
}

// Open prepares the service: clone if needed, validate the clone, open the
// index and build the catalog.
func Open(ctx context.Context, cfg config.Config, log *slog.Logger, version string) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	opts := gitrepo.Options{AuthorName: cfg.Git.AuthorName, AuthorEmail: cfg.Git.AuthorEmail, Token: cfg.Git.Token, Username: cfg.Git.Username, Logger: log}
	if _, err := os.Stat(cfg.Vault.Path); errors.Is(err, fs.ErrNotExist) || isEmptyDir(cfg.Vault.Path) {
		if cfg.Vault.Remote == "" {
			return nil, fmt.Errorf("vault path %s does not exist and no KB_GIT_REMOTE is configured to clone from", cfg.Vault.Path)
		}
		log.Info("cloning vault", "path", cfg.Vault.Path, "branch", cfg.Git.Branch)
		if isEmptyDir(cfg.Vault.Path) {
			_ = os.Remove(cfg.Vault.Path)
		}
		if err := gitrepo.Clone(ctx, cfg.Vault.Remote, cfg.Vault.Path, cfg.Git.Branch, opts); err != nil {
			return nil, fmt.Errorf("clone %s: %w", cfg.Vault.Path, err)
		}
	}
	repo := gitrepo.New(cfg.Vault.Path, opts)
	if !repo.IsWorkTree(ctx) {
		return nil, fmt.Errorf("vault path %s is not the top level of a Git work tree; a Git clone of the knowledge base is required", cfg.Vault.Path)
	}
	branch, err := repo.CurrentBranch(ctx)
	if err != nil {
		return nil, err
	}
	if branch != cfg.Git.Branch {
		return nil, fmt.Errorf("vault is on branch %q but the server is configured for %q; check out %q first", branch, cfg.Git.Branch, cfg.Git.Branch)
	}
	v, err := vault.Open(cfg.Vault.Path, cfg.Vault.Exclude)
	if err != nil {
		return nil, err
	}
	lock, err := acquireInstanceLock(cfg.Search.IndexDir)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, v: v, repo: repo, cat: search.NewCatalog(), log: log, version: version, lock: lock,
		pushRequested: make(chan struct{}, 1), stop: make(chan struct{}), state: "ok"}
	if repo.HasRemote(ctx, "origin") {
		s.remote = "origin"
	}
	s.instanceID = cfg.Git.InstanceID
	if s.instanceID == "" {
		host, _ := os.Hostname()
		s.instanceID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if err := s.openIndex(ctx, false); err != nil {
		lock.release()
		return nil, err
	}
	if repo.MergeInProgress(ctx) {
		s.setState("conflict", "merge in progress from a previous run")
	}
	return s, nil
}

func isEmptyDir(p string) bool {
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) == 0
}

// Start launches the background pull and push loops.
func (s *Service) Start(ctx context.Context) {
	if s.remote == "" {
		s.log.Warn("no 'origin' remote: running without pull/push")
		return
	}
	s.wg.Add(1)
	go s.pushLoop(ctx)
	if s.cfg.Git.PullInterval > 0 {
		s.wg.Add(1)
		go s.pullLoop(ctx)
	}
	// Initial sync in the background so startup stays fast.
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.pull(ctx); err != nil {
			s.log.Warn("initial pull failed", "err", err)
		}
		s.requestPush()
	}()
}

// Close stops background work and releases the index. It is idempotent.
func (s *Service) Close() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		err = s.idx.Close()
		s.lock.release()
	})
	return err
}

// ---- index and catalog ----

func (s *Service) openIndex(ctx context.Context, reset bool) error {
	idx, err := search.OpenBleve(s.cfg.Search.IndexDir, s.cfg.Search.Languages, reset)
	if err != nil {
		return fmt.Errorf("open index %s: %w", s.cfg.Search.IndexDir, err)
	}
	head, _ := s.repo.Head(ctx)
	stamp, ok, _ := idx.Stamp()
	fresh := ok && stamp.Schema == search.SchemaVersion && stamp.VaultPath == s.cfg.Vault.Path && strings.Join(stamp.Languages, ",") == strings.Join(s.cfg.Search.Languages, ",")
	if ok && !fresh && !reset {
		s.log.Info("index schema or vault changed, rebuilding")
		_ = idx.Close()
		return s.openIndex(ctx, true)
	}
	s.idx = idx
	// The catalog always comes from a full scan (cheap, in memory).
	metas, files, err := s.scan()
	if err != nil {
		return err
	}
	s.cat.Replace(metas, files)
	switch {
	case fresh && stamp.Revision == head && head != "":
		s.log.Info("index up to date", "revision", head[:8], "notes", len(metas))
	case fresh && stamp.Revision != "" && head != "":
		changes, derr := s.repo.DiffNameStatus(ctx, stamp.Revision, head)
		if derr == nil {
			s.log.Info("index catching up", "changes", len(changes))
			if err := s.applyChanges(ctx, changes); err != nil {
				return err
			}
			break
		}
		fallthrough
	default:
		s.log.Info("building index", "notes", len(metas))
		if err := s.indexAll(ctx, metas); err != nil {
			return err
		}
	}
	return s.stamp(ctx)
}

func (s *Service) stamp(ctx context.Context) error {
	head, _ := s.repo.Head(ctx)
	s.stateMu.Lock()
	s.indexUpdated = time.Now()
	s.stateMu.Unlock()
	return s.idx.SetStamp(search.Stamp{Schema: search.SchemaVersion, VaultPath: s.cfg.Vault.Path, Revision: head, UpdatedAt: time.Now(), Languages: s.cfg.Search.Languages})
}

func (s *Service) scan() ([]*search.NoteMeta, []vault.Entry, error) {
	var metas []*search.NoteMeta
	var files []vault.Entry
	err := s.v.Walk("", func(rel string, info fs.FileInfo) error {
		if !vault.IsMarkdown(rel) {
			files = append(files, vault.Entry{Path: rel, Kind: "attachment", Size: info.Size(), ModTime: info.ModTime()})
			return nil
		}
		n, err := s.v.ReadNote(rel)
		if err != nil {
			s.log.Warn("skip unreadable note", "path", rel, "err", err)
			return nil
		}
		metas = append(metas, search.MetaOf(n))
		return nil
	})
	return metas, files, err
}

func (s *Service) indexAll(ctx context.Context, metas []*search.NoteMeta) error {
	docs := make([]search.Document, 0, len(metas))
	for _, m := range metas {
		n, err := s.v.ReadNote(m.Path)
		if err != nil {
			continue
		}
		docs = append(docs, search.DocumentOf(m, string(n.Body)))
	}
	return s.idx.UpsertMany(ctx, docs)
}

// refreshPaths re-reads the given paths from disk and updates catalog+index;
// missing files are removed.
func (s *Service) refreshPaths(ctx context.Context, paths ...string) error {
	for _, p := range paths {
		if s.v.Excluded(p) {
			continue
		}
		if !vault.IsMarkdown(p) {
			if data, st, err := s.v.Read(p); err == nil {
				_ = data
				s.cat.UpsertFile(vault.Entry{Path: p, Kind: "attachment", Size: st.Size(), ModTime: st.ModTime()})
			} else {
				s.cat.Delete(p)
			}
			continue
		}
		n, err := s.v.ReadNote(p)
		if err != nil {
			s.cat.Delete(p)
			if err := s.idx.Delete(ctx, p); err != nil {
				return err
			}
			continue
		}
		m := search.MetaOf(n)
		s.cat.Upsert(m)
		if err := s.idx.Upsert(ctx, search.DocumentOf(m, string(n.Body))); err != nil {
			return err
		}
	}
	return s.stamp(ctx)
}

func (s *Service) applyChanges(ctx context.Context, changes []gitrepo.Change) error {
	var paths []string
	for _, c := range changes {
		if c.OldPath != "" {
			paths = append(paths, c.OldPath)
		}
		paths = append(paths, c.Path)
	}
	return s.refreshPaths(ctx, paths...)
}

// ---- state ----

func (s *Service) setState(state, lastErr string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state = state
	s.lastErr = lastErr
}

// State returns the sync state.
func (s *Service) State() string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.state
}

// MaxUploadBytes returns the configured upload limit.
func (s *Service) MaxUploadBytes() int64 { return s.cfg.Server.MaxUploadBytes() }

// ReadOnly reports whether writes are disabled.
func (s *Service) ReadOnly() bool { return s.cfg.Server.ReadOnly }

// Vault exposes the sandboxed vault (read helpers for resources).
func (s *Service) Vault() *vault.Vault { return s.v }

// InstanceID identifies this server instance in commit trailers.
func (s *Service) InstanceID() string { return s.instanceID }

// clientKey carries the MCP client name in a context.
type clientKey struct{}

// WithClient records the calling client's name for commit trailers.
func WithClient(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, clientKey{}, name)
}

func clientOf(ctx context.Context) string {
	v, _ := ctx.Value(clientKey{}).(string)
	return v
}

func (s *Service) commitMessage(ctx context.Context, subject, summary string) string {
	var b strings.Builder
	b.WriteString(subject)
	if summary = strings.TrimSpace(summary); summary != "" {
		b.WriteString("\n\n" + summary)
	}
	b.WriteString("\n\n")
	if c := clientOf(ctx); c != "" {
		b.WriteString("KB-Client: " + sanitizeTrailer(c) + "\n")
	}
	b.WriteString("KB-Instance: " + sanitizeTrailer(s.instanceID) + "\n")
	return b.String()
}

func sanitizeTrailer(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}
