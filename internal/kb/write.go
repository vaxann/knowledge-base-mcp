package kb

import (
	"context"
	"fmt"
	"strings"

	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// WriteResult is returned by every write tool.
type WriteResult struct {
	Path             string   `json:"path"`
	ETag             string   `json:"etag,omitempty"`
	Commit           string   `json:"commit"`
	Warnings         []string `json:"warnings,omitempty"`
	ReferencingNotes []string `json:"referencing_notes,omitempty"`
}

// mutation is one write transaction.
type mutation struct {
	subject string   // commit subject, e.g. "kb_patch_note: a.md"
	summary string   // optional client text
	paths   []string // paths to stage (added, modified or deleted)
	result  *WriteResult
}

// mutate runs fn under the vault lock, after making sure the clone is
// current and not in a conflict, then commits, re-indexes and pushes.
func (s *Service) mutate(ctx context.Context, fn func() (*mutation, error)) (*WriteResult, error) {
	if s.ReadOnly() {
		return nil, E(CodeReadOnly, "the server runs in read-only mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repo.MergeInProgress(ctx) {
		return nil, s.mergeConflictError(ctx)
	}
	conflicted, err := s.pullIfStale(ctx)
	if err != nil {
		s.log.Warn("pull before write failed, writing on local state", "err", err)
	}
	if conflicted {
		return nil, s.mergeConflictError(ctx)
	}
	m, err := fn()
	if err != nil {
		return nil, wrap(err)
	}
	if err := s.repo.Stage(ctx, m.paths...); err != nil {
		return nil, wrap(err)
	}
	hash, err := s.repo.Commit(ctx, s.commitMessage(ctx, m.subject, m.summary))
	if err != nil {
		return nil, wrap(err)
	}
	if err := s.refreshPaths(ctx, m.paths...); err != nil {
		return nil, wrap(err)
	}
	m.result.Commit = hash
	s.requestPush()
	return m.result, nil
}

func (s *Service) checkNotePath(p string) (string, error) {
	clean, err := vault.CleanRel(p)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return "", E(CodeInvalidPath, "path must name a note")
	}
	if !vault.IsMarkdown(clean) {
		if s.v.Exists(clean) {
			return "", E(CodeUnsupportedFile, "%s is not a Markdown note", clean)
		}
		return "", E(CodeInvalidPath, "note paths must end with .md: %s", clean)
	}
	if _, _, err := s.v.Check(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func etagConflict(n *vault.Note) *Error {
	return E(CodeConflict, "%s changed since the etag was obtained; merge your change into the current content", n.Path).
		With("etag", n.ETag).With("content", string(n.Content))
}

func frontmatterWarnings(content []byte) []string {
	if err := vault.ValidateFrontmatter(content); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// CreateNote creates a note.
func (s *Service) CreateNote(ctx context.Context, path, content string, frontmatter map[string]any, overwrite bool, summary string) (*WriteResult, error) {
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkNotePath(path)
		if err != nil {
			return nil, err
		}
		if s.v.Exists(clean) && !overwrite {
			return nil, E(CodeAlreadyExists, "%s already exists", clean)
		}
		data := []byte(content)
		var warnings []string
		if len(frontmatter) > 0 {
			fm, body, eol := vault.Split(data)
			if fm.Err != nil {
				return nil, E(CodePatchFailed, "content has unparseable frontmatter, cannot merge fields: %s", fm.Err)
			}
			for k, v := range frontmatter {
				if err := fm.Set(k, v); err != nil {
					return nil, E(CodeInvalidArgument, "frontmatter %s: %s", k, err)
				}
			}
			if data, err = vault.Serialize(fm, body, eol); err != nil {
				return nil, err
			}
		} else {
			warnings = frontmatterWarnings(data)
		}
		if err := s.v.WriteAtomic(clean, data); err != nil {
			return nil, err
		}
		return &mutation{subject: "kb_create_note: " + clean, summary: summary, paths: []string{clean},
			result: &WriteResult{Path: clean, ETag: vault.ETagOf(data), Warnings: warnings}}, nil
	})
}

// ReplaceNote replaces a note's content.
func (s *Service) ReplaceNote(ctx context.Context, path, content, etag, summary string) (*WriteResult, error) {
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkNotePath(path)
		if err != nil {
			return nil, err
		}
		n, err := s.v.ReadNote(clean)
		if err != nil {
			return nil, err
		}
		if etag != "" && etag != n.ETag {
			return nil, etagConflict(n)
		}
		data := []byte(content)
		if err := s.v.WriteAtomic(clean, data); err != nil {
			return nil, err
		}
		warnings := frontmatterWarnings(data)
		if HasConflictMarkers(content) {
			warnings = append(warnings, CodeMarkersPresent+": the content still contains conflict markers")
		}
		return &mutation{subject: "kb_replace_note: " + clean, summary: summary, paths: []string{clean},
			result: &WriteResult{Path: clean, ETag: vault.ETagOf(data), Warnings: warnings}}, nil
	})
}

