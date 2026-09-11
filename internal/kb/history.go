package kb

import (
	"context"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// LogRequest is the kb_log input.
type LogRequest struct {
	Limit  int
	Cursor int // number of commits to skip
	Folder string
	Since  string
}

// LogResult is a page of commits.
type LogResult struct {
	Commits    []gitrepo.Commit `json:"commits"`
	NextCursor int              `json:"next_cursor,omitempty"`
}

// Log lists commits on the branch.
func (s *Service) Log(ctx context.Context, req LogRequest) (*LogResult, error) {
	if req.Limit <= 0 {
		req.Limit = 20
	}
	folder := ""
	if req.Folder != "" {
		clean, err := vault.CleanRel(req.Folder)
		if err != nil {
			return nil, wrap(err)
		}
		folder = clean
	}
	commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Limit: req.Limit + 1, Skip: req.Cursor, Folder: folder, Since: req.Since})
	if err != nil {
		return nil, wrap(err)
	}
	out := &LogResult{Commits: commits}
	if len(commits) > req.Limit {
		out.Commits = commits[:req.Limit]
		out.NextCursor = req.Cursor + req.Limit
	}
	if out.Commits == nil {
		out.Commits = []gitrepo.Commit{}
	}
	return out, nil
}

// History lists commits touching a note, following renames.
func (s *Service) History(ctx context.Context, path string, limit int) ([]gitrepo.Commit, error) {
	clean, err := vault.CleanRel(path)
	if err != nil {
		return nil, wrap(err)
	}
	if limit <= 0 {
		limit = 20
	}
	commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Path: clean, Limit: limit})
	if err != nil {
		return nil, wrap(err)
	}
	if len(commits) == 0 {
		return nil, E(CodeNotFound, "%s has no history", clean)
	}
	return commits, nil
}

// LsTree lists a folder at a revision.
func (s *Service) LsTree(ctx context.Context, revision, folder string) ([]gitrepo.TreeEntry, error) {
	clean, err := vault.CleanRel(folder)
	if err != nil {
		return nil, wrap(err)
	}
	if revision == "" {
		revision = "HEAD"
	}
	entries, err := s.repo.LsTree(ctx, revision, clean)
	if err != nil {
		return nil, wrap(err)
	}
	out := entries[:0]
	for _, e := range entries {
		if !s.v.Excluded(e.Path) {
			out = append(out, e)
		}
	}
	if out == nil {
		out = []gitrepo.TreeEntry{}
	}
	return out, nil
}

// RevisionView is the content of a note at a revision.
type RevisionView struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Content  string `json:"content"`
}

// ShowRevision returns a note as of a revision (deleted notes included).
func (s *Service) ShowRevision(ctx context.Context, path, revision string) (*RevisionView, error) {
	clean, err := vault.CleanRel(path)
	if err != nil {
		return nil, wrap(err)
	}
	if s.v.Excluded(clean) {
		return nil, E(CodeNotFound, "%s", clean)
	}
	if revision == "" {
		revision = "HEAD"
	}
	hash, err := s.repo.RevParse(ctx, revision)
	if err != nil {
		return nil, wrap(err)
	}
	data, err := s.repo.Show(ctx, hash, clean)
	if err != nil {
		return nil, wrap(err)
	}
	return &RevisionView{Path: clean, Revision: hash, Content: string(data)}, nil
}

// Diff returns a unified diff.
func (s *Service) Diff(ctx context.Context, from, to, path string) (string, error) {
	clean := ""
	if path != "" {
		c, err := vault.CleanRel(path)
		if err != nil {
			return "", wrap(err)
		}
		clean = c
	}
	if from == "" {
		return "", E(CodeInvalidArgument, "from revision is required")
	}
	d, err := s.repo.Diff(ctx, from, to, clean)
	if err != nil {
		return "", wrap(err)
	}
	return d, nil
}

// Info is the kb_info payload.
type Info struct {
	Version      string    `json:"version"`
	VaultPath    string    `json:"vault_path"`
	Branch       string    `json:"branch"`
	Remote       string    `json:"remote,omitempty"`
	NoteCount    int       `json:"note_count"`
	IndexedDocs  uint64    `json:"indexed_docs"`
	IndexUpdated time.Time `json:"index_updated"`
	SyncState    string    `json:"sync_state"`
	InstanceID   string    `json:"instance_id"`
	ReadOnly     bool      `json:"read_only"`
	Head         string    `json:"head,omitempty"`
}

// GetInfo reports server and vault facts.
func (s *Service) GetInfo(ctx context.Context) Info {
	docs, _ := s.idx.Count()
	head, _ := s.repo.Head(ctx)
	s.stateMu.RLock()
	updated := s.indexUpdated
	s.stateMu.RUnlock()
	state := s.State()
	if s.repo.MergeInProgress(ctx) {
		state = "conflict"
	}
	return Info{Version: s.version, VaultPath: s.cfg.Vault.Path, Branch: s.cfg.Git.Branch, Remote: s.remote,
		NoteCount: s.cat.Count(), IndexedDocs: docs, IndexUpdated: updated, SyncState: state,
		InstanceID: s.instanceID, ReadOnly: s.cfg.Server.ReadOnly, Head: head}
}

// Reindex rebuilds the index from scratch.
func (s *Service) Reindex(ctx context.Context) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.idx.Close(); err != nil {
		return Info{}, wrap(err)
	}
	if err := s.openIndex(ctx, true); err != nil {
		return Info{}, wrap(err)
	}
	return s.GetInfo(ctx), nil
}
