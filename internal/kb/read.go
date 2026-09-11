package kb

import (
	"context"
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
	n, err := s.v.ReadNote(path)
	if err != nil {
		return nil, wrap(err)
	}
	view := s.viewOf(n)
	if commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Path: n.Path, Limit: 1}); err == nil && len(commits) > 0 {
		c := commits[0]
		c.Changes = nil
		view.LastCommit = &c
	}
	if s.repo.MergeInProgress(ctx) {
		if rep, err := s.conflictReport(ctx); err == nil {
			for i := range rep.Conflicts {
				if rep.Conflicts[i].Path == n.Path {
					view.Conflict = &rep.Conflicts[i]
				}
			}
		}
	}
	return view, nil
}

func (s *Service) viewOf(n *vault.Note) *NoteView {
	links := append([]vault.Link(nil), n.Links...)
	for i := range links {
		links[i].Resolved = s.cat.Resolve(n.Path, links[i].Target)
	}
	view := &NoteView{Path: n.Path, Title: n.Title, Content: string(n.Content), Frontmatter: n.Frontmatter.Map(),
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
func (s *Service) GetSection(_ context.Context, path, heading string) (*SectionView, error) {
	n, err := s.v.ReadNote(path)
	if err != nil {
		return nil, wrap(err)
	}
	sec, err := vault.FindSection(n.Body, heading)
	if err != nil {
		return nil, E(CodeNotFound, "heading %q not found in %s", heading, n.Path)
	}
	return &SectionView{Path: n.Path, Heading: sec.Heading.Text, Level: sec.Heading.Level, StartLine: sec.StartLine, EndLine: sec.EndLine, Text: sec.Text}, nil
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

// List lists notes and folders.
func (s *Service) List(_ context.Context, req ListRequest) (*ListResult, error) {
	if req.Glob != "" && !doublestar.ValidatePattern(req.Glob) {
		return nil, E(CodeInvalidArgument, "invalid glob %q", req.Glob)
	}
	entries, err := s.v.List(req.Folder, req.Recursive)
	if err != nil {
		return nil, wrap(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	out := &ListResult{Entries: []vault.Entry{}}
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
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

// Backlinks returns notes linking to path.
func (s *Service) Backlinks(_ context.Context, path string) ([]search.Backlink, error) {
	clean, err := vault.CleanRel(path)
	if err != nil {
		return nil, wrap(err)
	}
	if _, ok := s.cat.Get(clean); !ok {
		if resolved := s.cat.Resolve("", strings.TrimSuffix(clean, ".md")); resolved != "" {
			clean = resolved
		} else {
			return nil, E(CodeNotFound, "%s", clean)
		}
	}
	bl := s.cat.Backlinks(clean)
	if bl == nil {
		bl = []search.Backlink{}
	}
	return bl, nil
}

// Tags returns tag counts.
func (s *Service) Tags(_ context.Context, prefix string) []search.TagCount {
	t := s.cat.Tags(prefix)
	if t == nil {
		t = []search.TagCount{}
	}
	return t
}

// ---- search wrappers ----

// Search runs the ranked full-text search.
func (s *Service) Search(ctx context.Context, req search.Request) (search.Result, error) {
	res, err := s.idx.Search(ctx, req)
	if err != nil {
		return res, E(CodeInvalidArgument, "search failed: %s", err)
	}
	if res.Hits == nil {
		res.Hits = []search.Hit{}
	}
	return res, nil
}

// Grep runs the exact text search.
func (s *Service) Grep(ctx context.Context, req search.GrepRequest) (search.GrepResult, error) {
	if req.MaxFileSize == 0 {
		req.MaxFileSize = s.cfg.GrepMaxBytes()
	}
	res, err := search.Grep(ctx, s.v, req)
	if err != nil {
		return res, E(CodeInvalidArgument, "%s", err)
	}
	if res.Files == nil {
		res.Files = []search.GrepFile{}
	}
	return res, nil
}

// Query evaluates a metadata query.
func (s *Service) Query(_ context.Context, req search.QueryRequest) ([]search.Row, error) {
	rows, err := s.cat.RunQuery(req)
	if err != nil {
		return nil, E(CodeInvalidArgument, "invalid where clause: %s", err)
	}
	if rows == nil {
		rows = []search.Row{}
	}
	return rows, nil
}

// Context builds a retrieval bundle.
func (s *Service) Context(ctx context.Context, req search.ContextRequest) (search.ContextResult, error) {
	res, err := search.BuildContext(ctx, s.idx, s.v, req)
	if err != nil {
		return res, E(CodeInvalidArgument, "%s", err)
	}
	if res.Sources == nil {
		res.Sources = []string{}
	}
	return res, nil
}

// QuickOpen fuzzy-matches titles and paths.
func (s *Service) QuickOpen(_ context.Context, text string, limit int) []search.Candidate {
	c := s.cat.QuickOpen(text, limit)
	if c == nil {
		c = []search.Candidate{}
	}
	return c
}
