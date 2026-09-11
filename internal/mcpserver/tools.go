package mcpserver

import (
	"context"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
)

// ---- read ----

type pathIn struct {
	Path string `json:"path" jsonschema:"Vault-relative path of the note, e.g. Projects/Alpha.md"`
}

type sectionIn struct {
	Path    string `json:"path" jsonschema:"Vault-relative path of the note"`
	Heading string `json:"heading" jsonschema:"Heading text to look for (case-insensitive, with or without leading #)"`
}

type listIn struct {
	Folder    string `json:"folder,omitempty" jsonschema:"Folder to list; empty for the vault root"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"Include all descendants instead of direct children"`
	Glob      string `json:"glob,omitempty" jsonschema:"Optional glob on the relative path, e.g. **/*.md"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Page size (default 200)"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from a previous page"`
}

type tagsIn struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"Only tags starting with this prefix"`
}

func (s *Server) registerReadTools() {
	ro := toolOpts{readOnly: true, idempotent: true}
	addTool(s, "kb_get_note", "Read a note: raw content, parsed frontmatter, body, headings, tags, outgoing links (resolved), etag for concurrency control, last commit, and conflict sides when the note is part of a frozen merge.", ro,
		func(ctx context.Context, in pathIn) (any, error) { return s.svc.GetNote(ctx, in.Path) })
	addTool(s, "kb_get_section", "Return only the text under a heading (up to the next heading of the same or higher level).", ro,
		func(ctx context.Context, in sectionIn) (any, error) {
			return s.svc.GetSection(ctx, in.Path, in.Heading)
		})
	addTool(s, "kb_list", "List notes, attachments and folders under a folder, sorted by path, with cursor pagination and an optional glob.", ro,
		func(ctx context.Context, in listIn) (any, error) {
			return s.svc.List(ctx, kb.ListRequest{Folder: in.Folder, Recursive: in.Recursive, Glob: in.Glob, Limit: in.Limit, Cursor: in.Cursor})
		})
	addTool(s, "kb_backlinks", "List notes that link to the given note (wikilinks and relative Markdown links), with the line context of each link. Accepts a path or a bare note name.", ro,
		func(ctx context.Context, in pathIn) (any, error) { return s.svc.Backlinks(ctx, in.Path) })
	addTool(s, "kb_tags", "List tags (frontmatter and inline #tags) with the number of notes using each, sorted by count.", ro,
		func(ctx context.Context, in tagsIn) (any, error) { return s.svc.Tags(ctx, in.Prefix), nil })
}

// ---- search ----

type filtersIn struct {
	Folder         string            `json:"folder,omitempty" jsonschema:"Only notes under this folder"`
	Tags           []string          `json:"tags,omitempty" jsonschema:"Notes must carry all of these tags"`
	Frontmatter    map[string]string `json:"frontmatter,omitempty" jsonschema:"Exact frontmatter field=value matches"`
	ModifiedAfter  string            `json:"modified_after,omitempty" jsonschema:"RFC3339 or YYYY-MM-DD"`
	ModifiedBefore string            `json:"modified_before,omitempty" jsonschema:"RFC3339 or YYYY-MM-DD"`
}

func (f filtersIn) toFilters() (search.Filters, error) {
	out := search.Filters{Folder: f.Folder, Tags: f.Tags, Frontmatter: f.Frontmatter}
	parse := func(s string) (*time.Time, error) {
		if s == "" {
			return nil, nil
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return &t, nil
			}
		}
		return nil, kb.E(kb.CodeInvalidArgument, "invalid date %q (use RFC3339 or YYYY-MM-DD)", s)
	}
	var err error
	if out.ModifiedAfter, err = parse(f.ModifiedAfter); err != nil {
		return out, err
	}
	if out.ModifiedBefore, err = parse(f.ModifiedBefore); err != nil {
		return out, err
	}
	return out, nil
}

type searchIn struct {
	Query   string    `json:"query" jsonschema:"Search terms. Quote phrases (\"annual report\"), use word* for prefix matching. Russian and English are stemmed."`
	Limit   int       `json:"limit,omitempty" jsonschema:"Max results (default 10)"`
	Offset  int       `json:"offset,omitempty" jsonschema:"Results to skip"`
	Filters filtersIn `json:"filters,omitempty" jsonschema:"Optional filters applied before ranking"`
}

type grepIn struct {
	Pattern       string `json:"pattern" jsonschema:"Literal text (default) or RE2 regular expression"`
	Regex         bool   `json:"regex,omitempty" jsonschema:"Treat pattern as a regular expression"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema:"Case-sensitive matching (default false)"`
	Folder        string `json:"folder,omitempty" jsonschema:"Restrict to a folder"`
	Glob          string `json:"glob,omitempty" jsonschema:"Restrict to paths matching a glob, e.g. Finance/**"`
	ContextLines  int    `json:"context_lines,omitempty" jsonschema:"Lines of context before and after each match"`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max matching lines (default 200)"`
}