// PatchOp is one operation of kb_patch_note.
type PatchOp struct {
	Op      string         `json:"op"`
	Content string         `json:"content,omitempty"`
	Heading string         `json:"heading,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
	Keys    []string       `json:"keys,omitempty"`
	Find    string         `json:"find,omitempty"`
	Replace string         `json:"replace,omitempty"`
	All     bool           `json:"all,omitempty"`
}

// PatchNote applies operations atomically.
func (s *Service) PatchNote(ctx context.Context, path string, ops []PatchOp, etag, summary string) (*WriteResult, error) {
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkNotePath(path)
		if err != nil {
			return nil, err
		}
		n, err := s.v.ReadNote(clean)
		if err != nil {
			return nil, err
		}
		if etag != "" && etag != n.ETag {
			return nil, etagConflict(n)
		}
		if len(ops) == 0 {
			return nil, E(CodeInvalidArgument, "operations must not be empty")
		}
		fm, body, eol := n.Frontmatter, n.Body, n.EOL
		fmChanged := false
		for i, op := range ops {
			switch op.Op {
			case "append":
				body = appendText(body, eol, op.Content)
			case "prepend":
				body = []byte(strings.TrimRight(op.Content, "\n") + eol + string(body))
			case "replace_section":
				body, err = vault.ReplaceSection(body, eol, op.Heading, op.Content)
			case "insert_after_heading":
				body, err = vault.InsertAfterHeading(body, eol, op.Heading, op.Content)
			case "set_frontmatter":
				if fm.Err != nil {
					return nil, E(CodePatchFailed, "operation %d: %s", i, vault.ErrNoFrontmatter)
				}
				for k, v := range op.Fields {
					if err := fm.Set(k, v); err != nil {
						return nil, E(CodePatchFailed, "operation %d (%s): %s", i, k, err)
					}
				}
				fmChanged = fmChanged || len(op.Fields) > 0
			case "remove_frontmatter":
				if fm.Err != nil {
					return nil, E(CodePatchFailed, "operation %d: %s", i, vault.ErrNoFrontmatter)
				}
				for _, k := range op.Keys {
					if fm.Remove(k) {
						fmChanged = true
					}
				}
			case "find_replace":
				if op.Find == "" {
					return nil, E(CodePatchFailed, "operation %d: find must not be empty", i)
				}
				if !strings.Contains(string(body), op.Find) {
					return nil, E(CodePatchFailed, "operation %d: %q not found", i, op.Find)
				}
				count := 1
				if op.All {
					count = -1
				}
				body = []byte(strings.Replace(string(body), op.Find, op.Replace, count))
			default:
				return nil, E(CodePatchFailed, "operation %d: unknown op %q", i, op.Op)
			}
			if err != nil {
				return nil, E(CodePatchFailed, "operation %d (%s): %s", i, op.Op, err)
			}
		}
		var data []byte
		if fmChanged {
			data, err = vault.Serialize(fm, body, eol)
			if err != nil {
				return nil, E(CodePatchFailed, "%s", err)
			}
		} else {
			data = vault.Join(fm.Raw, body, eol)
		}
		if err := s.v.WriteAtomic(clean, data); err != nil {
			return nil, err
		}
		return &mutation{subject: "kb_patch_note: " + clean, summary: summary, paths: []string{clean},
			result: &WriteResult{Path: clean, ETag: vault.ETagOf(data)}}, nil
	})
}

func appendText(body []byte, eol, text string) []byte {
	b := string(body)
	if b != "" && !strings.HasSuffix(b, eol) {
		b += eol
	}
	return []byte(b + strings.TrimRight(text, "\n") + eol)
}

// MoveNote renames a note without touching other notes.
func (s *Service) MoveNote(ctx context.Context, from, to, summary string) (*WriteResult, error) {
	return s.mutate(ctx, func() (*mutation, error) {
		src, err := s.checkNotePath(from)
		if err != nil {
			return nil, err
		}
		dst, err := s.checkNotePath(to)
		if err != nil {
			return nil, err
		}
		if !s.v.Exists(src) {
			return nil, E(CodeNotFound, "%s", src)
		}
		if s.v.Exists(dst) {
			return nil, E(CodeAlreadyExists, "%s already exists", dst)
		}
		refs := []string{}
		for _, b := range s.cat.Backlinks(src) {
			if len(refs) == 0 || refs[len(refs)-1] != b.Source {
				refs = append(refs, b.Source)
			}
		}
		if err := s.v.Rename(src, dst); err != nil {
			return nil, err
		}
		data, _, _ := s.v.Read(dst)
		return &mutation{subject: fmt.Sprintf("kb_move_note: %s -> %s", src, dst), summary: summary, paths: []string{src, dst},
			result: &WriteResult{Path: dst, ETag: vault.ETagOf(data), ReferencingNotes: refs}}, nil
	})
}

// DeleteNote removes a note permanently (Git keeps the history).
func (s *Service) DeleteNote(ctx context.Context, path, etag, summary string) (*WriteResult, error) {
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkNotePath(path)
		if err != nil {
			return nil, err
		}
		n, err := s.v.ReadNote(clean)
		if err != nil {
			return nil, err
		}
		if etag != "" && etag != n.ETag {
			return nil, etagConflict(n)
		}
		if err := s.v.Remove(clean); err != nil {
			return nil, err
		}
		return &mutation{subject: "kb_delete_note: " + clean, summary: summary, paths: []string{clean},
			result: &WriteResult{Path: clean}}, nil
	})
}

// Restore writes the content of path at revision as a new commit.
func (s *Service) Restore(ctx context.Context, path, revision, summary string) (*WriteResult, error) {
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkNotePath(path)
		if err != nil {
			return nil, err
		}
		hash, err := s.repo.RevParse(ctx, revision)
		if err != nil {
			return nil, err
		}
		data, err := s.repo.Show(ctx, hash, clean)
		if err != nil {
			return nil, err
		}
		if err := s.v.WriteAtomic(clean, data); err != nil {
			return nil, err
		}
		return &mutation{subject: fmt.Sprintf("kb_restore: %s (from %s)", clean, hash[:12]), summary: summary, paths: []string{clean},
			result: &WriteResult{Path: clean, ETag: vault.ETagOf(data)}}, nil
	})
}
