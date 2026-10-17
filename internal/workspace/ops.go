package workspace

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// ---- read ----

// GetNote reads a note.
func (w *Workspace) GetNote(ctx context.Context, path string) (*kb.NoteView, error) {
	t, err := w.route(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.GetNote(t.ctx, path)
}

// GetSection returns the text under a heading.
func (w *Workspace) GetSection(ctx context.Context, path, heading string) (*kb.SectionView, error) {
	t, err := w.route(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.GetSection(t.ctx, path, heading)
}

// List lists notes and folders. The root of a multi-user workspace holds
// one folder per repository the caller can reach.
func (w *Workspace) List(ctx context.Context, req kb.ListRequest) (*kb.ListResult, error) {
	clean, err := vault.CleanRel(req.Folder)
	if err != nil {
		return nil, kb.E(kb.CodeInvalidPath, "%s", err.Error())
	}
	if w.flat != nil || clean != "" {
		t, err := w.route(ctx, req.Folder)
		if err != nil {
			return nil, err
		}
		return t.svc.List(t.ctx, req)
	}
	if req.Glob != "" && !doublestar.ValidatePattern(req.Glob) {
		return nil, kb.E(kb.CodeInvalidArgument, "invalid glob %q", req.Glob)
	}
	var entries []vault.Entry
	for _, t := range w.reachable(ctx) {
		if !req.Recursive {
			entries = append(entries, vault.Entry{Path: t.alias, Kind: "folder"})
			continue
		}
		res, err := t.svc.List(t.ctx, kb.ListRequest{Folder: t.alias, Recursive: true, Limit: math.MaxInt32})
		if err != nil {
			return nil, err
		}
		entries = append(entries, res.Entries...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	out := &kb.ListResult{Entries: []vault.Entry{}}
	for _, e := range entries {
		if req.Cursor != "" && e.Path <= req.Cursor {
			continue
		}
		if req.Glob != "" {
			if ok, _ := doublestar.Match(req.Glob, e.Path); !ok {
				continue
			}
		}
		if len(out.Entries) == limit {
			out.NextCursor = out.Entries[len(out.Entries)-1].Path
			break
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// Backlinks returns notes linking to path, within its repository.
func (w *Workspace) Backlinks(ctx context.Context, path string) ([]search.Backlink, error) {
	t, err := w.route(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.Backlinks(t.ctx, path)
}

// Tags returns tag counts over every note the caller may read.
func (w *Workspace) Tags(ctx context.Context, prefix string) []search.TagCount {
	ts := w.reachable(ctx)
	if len(ts) == 1 {
		return ts[0].svc.Tags(ts[0].ctx, prefix)
	}
	sum := map[string]int{}
	for _, t := range ts {
		for _, tc := range t.svc.Tags(t.ctx, prefix) {
			sum[tc.Tag] += tc.Count
		}
	}
	out := make([]search.TagCount, 0, len(sum))
	for tag, n := range sum {
		out = append(out, search.TagCount{Tag: tag, Count: n})
	}
	search.SortTags(out)
	return out
}

// ---- search ----

// Search runs the ranked full-text search over every repository the caller
// can reach, or over one when a folder filter names it.
func (w *Workspace) Search(ctx context.Context, req search.Request) (search.Result, error) {
	if w.flat != nil || req.Filters.Folder != "" {
		t, err := w.route(ctx, req.Filters.Folder)
		if err != nil {
			return search.Result{Hits: []search.Hit{}}, quiet(err)
		}
		return t.svc.Search(t.ctx, req)
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	// Each repository contributes its best offset+limit hits; the page is
	// cut from the merged ranking. Scores come from separate indexes, so the
	// order across repositories is approximate.
	per := req
	per.Offset, per.Limit = 0, req.Offset+req.Limit
	out := search.Result{Hits: []search.Hit{}}
	var all []search.Hit
	for _, t := range w.reachable(ctx) {
		res, err := t.svc.Search(t.ctx, per)
		if err != nil {
			return out, err
		}
		all = append(all, res.Hits...)
		out.Total += res.Total
		out.TookMs += res.TookMs
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Path < all[j].Path
	})
	if req.Offset < len(all) {
		all = all[req.Offset:]
		if len(all) > req.Limit {
			all = all[:req.Limit]
		}
		out.Hits = all
	}
	return out, nil
}

// quiet turns "that folder is not reachable" into "nothing matched": a
// filter on a missing or hidden folder is not an error for search tools.
func quiet(err error) error {
	if ke, ok := err.(*kb.Error); ok && ke.Code == kb.CodeNotFound {
		return nil
	}
	return err
}

// Grep runs the exact text search.
func (w *Workspace) Grep(ctx context.Context, req search.GrepRequest) (search.GrepResult, error) {
	if w.flat != nil || req.Folder != "" {
		t, err := w.route(ctx, req.Folder)
		if err != nil {
			return search.GrepResult{Files: []search.GrepFile{}}, err
		}
		return t.svc.Grep(t.ctx, req)
	}
	if req.Limit <= 0 {
		req.Limit = 200
	}
	out := search.GrepResult{Files: []search.GrepFile{}}
	for _, t := range w.reachable(ctx) {
		per := req
		per.Limit = req.Limit - out.Total
		if per.Limit <= 0 {
			out.Truncated = true
			break
		}
		res, err := t.svc.Grep(t.ctx, per)
		if err != nil {
			return out, err
		}
		out.Files = append(out.Files, res.Files...)
		out.Total += res.Total
		out.Truncated = out.Truncated || res.Truncated
	}
	return out, nil
}

// Query evaluates a metadata query over every note the caller may read.
func (w *Workspace) Query(ctx context.Context, req search.QueryRequest) ([]search.Row, error) {
	ts := w.reachable(ctx)
	if len(ts) == 1 {
		return ts[0].svc.Query(ts[0].ctx, req)
	}
	var notes []*search.NoteMeta
	for _, t := range ts {
		notes = append(notes, t.svc.VisibleNotes(t.ctx)...)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Path < notes[j].Path })
	rows, err := search.QueryNotes(notes, req)
	if err != nil {
		return nil, kb.E(kb.CodeInvalidArgument, "invalid where clause: %s", err)
	}
	if rows == nil {
		rows = []search.Row{}
	}
	return rows, nil
}

// Context builds a retrieval bundle.
func (w *Workspace) Context(ctx context.Context, req search.ContextRequest) (search.ContextResult, error) {
	empty := search.ContextResult{Sources: []string{}}
	if w.flat != nil || req.Filters.Folder != "" {
		t, err := w.route(ctx, req.Filters.Folder)
		if err != nil {
			return empty, quiet(err)
		}
		return t.svc.Context(t.ctx, req)
	}
	var chunks []search.Chunk
	for _, t := range w.reachable(ctx) {
		c, err := t.svc.ContextChunks(t.ctx, req)
		if err != nil {
			return empty, err
		}
		chunks = append(chunks, c...)
	}
	res := search.AssembleContext(chunks, req.MaxChars)
	if res.Sources == nil {
		res.Sources = []string{}
	}
	return res, nil
}

// QuickOpen fuzzy-matches titles and paths.
func (w *Workspace) QuickOpen(ctx context.Context, text string, limit int) []search.Candidate {
	ts := w.reachable(ctx)
	if len(ts) == 1 {
		return ts[0].svc.QuickOpen(ts[0].ctx, text, limit)
	}
	all := []search.Candidate{}
	for _, t := range ts {
		all = append(all, t.svc.QuickOpen(t.ctx, text, limit)...)
	}
	return search.TopCandidates(all, limit)
}

// ---- write ----

// CreateNote creates a note.
func (w *Workspace) CreateNote(ctx context.Context, path, content string, frontmatter map[string]any, overwrite bool, summary string) (*kb.WriteResult, error) {
	t, err := w.routeWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.CreateNote(t.ctx, path, content, frontmatter, overwrite, summary)
}

// ReplaceNote replaces a note's content.
func (w *Workspace) ReplaceNote(ctx context.Context, path, content, etag, summary string) (*kb.WriteResult, error) {
	t, err := w.routeWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.ReplaceNote(t.ctx, path, content, etag, summary)
}

// PatchNote applies operations atomically.
func (w *Workspace) PatchNote(ctx context.Context, path string, ops []kb.PatchOp, etag, summary string) (*kb.WriteResult, error) {
	t, err := w.routeWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.PatchNote(t.ctx, path, ops, etag, summary)
}

// MoveNote renames a note or attachment inside one repository.
func (w *Workspace) MoveNote(ctx context.Context, from, to, summary string) (*kb.WriteResult, error) {
	src, err := w.routeWrite(ctx, from)
	if err != nil {
		return nil, err
	}
	dst, err := w.routeWrite(ctx, to)
	if err != nil {
		return nil, err
	}
	if src.svc != dst.svc {
		return nil, kb.E(kb.CodeInvalidArgument, "cannot move between repositories (%s and %s); create the note in the other repository and delete it here", src.alias, dst.alias)
	}
	return src.svc.MoveNote(src.ctx, from, to, summary)
}

// DeleteNote removes a note or attachment.
func (w *Workspace) DeleteNote(ctx context.Context, path, etag, summary string) (*kb.WriteResult, error) {
	t, err := w.routeWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.DeleteNote(t.ctx, path, etag, summary)
}

// Restore writes the content of path at revision as a new commit.
func (w *Workspace) Restore(ctx context.Context, path, revision, summary string) (*kb.WriteResult, error) {
	t, err := w.routeWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.Restore(t.ctx, path, revision, summary)
}

// ---- files ----

// ReadFile returns a file's bytes and metadata.
func (w *Workspace) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, *kb.FileInfo, error) {
	t, err := w.route(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	return t.svc.ReadFile(t.ctx, p, maxBytes)
}

// PutFile writes bytes to a path and commits.
func (w *Workspace) PutFile(ctx context.Context, p string, data []byte, overwrite bool, summary string) (*kb.WriteResult, error) {
	t, err := w.routeWrite(ctx, p)
	if err != nil {
		return nil, err
	}
	return t.svc.PutFile(t.ctx, p, data, overwrite, summary)
}

// ---- history ----

// Log lists commits of one repository.
func (w *Workspace) Log(ctx context.Context, req kb.LogRequest) (*kb.LogResult, error) {
	t, err := w.one(ctx, req.Folder, "folder")
	if err != nil {
		return nil, err
	}
	if req.Folder == "" {
		req.Folder = t.alias
	}
	return t.svc.Log(t.ctx, req)
}

// History lists commits touching a note.
func (w *Workspace) History(ctx context.Context, path string, limit int) ([]gitrepo.Commit, error) {
	t, err := w.route(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.History(t.ctx, path, limit)
}

// LsTree lists a folder at a revision.
func (w *Workspace) LsTree(ctx context.Context, revision, folder string) ([]gitrepo.TreeEntry, error) {
	t, err := w.one(ctx, folder, "folder")
	if err != nil {
		return nil, err
	}
	if folder == "" {
		folder = t.alias
	}
	return t.svc.LsTree(t.ctx, revision, folder)
}

// ShowRevision returns a note as of a revision.
func (w *Workspace) ShowRevision(ctx context.Context, path, revision string) (*kb.RevisionView, error) {
	t, err := w.route(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.svc.ShowRevision(t.ctx, path, revision)
}

// Diff returns a unified diff inside one repository.
func (w *Workspace) Diff(ctx context.Context, from, to, path string) (string, error) {
	t, err := w.one(ctx, path, "path")
	if err != nil {
		return "", err
	}
	if path == "" {
		path = t.alias
	}
	return t.svc.Diff(t.ctx, from, to, path)
}

// ---- sync ----

// RepoSync is the sync state of one repository.
type RepoSync struct {
	Repository string `json:"repository"`
	kb.SyncStatus
	// Error is set by kb_sync_now when this repository could not be
	// synchronised.
	Error *kb.Error `json:"error,omitempty"`
}

// SyncReport is the sync state of every repository available to the caller.
type SyncReport struct {
	Repositories []RepoSync `json:"repositories"`
}

// Status reports synchronisation state: one status for a classic server, one
// per repository (optionally narrowed to alias) for a multi-user server.
func (w *Workspace) Status(ctx context.Context, alias string) (any, error) {
	if w.flat != nil {
		return w.flat.Status(ctx)
	}
	ts, err := w.byAlias(ctx, alias)
	if err != nil {
		return nil, err
	}
	rep := SyncReport{Repositories: []RepoSync{}}
	for _, t := range ts {
		st, err := t.svc.Status(t.ctx)
		if err != nil {
			return nil, err
		}
		rep.Repositories = append(rep.Repositories, RepoSync{Repository: t.alias, SyncStatus: st})
	}
	return rep, nil
}

// SyncNow pulls and pushes immediately.
func (w *Workspace) SyncNow(ctx context.Context, alias string) (any, error) {
	if w.flat != nil {
		return w.flat.SyncNow(ctx)
	}
	ts, err := w.byAlias(ctx, alias)
	if err != nil {
		return nil, err
	}
	rep := SyncReport{Repositories: []RepoSync{}}
	for _, t := range ts {
		st, err := t.svc.SyncNow(t.ctx)
		rs := RepoSync{Repository: t.alias, SyncStatus: st}
		if err != nil {
			if alias != "" {
				return nil, err
			}
			// One failing repository must not hide the others.
			rs.SyncStatus, _ = t.svc.Status(t.ctx)
			if ke, ok := err.(*kb.Error); ok {
				rs.Error = kb.E(ke.Code, "%s", ke.Message)
			} else {
				rs.Error = kb.E(kb.CodeInternal, "%s", err.Error())
			}
		}
		rep.Repositories = append(rep.Repositories, rs)
	}
	return rep, nil
}

// RepoConflicts is the frozen merge of one repository.
type RepoConflicts struct {
	Repository string `json:"repository"`
	kb.ConflictReport
}

// ConflictsReport lists frozen merges per repository.
type ConflictsReport struct {
	Repositories []RepoConflicts `json:"repositories"`
}

// Conflicts lists frozen merges.
func (w *Workspace) Conflicts(ctx context.Context, alias string) (any, error) {
	if w.flat != nil {
		return w.flat.Conflicts(ctx)
	}
	ts, err := w.byAlias(ctx, alias)
	if err != nil {
		return nil, err
	}
	rep := ConflictsReport{Repositories: []RepoConflicts{}}
	for _, t := range ts {
		cr, err := t.svc.Conflicts(t.ctx)
		if err != nil {
			return nil, err
		}
		if alias == "" && len(cr.Conflicts) == 0 && cr.Hidden == 0 {
			continue
		}
		rep.Repositories = append(rep.Repositories, RepoConflicts{Repository: t.alias, ConflictReport: cr})
	}
	return rep, nil
}

// ResolveConflict applies resolutions to the frozen merge of one repository.
func (w *Workspace) ResolveConflict(ctx context.Context, summary string, res []kb.Resolution) (kb.ResolveResult, error) {
	if w.flat != nil {
		return w.flat.ResolveConflict(ctx, summary, res)
	}
	if w.ReadOnly() {
		return kb.ResolveResult{}, kb.E(kb.CodeReadOnly, "the server runs in read-only mode")
	}
	if len(res) == 0 {
		return kb.ResolveResult{}, kb.E(kb.CodeInvalidArgument, "resolutions must not be empty")
	}
	t, err := w.route(ctx, res[0].Path)
	if err != nil {
		return kb.ResolveResult{}, err
	}
	for _, r := range res[1:] {
		other, err := w.route(ctx, r.Path)
		if err != nil {
			return kb.ResolveResult{}, err
		}
		if other.svc != t.svc {
			return kb.ResolveResult{}, kb.E(kb.CodeInvalidArgument, "one call resolves the merge of one repository; %s and %s are in different repositories", res[0].Path, r.Path)
		}
	}
	return t.svc.ResolveConflict(t.ctx, summary, res)
}

// ---- ops ----

// RepoInfo describes one repository to an account that can read it.
type RepoInfo struct {
	Alias        string    `json:"alias"`
	Branch       string    `json:"branch"`
	SyncState    string    `json:"sync_state"`
	IndexUpdated time.Time `json:"index_updated"`
	NoteCount    int       `json:"note_count"`
}

// MultiInfo is the kb_info payload of a multi-user server. It carries no
// filesystem paths and no remote URLs.
type MultiInfo struct {
	Version      string     `json:"version"`
	InstanceID   string     `json:"instance_id"`
	Account      string     `json:"account"`
	ReadOnly     bool       `json:"read_only"`
	Repositories []RepoInfo `json:"repositories"`
}

// Info reports server facts.
func (w *Workspace) Info(ctx context.Context) any {
	if w.flat != nil {
		return w.flat.GetInfo(ctx)
	}
	c, _ := CallerOf(ctx)
	out := MultiInfo{Version: w.version, Account: c.Email, ReadOnly: w.ReadOnly(), Repositories: []RepoInfo{}}
	for _, t := range w.reachable(ctx) {
		i := t.svc.GetInfo(t.ctx)
		out.InstanceID = i.InstanceID
		out.Repositories = append(out.Repositories, RepoInfo{Alias: t.alias, Branch: i.Branch, SyncState: i.SyncState, IndexUpdated: i.IndexUpdated, NoteCount: i.NoteCount})
	}
	if out.InstanceID == "" {
		out.InstanceID = w.cfg.Git.InstanceID
	}
	return out
}

// RepoReindex is the outcome of rebuilding one index.
type RepoReindex struct {
	Repository string `json:"repository"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// Reindex rebuilds the full-text index. On a multi-user server only the
// administrator may do it, for any repository, and the result reports
// status only: no paths, titles or counts.
func (w *Workspace) Reindex(ctx context.Context, alias string) (any, error) {
	if w.flat != nil {
		return w.flat.Reindex(ctx)
	}
	c, ok := w.activeCaller(ctx, w.store.Snapshot())
	if !ok || !c.Admin {
		return nil, kb.E(kb.CodeForbidden, "only the administrator may rebuild an index")
	}
	out := []RepoReindex{}
	for _, m := range w.mountList() {
		if (alias != "" && m.alias != alias) || m.svc == nil {
			continue
		}
		r := RepoReindex{Repository: m.alias, Status: "reindexed"}
		if _, err := m.svc.Reindex(ctx); err != nil {
			r.Status, r.Error = "failed", "the index could not be rebuilt; see the server log"
			w.log.Error("reindex failed", "alias", m.alias, "err", err)
		}
		out = append(out, r)
	}
	if alias != "" && len(out) == 0 {
		return nil, notFound(strings.TrimSpace(alias))
	}
	return map[string]any{"repositories": out}, nil
}