type queryIn struct {
	Where  string   `json:"where,omitempty" jsonschema:"Predicate over frontmatter and metadata, e.g. type eq \"contact\" and (status eq \"active\" or not expires exists). Operators: eq ne in contains exists gt gte lt lte; fields path folder title tags modified size or any frontmatter key; dates as YYYY-MM-DD."`
	Select []string `json:"select,omitempty" jsonschema:"Fields to return (path is always included); empty returns title and all frontmatter"`
	Sort   string   `json:"sort,omitempty" jsonschema:"\"field asc\" or \"field desc\""`
	Limit  int      `json:"limit,omitempty" jsonschema:"Max rows"`
}

type contextIn struct {
	Query    string    `json:"query" jsonschema:"What the caller wants to know"`
	MaxChars int       `json:"max_chars,omitempty" jsonschema:"Character budget for the bundle (default 8000)"`
	Filters  filtersIn `json:"filters,omitempty"`
}

type quickOpenIn struct {
	Text  string `json:"text" jsonschema:"Approximate note name or path"`
	Limit int    `json:"limit,omitempty" jsonschema:"Max candidates (default 10)"`
}

func (s *Server) registerSearchTools() {
	ro := toolOpts{readOnly: true, idempotent: true}
	addTool(s, "kb_search", "Ranked full-text search over titles, bodies, tags and frontmatter with Russian/English stemming, highlighted snippets and filters. Use kb_grep for exact matches.", ro,
		func(ctx context.Context, in searchIn) (any, error) {
			f, err := in.Filters.toFilters()
			if err != nil {
				return nil, err
			}
			return s.svc.Search(ctx, search.Request{Query: in.Query, Limit: in.Limit, Offset: in.Offset, Filters: f})
		})
	addTool(s, "kb_grep", "Exact line search across notes (literal or RE2 regex), like ripgrep: returns path, line number, line text and optional context, including matches inside code blocks. Ideal for finding every [[link]] to a note.", ro,
		func(ctx context.Context, in grepIn) (any, error) {
			if err := requireString("pattern", in.Pattern); err != nil {
				return nil, err
			}
			return s.svc.Grep(ctx, search.GrepRequest{Pattern: in.Pattern, Regex: in.Regex, CaseSensitive: in.CaseSensitive, Folder: in.Folder, Glob: in.Glob, ContextLines: in.ContextLines, Limit: in.Limit})
		})
	addTool(s, "kb_query", "Structured query over frontmatter and note metadata (Dataview-style): filter with a where clause, choose fields, sort and limit.", ro,
		func(ctx context.Context, in queryIn) (any, error) {
			return s.svc.Query(ctx, search.QueryRequest{Where: in.Where, Select: in.Select, Sort: in.Sort, Limit: in.Limit})
		})
	addTool(s, "kb_context", "Retrieval bundle: the most relevant note sections for a query, concatenated with their source paths within a character budget, so a question can be answered from one call.", ro,
		func(ctx context.Context, in contextIn) (any, error) {
			if err := requireString("query", in.Query); err != nil {
				return nil, err
			}
			f, err := in.Filters.toFilters()
			if err != nil {
				return nil, err
			}
			return s.svc.Context(ctx, search.ContextRequest{Query: in.Query, MaxChars: in.MaxChars, Filters: f})
		})
	addTool(s, "kb_quick_open", "Resolve an approximate note name or path to candidates (prefix, substring and fuzzy matching on titles and paths).", ro,
		func(ctx context.Context, in quickOpenIn) (any, error) {
			return s.svc.QuickOpen(ctx, in.Text, in.Limit), nil
		})
}

