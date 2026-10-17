package kb

import (
	"context"
	"errors"
	"strings"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// Scope limits what one call may see and change inside the vault. Paths are
// vault-relative ("" is the root). A call without a scope is unrestricted,
// which is the case for the classic single-user server.
type Scope interface {
	// CanRead reports whether the path exists for the caller.
	CanRead(rel string) bool
	// CanWrite reports whether the caller may change the path.
	CanWrite(rel string) bool
	// Traversable reports whether a folder is readable or leads to
	// something readable, so it appears in listings.
	Traversable(folder string) bool
}

type scopeKey struct{}
type userKey struct{}

// WithScope attaches the caller's scope to a context.
func WithScope(ctx context.Context, sc Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, sc)
}

func scopeOf(ctx context.Context) Scope {
	sc, _ := ctx.Value(scopeKey{}).(Scope)
	return sc
}

// WithUser records the acting account for the KB-User commit trailer.
func WithUser(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, userKey{}, email)
}

func userOf(ctx context.Context) string {
	v, _ := ctx.Value(userKey{}).(string)
	return v
}

func (s *Service) canRead(ctx context.Context, rel string) bool {
	sc := scopeOf(ctx)
	return sc == nil || sc.CanRead(rel)
}

func (s *Service) canWrite(ctx context.Context, rel string) bool {
	sc := scopeOf(ctx)
	return sc == nil || sc.CanWrite(rel)
}

func (s *Service) traversable(ctx context.Context, folder string) bool {
	sc := scopeOf(ctx)
	return sc == nil || sc.Traversable(folder)
}

// visible returns the read predicate of the call, or nil when unrestricted
// so that callers can skip filtering altogether.
func (s *Service) visible(ctx context.Context) func(string) bool {
	sc := scopeOf(ctx)
	if sc == nil {
		return nil
	}
	return sc.CanRead
}

// ---- paths as the caller sees them ----

// in converts a path given by the caller into a vault-relative one. A
// mounted vault is addressed as "<mount>/<path>".
func (s *Service) in(p string) (string, error) {
	clean, err := vault.CleanRel(p)
	if err != nil {
		return "", err
	}
	if s.mount == "" {
		return clean, nil
	}
	if clean == s.mount {
		return "", nil
	}
	if rest, ok := strings.CutPrefix(clean, s.mount+"/"); ok {
		return rest, nil
	}
	return "", E(CodeNotFound, "not_found: %s", clean)
}

// out converts a vault-relative path into the form shown to the caller.
func (s *Service) out(rel string) string {
	if s.mount == "" {
		return rel
	}
	if rel == "" {
		return s.mount
	}
	return s.mount + "/" + rel
}

// prefix is what out puts in front of a non-empty relative path.
func (s *Service) prefix() string {
	if s.mount == "" {
		return ""
	}
	return s.mount + "/"
}

// ---- errors ----

// notFound is the one way a missing path is reported. A path the caller may
// not read is reported exactly the same way, so the two cannot be told apart.
func (s *Service) notFound(rel string) *Error {
	return E(CodeNotFound, "not_found: %s", s.out(rel))
}

func (s *Service) forbidden(rel string) *Error {
	return E(CodeForbidden, "%s is read-only for this account", s.out(rel))
}

// guardRead fails when the caller may not read rel.
func (s *Service) guardRead(ctx context.Context, rel string) error {
	if !s.canRead(ctx, rel) {
		return s.notFound(rel)
	}
	return nil
}

// guardWrite fails when the caller may not change rel: not_found when it
// cannot even be read, forbidden when it is readable but not writable.
func (s *Service) guardWrite(ctx context.Context, rel string) error {
	if !s.canRead(ctx, rel) {
		return s.notFound(rel)
	}
	if !s.canWrite(ctx, rel) {
		return s.forbidden(rel)
	}
	return nil
}

// wrap converts lower-level errors into tool errors, presenting paths the
// way the caller addressed them.
func (s *Service) wrap(err error) *Error {
	if err == nil {
		return nil
	}
	var ke *Error
	if errors.As(err, &ke) {
		return ke
	}
	var pe *vault.PathError
	if errors.As(err, &pe) {
		return E(CodeNotFound, "not_found: %s%s", s.out(pe.Path), pe.Detail)
	}
	return wrap(err)
}

// ---- history as the caller may see it ----

// redactCommit reduces a commit to what the caller may see. full is every
// path the commit changed. It returns false when nothing visible remains.
// Excluded paths (editor settings and the like) are dropped silently: nobody
// can read them, so they do not make a commit partial.
func (s *Service) redactCommit(ctx context.Context, c *gitrepo.Commit, full []gitrepo.Change) bool {
	var shown []gitrepo.Change
	hidden := false
	for _, ch := range full {
		newOK := !s.v.Excluded(ch.Path) && s.canRead(ctx, ch.Path)
		if ch.OldPath == "" {
			if s.v.Excluded(ch.Path) {
				continue
			}
			if !newOK {
				hidden = true
				continue
			}
			shown = append(shown, gitrepo.Change{Status: ch.Status, Path: s.out(ch.Path)})
			continue
		}
		oldOK := !s.v.Excluded(ch.OldPath) && s.canRead(ctx, ch.OldPath)
		switch {
		case newOK && oldOK:
			shown = append(shown, gitrepo.Change{Status: ch.Status, Path: s.out(ch.Path), OldPath: s.out(ch.OldPath)})
		case newOK:
			// Moved in from somewhere the caller cannot see: it just appears.
			hidden = true
			shown = append(shown, gitrepo.Change{Status: "A", Path: s.out(ch.Path)})
		case oldOK:
			hidden = true
			shown = append(shown, gitrepo.Change{Status: "D", Path: s.out(ch.OldPath)})
		default:
			if !s.v.Excluded(ch.Path) || !s.v.Excluded(ch.OldPath) {
				hidden = true
			}
		}
	}
	if len(shown) == 0 {
		return false
	}
	c.Changes = shown
	if hidden {
		c.Message, c.Partial = "", true
	}
	return true
}

// outChanges presents change paths to an unrestricted caller.
func (s *Service) outChanges(c *gitrepo.Commit) {
	if s.mount == "" {
		return
	}
	for i := range c.Changes {
		c.Changes[i].Path = s.out(c.Changes[i].Path)
		if c.Changes[i].OldPath != "" {
			c.Changes[i].OldPath = s.out(c.Changes[i].OldPath)
		}
	}
}
