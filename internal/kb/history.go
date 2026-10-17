package kb

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
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

// Log lists commits on the branch. A caller with a scope sees only commits
// that touch something it may read, reduced to that part.
func (s *Service) Log(ctx context.Context, req LogRequest) (*LogResult, error) {
	if req.Limit <= 0 {
		req.Limit = 20
	}
	folder := ""
	if req.Folder != "" {
		rel, err := s.in(req.Folder)
		if err != nil {
			return nil, s.wrap(err)
		}
		folder = rel
	}
	if s.visible(ctx) != nil {
		return s.scopedLog(ctx, req, folder)
	}
	commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Limit: req.Limit + 1, Skip: req.Cursor, Folder: folder, Since: req.Since})
	if err != nil {
		return nil, s.wrap(err)
	}
	out := &LogResult{Commits: commits}
	if len(commits) > req.Limit {
		out.Commits = commits[:req.Limit]
		out.NextCursor = req.Cursor + req.Limit
	}
	if out.Commits == nil {
		out.Commits = []gitrepo.Commit{}
	}
	for i := range out.Commits {
		s.outChanges(&out.Commits[i])
	}
	return out, nil
}

// scopedLog pages through the visible part of history. The cursor counts
// visible commits, so it says nothing about how many others exist.
func (s *Service) scopedLog(ctx context.Context, req LogRequest, folder string) (*LogResult, error) {
	out := &LogResult{Commits: []gitrepo.Commit{}}
	const batch = 200
	skipped := 0
	for raw := 0; ; raw += batch {
		// No path filter here: the full list of changed paths decides both
		// whether a commit is shown and whether it is shown in part.
		commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Limit: batch, Skip: raw, Since: req.Since})
		if err != nil {
			return nil, s.wrap(err)
		}
		for i := range commits {
			c := commits[i]
			if folder != "" && !touches(c.Changes, folder) {
				continue
			}
			if !s.redactCommit(ctx, &c, c.Changes) {
				continue
			}
			if folder != "" && !touches(c.Changes, s.out(folder)) {
				continue
			}
			if skipped < req.Cursor {
				skipped++
				continue
			}
			if len(out.Commits) == req.Limit {
				out.NextCursor = req.Cursor + req.Limit
				return out, nil
			}
			out.Commits = append(out.Commits, c)
		}
		if len(commits) < batch {
			return out, nil
		}
	}
}

func under(folder, p string) bool {
	return folder == "" || p == folder || strings.HasPrefix(p, folder+"/")
}

func touches(changes []gitrepo.Change, folder string) bool {
	for _, ch := range changes {
		if under(folder, ch.Path) || (ch.OldPath != "" && under(folder, ch.OldPath)) {
			return true
		}
	}
	return false
}

// History lists commits touching a note, following renames.
func (s *Service) History(ctx context.Context, path string, limit int) ([]gitrepo.Commit, error) {
	clean, err := s.in(path)
	if err != nil {
		return nil, s.wrap(err)
	}
	noHistory := E(CodeNotFound, "%s has no history", s.out(clean))
	if !s.canRead(ctx, clean) {
		return nil, noHistory
	}
	if limit <= 0 {
		limit = 20
	}
	commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Path: clean, Limit: limit})
	if err != nil {
		return nil, s.wrap(err)
	}
	if len(commits) == 0 {
		return nil, noHistory
	}
	if s.visible(ctx) == nil {
		for i := range commits {
			s.outChanges(&commits[i])
		}
		return commits, nil
	}
	hashes := make([]string, len(commits))
	for i, c := range commits {
		hashes[i] = c.Hash
	}
	full, err := s.repo.FullChanges(ctx, hashes)
	if err != nil {
		return nil, s.wrap(err)
	}
	out := []gitrepo.Commit{}
	for _, c := range commits {
		// The followed file's own change tells where it lived before.
		cameFromHidden := false
		for _, ch := range c.Changes {
			if ch.OldPath != "" && !s.canRead(ctx, ch.OldPath) {
				cameFromHidden = true
			}
		}
		if s.redactCommit(ctx, &c, full[c.Hash]) {
			out = append(out, c)
		}
		if cameFromHidden {
			break // earlier history belongs to a path the caller may not see
		}
	}
	if len(out) == 0 {
		return nil, noHistory
	}
	return out, nil
}