// ---- write ----

type createIn struct {
	Path        string         `json:"path" jsonschema:"Vault-relative path ending in .md; parent folders are created"`
	Content     string         `json:"content" jsonschema:"Full Markdown content (may include its own frontmatter)"`
	Frontmatter map[string]any `json:"frontmatter,omitempty" jsonschema:"Frontmatter fields merged over any header in content"`
	Overwrite   bool           `json:"overwrite,omitempty" jsonschema:"Replace an existing note instead of failing with already_exists"`
	Summary     string         `json:"summary,omitempty" jsonschema:"Optional commit message paragraph"`
}

type replaceIn struct {
	Path    string `json:"path"`
	Content string `json:"content" jsonschema:"New full content"`
	ETag    string `json:"etag,omitempty" jsonschema:"etag from kb_get_note; the write fails with conflict if the note changed"`
	Summary string `json:"summary,omitempty"`
}

type patchIn struct {
	Path       string       `json:"path"`
	Operations []kb.PatchOp `json:"operations" jsonschema:"Ordered operations applied atomically: append, prepend, replace_section(heading,content), insert_after_heading(heading,content), set_frontmatter(fields), remove_frontmatter(keys), find_replace(find,replace,all)"`
	ETag       string       `json:"etag,omitempty"`
	Summary    string       `json:"summary,omitempty"`
}

type moveIn struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Summary string `json:"summary,omitempty"`
}

type deleteIn struct {
	Path    string `json:"path"`
	ETag    string `json:"etag,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type restoreIn struct {
	Path     string `json:"path"`
	Revision string `json:"revision" jsonschema:"Commit hash or Git revision such as HEAD~3"`
	Summary  string `json:"summary,omitempty"`
}

type resolveIn struct {
	Resolutions []kb.Resolution `json:"resolutions" jsonschema:"One entry per conflicted path with content (final text), take (ours|theirs) or delete"`
	Summary     string          `json:"summary,omitempty" jsonschema:"Explain how the conflict was resolved"`
}

func (s *Server) registerWriteTools() {
	w := toolOpts{}
	addTool(s, "kb_create_note", "Create a Markdown note (one commit). Fails with already_exists unless overwrite is set. No templates: read a template note first if you need one.", w,
		func(ctx context.Context, in createIn) (any, error) {
			return s.svc.CreateNote(ctx, in.Path, in.Content, in.Frontmatter, in.Overwrite, in.Summary)
		})
	addTool(s, "kb_replace_note", "Replace the whole content of a note (one commit). Pass the etag to avoid overwriting concurrent edits; a conflict error returns the current content.", toolOpts{idempotent: true},
		func(ctx context.Context, in replaceIn) (any, error) {
			return s.svc.ReplaceNote(ctx, in.Path, in.Content, in.ETag, in.Summary)
		})
	addTool(s, "kb_patch_note", "Apply targeted edits atomically (one commit): append/prepend text, replace or insert under a heading, set/remove frontmatter keys, find/replace.", w,
		func(ctx context.Context, in patchIn) (any, error) {
			return s.svc.PatchNote(ctx, in.Path, in.Operations, in.ETag, in.Summary)
		})
	addTool(s, "kb_move_note", "Rename or move a note (one commit). Links in other notes are NOT rewritten; the result lists referencing_notes so you can update them.", w,
		func(ctx context.Context, in moveIn) (any, error) {
			return s.svc.MoveNote(ctx, in.From, in.To, in.Summary)
		})
	addTool(s, "kb_delete_note", "Delete a note permanently (one commit). Git history keeps it: use kb_restore to bring it back.", toolOpts{destructive: true},
		func(ctx context.Context, in deleteIn) (any, error) {
			return s.svc.DeleteNote(ctx, in.Path, in.ETag, in.Summary)
		})
	addTool(s, "kb_restore", "Write the content of a note as of a revision into the vault as a new commit (also recreates deleted notes).", w,
		func(ctx context.Context, in restoreIn) (any, error) {
			return s.svc.Restore(ctx, in.Path, in.Revision, in.Summary)
		})
	addTool(s, "kb_resolve_conflict", "Resolve a frozen merge: supply the final content (no conflict markers), or take ours/theirs, or delete, for each conflicted path. When all paths are resolved the merge is committed and pushed.", w,
		func(ctx context.Context, in resolveIn) (any, error) {
			return s.svc.ResolveConflict(ctx, in.Summary, in.Resolutions)
		})
}

// ---- git ----

type logIn struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Commits per page (default 20)"`
	Cursor int    `json:"cursor,omitempty" jsonschema:"next_cursor from a previous page"`
	Folder string `json:"folder,omitempty" jsonschema:"Only commits touching this folder"`
	Since  string `json:"since,omitempty" jsonschema:"Only commits after this date (YYYY-MM-DD or Git date)"`
}

