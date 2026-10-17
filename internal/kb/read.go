package kb

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// NoteView is the full note representation returned by kb_get_note.
type NoteView struct {
	Path             string          `json:"path"`
	Title            string          `json:"title"`
	Content          string          `json:"content"`
	Frontmatter      map[string]any  `json:"frontmatter"`
	FrontmatterError string          `json:"frontmatter_error,omitempty"`
	Body             string          `json:"body"`
	Headings         []vault.Heading `json:"headings"`
	Tags             []string        `json:"tags"`
	Links            []vault.Link    `json:"links"`
	ETag             string          `json:"etag"`
	Size             int64           `json:"size"`
	Modified         time.Time       `json:"modified"`
	LastCommit       *gitrepo.Commit `json:"last_commit,omitempty"`
	Conflict         *Conflict       `json:"conflict,omitempty"`
}

// GetNote reads a note.
func (s *Service) GetNote(ctx context.Context, path string) (*NoteView, error) {
	rel, err := s.in(path)
	if err != nil {
		return nil, s.wrap(err)
	}
	if err := s.guardRead(ctx, rel); err != nil {
		return nil, err
	}
	n, err := s.v.ReadNote(rel)
	if err != nil {
		return nil, s.wrap(err)
	}
	view := s.viewOf(ctx, n)
	if commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Path: n.Path, Limit: 1}); err == nil && len(commits) > 0 {
		c := commits[0]
		if s.visible(ctx) == nil {
			c.Changes = nil
			view.LastCommit = &c
		} else if full, err := s.repo.FullChanges(ctx, []string{c.Hash}); err == nil && s.redactCommit(ctx, &c, full[c.Hash]) {
			c.Changes = nil
			view.LastCommit = &c
		}
	}
	if s.repo.MergeInProgress(ctx) {
		if rep, err := s.conflictReport(ctx); err == nil {
			for i := range rep.Conflicts {
				if rep.Conflicts[i].Path == view.Path {
					view.Conflict = &rep.Conflicts[i]
				}
			}
		}
	}
	return view, nil
}

func (s *Service) viewOf(ctx context.Context, n *vault.Note) *NoteView {
	links := append([]vault.Link(nil), n.Links...)
	for i := range links {
		// Links resolve over the whole vault, as an editor would; a target
		// the caller may not read is then reported as unresolved.
		links[i].Resolved = ""
		if r := s.cat.Resolve(n.Path, links[i].Target); r != "" && s.canRead(ctx, r) {
			links[i].Resolved = s.out(r)
		}
	}
	view := &NoteView{Path: s.out(n.Path), Title: n.Title, Content: string(n.Content), Frontmatter: n.Frontmatter.Map(),
		Body: string(n.Body), Headings: n.Headings, Tags: n.Tags, Links: links, ETag: n.ETag, Size: n.Size, Modified: n.ModTime}
	if view.Frontmatter == nil {
		view.Frontmatter = map[string]any{}
	}
	if view.Headings == nil {
		view.Headings = []vault.Heading{}
	}
	if view.Tags == nil {
		view.Tags = []string{}
	}
	if view.Links == nil {
		view.Links = []vault.Link{}
	}
	if n.Frontmatter.Err != nil {
		view.FrontmatterError = n.Frontmatter.Err.Error()
	}
	return view
}