// LsTree lists a folder at a revision.
func (s *Service) LsTree(ctx context.Context, revision, folder string) ([]gitrepo.TreeEntry, error) {
	clean, err := s.in(folder)
	if err != nil {
		return nil, s.wrap(err)
	}
	if revision == "" {
		revision = "HEAD"
	}
	if _, err := s.repo.RevParse(ctx, revision); err != nil {
		return nil, s.wrap(err)
	}
	missing := E(CodeNotFound, "not_found: %s at %s", s.out(clean), revision)
	if !s.traversable(ctx, clean) {
		return nil, missing
	}
	entries, err := s.repo.LsTree(ctx, revision, clean)
	if err != nil {
		if errors.Is(err, gitrepo.ErrNotFound) {
			return nil, missing
		}
		return nil, s.wrap(err)
	}
	out := []gitrepo.TreeEntry{}
	for _, e := range entries {
		if s.v.Excluded(e.Path) {
			continue
		}
		if e.Kind == "folder" {
			if !s.traversable(ctx, e.Path) {
				continue
			}
		} else if !s.canRead(ctx, e.Path) {
			continue
		}
		e.Path = s.out(e.Path)
		out = append(out, e)
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
	clean, err := s.in(path)
	if err != nil {
		return nil, s.wrap(err)
	}
	if revision == "" {
		revision = "HEAD"
	}
	hash, err := s.repo.RevParse(ctx, revision)
	if err != nil {
		return nil, s.wrap(err)
	}
	missing := E(CodeNotFound, "not_found: %s at %s", s.out(clean), hash)
	if s.v.Excluded(clean) || !s.canRead(ctx, clean) {
		return nil, missing
	}
	data, err := s.repo.Show(ctx, hash, clean)
	if err != nil {
		return nil, missing
	}
	return &RevisionView{Path: s.out(clean), Revision: hash, Content: string(data)}, nil
}

// Diff returns a unified diff. A caller with a scope gets the readable paths
// only; a path it may not read diffs as empty, like a path that never existed.
func (s *Service) Diff(ctx context.Context, from, to, path string) (string, error) {
	clean := ""
	if path != "" {
		c, err := s.in(path)
		if err != nil {
			return "", s.wrap(err)
		}
		clean = c
	}
	if from == "" {
		return "", E(CodeInvalidArgument, "from revision is required")
	}
	vis := s.visible(ctx)
	if vis == nil {
		var paths []string
		if clean != "" {
			paths = []string{clean}
		}
		d, err := s.repo.DiffPaths(ctx, from, to, paths, s.prefix())
		return d, errOrNil(s.wrap(err))
	}
	changed, err := s.repo.ChangedPaths(ctx, from, to)
	if err != nil {
		return "", s.wrap(err)
	}
	paths := []string{}
	for _, p := range changed {
		if under(clean, p) && !s.v.Excluded(p) && vis(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return "", nil
	}
	d, err := s.repo.DiffPaths(ctx, from, to, paths, s.prefix())
	return d, errOrNil(s.wrap(err))
}

// errOrNil keeps a nil *Error from becoming a non-nil error value.
func errOrNil(e *Error) error {
	if e == nil {
		return nil
	}
	return e
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
	notes := s.cat.Count()
	if s.visible(ctx) != nil {
		notes = len(s.VisibleNotes(ctx))
	}
	return Info{Version: s.version, VaultPath: s.cfg.Vault.Path, Branch: s.cfg.Git.Branch, Remote: s.remote,
		NoteCount: notes, IndexedDocs: docs, IndexUpdated: updated, SyncState: state,
		InstanceID: s.instanceID, ReadOnly: s.cfg.Server.ReadOnly, Head: head}
}

// Reindex rebuilds the index from scratch.
func (s *Service) Reindex(ctx context.Context) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.idx.Close(); err != nil {
		return Info{}, s.wrap(err)
	}
	if err := s.openIndex(ctx, true); err != nil {
		return Info{}, s.wrap(err)
	}
	return s.GetInfo(ctx), nil
}