type historyIn struct {
	Path  string `json:"path"`
	Limit int    `json:"limit,omitempty"`
}

type lsTreeIn struct {
	Revision string `json:"revision,omitempty" jsonschema:"Commit hash or revision (default HEAD)"`
	Folder   string `json:"folder,omitempty" jsonschema:"Folder to list at that revision"`
}

type showRevIn struct {
	Path     string `json:"path"`
	Revision string `json:"revision,omitempty" jsonschema:"Commit hash or revision (default HEAD)"`
}

type diffIn struct {
	From string `json:"from" jsonschema:"Base revision"`
	To   string `json:"to,omitempty" jsonschema:"Target revision; empty means the work tree"`
	Path string `json:"path,omitempty" jsonschema:"Limit the diff to one note"`
}

type emptyIn struct{}

func (s *Server) registerGitTools() {
	ro := toolOpts{readOnly: true, idempotent: true}
	addTool(s, "kb_log", "Commit log of the vault branch, newest first, with changed paths and pagination.", ro,
		func(ctx context.Context, in logIn) (any, error) {
			return s.svc.Log(ctx, kb.LogRequest{Limit: in.Limit, Cursor: in.Cursor, Folder: in.Folder, Since: in.Since})
		})
	addTool(s, "kb_history", "Commits touching one note, following renames and including the commit that deleted it.", ro,
		func(ctx context.Context, in historyIn) (any, error) { return s.svc.History(ctx, in.Path, in.Limit) })
	addTool(s, "kb_ls_tree", "List notes and folders as they existed at a revision (find deleted or renamed notes).", ro,
		func(ctx context.Context, in lsTreeIn) (any, error) { return s.svc.LsTree(ctx, in.Revision, in.Folder) })
	addTool(s, "kb_show_revision", "Content of a note at a revision, including notes deleted since.", ro,
		func(ctx context.Context, in showRevIn) (any, error) {
			return s.svc.ShowRevision(ctx, in.Path, in.Revision)
		})
	addTool(s, "kb_diff", "Unified diff between two revisions (or a revision and the work tree), optionally for one note.", ro,
		func(ctx context.Context, in diffIn) (any, error) {
			d, err := s.svc.Diff(ctx, in.From, in.To, in.Path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"from": in.From, "to": in.To, "path": in.Path, "diff": d}, nil
		})
	addTool(s, "kb_sync_status", "Synchronisation state: branch, ahead/behind, last pull/push, last error, and conflicted paths while a merge is frozen.", ro,
		func(ctx context.Context, _ emptyIn) (any, error) { return s.svc.Status(ctx) })
	addTool(s, "kb_conflicts", "Details of a frozen merge: for each conflicted path the kind, content with markers, and base/ours/theirs sides, plus the remote commits being merged.", ro,
		func(ctx context.Context, _ emptyIn) (any, error) { return s.svc.Conflicts(ctx) })
	addTool(s, "kb_sync_now", "Pull and push immediately; fails with merge_conflict when the merge needs a resolution.", toolOpts{idempotent: true},
		func(ctx context.Context, _ emptyIn) (any, error) { return s.svc.SyncNow(ctx) })
}

// ---- ops ----

func (s *Server) registerOpsTools() {
	addTool(s, "kb_info", "Server version, vault path, branch, note and index counts, index freshness, sync state and instance id.", toolOpts{readOnly: true, idempotent: true},
		func(ctx context.Context, _ emptyIn) (any, error) { return s.svc.GetInfo(ctx), nil })
	addTool(s, "kb_reindex", "Rebuild the full-text index from scratch.", toolOpts{idempotent: true},
		func(ctx context.Context, _ emptyIn) (any, error) { return s.svc.Reindex(ctx) })
}

var _ = sprintf