// SectionView is returned by kb_get_section.
type SectionView struct {
	Path      string `json:"path"`
	Heading   string `json:"heading"`
	Level     int    `json:"level"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
}

// GetSection returns the text under a heading.
func (s *Service) GetSection(ctx context.Context, path, heading string) (*SectionView, error) {
	rel, err := s.in(path)
	if err != nil {
		return nil, s.wrap(err)
	}
	if err := s.guardRead(ctx, rel); err != nil {
		return nil, err
	}
	n, err := s.v.ReadNote(rel)
	if err != nil {
		return nil, s.wrap(err)
	}
	sec, err := vault.FindSection(n.Body, heading)
	if err != nil {
		return nil, E(CodeNotFound, "heading %q not found in %s", heading, s.out(n.Path))
	}
	return &SectionView{Path: s.out(n.Path), Heading: sec.Heading.Text, Level: sec.Heading.Level, StartLine: sec.StartLine, EndLine: sec.EndLine, Text: sec.Text}, nil
}

// ListRequest is the kb_list input.
type ListRequest struct {
	Folder    string
	Recursive bool
	Glob      string
	Limit     int
	Cursor    string
}

// ListResult is a page of entries.
type ListResult struct {
	Entries    []vault.Entry `json:"entries"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// List lists notes and folders. A caller with a scope sees the entries it
// may read plus the folders that lead to something it may read.
func (s *Service) List(ctx context.Context, req ListRequest) (*ListResult, error) {
	if req.Glob != "" && !doublestar.ValidatePattern(req.Glob) {
		return nil, E(CodeInvalidArgument, "invalid glob %q", req.Glob)
	}
	folder, err := s.in(req.Folder)
	if err != nil {
		return nil, s.wrap(err)
	}
	if !s.traversable(ctx, folder) {
		return nil, s.notFound(folder)
	}
	entries, err := s.v.List(folder, req.Recursive)
	if err != nil {
		return nil, s.wrap(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	out := &ListResult{Entries: []vault.Entry{}}
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	for _, e := range entries {
		if e.Kind == "folder" {
			if !s.traversable(ctx, e.Path) {
				continue
			}
		} else if !s.canRead(ctx, e.Path) {
			continue
		}
		e.Path = s.out(e.Path)
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

// Backlinks returns notes linking to path.
func (s *Service) Backlinks(ctx context.Context, path string) ([]search.Backlink, error) {
	clean, err := s.in(path)
	if err != nil {
		return nil, s.wrap(err)
	}
	asked := clean
	if _, ok := s.cat.Get(clean); !ok {
		clean = s.cat.Resolve("", strings.TrimSuffix(clean, ".md"))
	}
	if clean == "" || !s.canRead(ctx, clean) {
		return nil, s.notFound(asked)
	}
	bl := []search.Backlink{}
	for _, b := range s.cat.Backlinks(clean) {
		if !s.canRead(ctx, b.Source) {
			continue
		}
		b.Source = s.out(b.Source)
		bl = append(bl, b)
	}
	return bl, nil
}

// Tags returns tag counts over the notes the caller may read.
func (s *Service) Tags(ctx context.Context, prefix string) []search.TagCount {
	t := s.cat.TagsVisible(prefix, s.visible(ctx))
	if t == nil {
		t = []search.TagCount{}
	}
	return t
}

// ---- search wrappers ----

// searchFolder converts a folder filter. ok is false when the filter points
// outside this vault, in which case nothing can match.
func (s *Service) searchFolder(folder string) (rel string, ok bool, err error) {
	if folder == "" {
		return "", true, nil
	}
	rel, err = s.in(folder)
	if err != nil {
		var ke *Error
		if errors.As(err, &ke) && ke.Code == CodeNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return rel, true, nil
}

// Search runs the ranked full-text search.
func (s *Service) Search(ctx context.Context, req search.Request) (search.Result, error) {
	folder, ok, err := s.searchFolder(req.Filters.Folder)
	if err != nil {
		return search.Result{}, s.wrap(err)
	}
	if !ok {
		return search.Result{Hits: []search.Hit{}}, nil
	}
	req.Filters.Folder = folder
	req.Visible = s.visible(ctx)
	res, err := s.idx.Search(ctx, req)
	if err != nil {
		return res, E(CodeInvalidArgument, "search failed: %s", err)
	}
	if res.Hits == nil {
		res.Hits = []search.Hit{}
	}
	for i := range res.Hits {
		res.Hits[i].Path = s.out(res.Hits[i].Path)
	}
	return res, nil
}

// Grep runs the exact text search.
func (s *Service) Grep(ctx context.Context, req search.GrepRequest) (search.GrepResult, error) {
	empty := search.GrepResult{Files: []search.GrepFile{}}
	if req.MaxFileSize == 0 {
		req.MaxFileSize = s.cfg.GrepMaxBytes()
	}
	folder, ok, err := s.searchFolder(req.Folder)
	if err != nil {
		return empty, s.wrap(err)
	}
	if !ok {
		return empty, nil
	}
	if folder != "" {
		// A folder that is missing and one the caller may not enter look alike.
		if !s.traversable(ctx, folder) || !s.v.IsDir(folder) {
			return empty, s.notFound(folder)
		}
	}
	req.Folder, req.Visible, req.PathPrefix = folder, s.visible(ctx), s.prefix()
	res, err := search.Grep(ctx, s.v, req)
	if err != nil {
		return res, E(CodeInvalidArgument, "%s", err)
	}
	if res.Files == nil {
		res.Files = []search.GrepFile{}
	}
	for i := range res.Files {
		res.Files[i].Path = s.out(res.Files[i].Path)
	}
	return res, nil
}

// VisibleNotes returns the metadata of every note the caller may read, with
// paths as the caller sees them, sorted by path.
func (s *Service) VisibleNotes(ctx context.Context) []*search.NoteMeta {
	notes := s.cat.Notes()
	vis := s.visible(ctx)
	if vis == nil && s.mount == "" {
		return notes
	}
	out := make([]*search.NoteMeta, 0, len(notes))
	for _, n := range notes {
		if vis != nil && !vis(n.Path) {
			continue
		}
		m := *n
		m.Path = s.out(n.Path)
		out = append(out, &m)
	}
	return out
}

// Query evaluates a metadata query.
func (s *Service) Query(ctx context.Context, req search.QueryRequest) ([]search.Row, error) {
	rows, err := search.QueryNotes(s.VisibleNotes(ctx), req)
	if err != nil {
		return nil, E(CodeInvalidArgument, "invalid where clause: %s", err)
	}
	if rows == nil {
		rows = []search.Row{}
	}
	return rows, nil
}

// ContextChunks returns the scored sections matching a query, with paths as
// the caller sees them; AssembleContext turns them into a bundle.
func (s *Service) ContextChunks(ctx context.Context, req search.ContextRequest) ([]search.Chunk, error) {
	folder, ok, err := s.searchFolder(req.Filters.Folder)
	if err != nil {
		return nil, s.wrap(err)
	}
	if !ok {
		return nil, nil
	}
	req.Filters.Folder, req.Visible = folder, s.visible(ctx)
	chunks, err := search.ContextChunks(ctx, s.idx, s.v, req)
	if err != nil {
		return nil, E(CodeInvalidArgument, "%s", err)
	}
	for i := range chunks {
		chunks[i].Path = s.out(chunks[i].Path)
	}
	return chunks, nil
}

// Context builds a retrieval bundle.
func (s *Service) Context(ctx context.Context, req search.ContextRequest) (search.ContextResult, error) {
	chunks, err := s.ContextChunks(ctx, req)
	if err != nil {
		return search.ContextResult{Sources: []string{}}, err
	}
	res := search.AssembleContext(chunks, req.MaxChars)
	if res.Sources == nil {
		res.Sources = []string{}
	}
	return res, nil
}

// QuickOpen fuzzy-matches titles and paths.
func (s *Service) QuickOpen(ctx context.Context, text string, limit int) []search.Candidate {
	c := s.cat.QuickOpenVisible(text, limit, s.visible(ctx))
	if c == nil {
		c = []search.Candidate{}
	}
	for i := range c {
		c[i].Path = s.out(c[i].Path)
	}
	return c
}

// ChildFolders lists the direct sub-folders of a folder by name, ignoring
// any scope. It serves administration only (drawing the tree that access
// rules are attached to) and never exposes file names or content.
func (s *Service) ChildFolders(rel string) ([]string, error) {
	entries, err := s.v.List(rel, false)
	if err != nil {
		return nil, s.wrap(err)
	}
	var out []string
	for _, e := range entries {
		if e.Kind == "folder" {
			out = append(out, e.Path[strings.LastIndex(e.Path, "/")+1:])
		}
	}
	return out, nil
}

// FolderExists reports whether rel is a folder of the vault.
func (s *Service) FolderExists(rel string) bool { return rel == "" || s.v.IsDir(rel) }
