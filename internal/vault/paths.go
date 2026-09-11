// Package vault models the knowledge base on disk: sandboxed paths, exclusion
// rules, note parsing (frontmatter, links, tags, sections) and atomic writes.
package vault

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ErrInvalidPath is returned for absolute, traversing or escaping paths.
var ErrInvalidPath = errors.New("invalid_path")

// ErrNotFound is returned for missing or excluded paths.
var ErrNotFound = errors.New("not_found")

// CleanRel validates a vault-relative path and returns its canonical form
// (forward slashes, no leading/trailing slash). "" denotes the vault root.
func CleanRel(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains NUL", ErrInvalidPath)
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || (len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("%w: absolute paths are not allowed: %q", ErrInvalidPath, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: parent references are not allowed: %q", ErrInvalidPath, p)
		}
	}
	p = strings.TrimSuffix(path.Clean("/"+p), "/")
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		p = ""
	}
	return p, nil
}

// Excluder decides which vault-relative paths are hidden.
type Excluder struct {
	patterns []string
}

// NewExcluder compiles user glob patterns; dot-files and dot-directories are
// always excluded.
func NewExcluder(patterns []string) (*Excluder, error) {
	for _, p := range patterns {
		if !doublestar.ValidatePattern(p) {
			return nil, fmt.Errorf("invalid exclude pattern %q", p)
		}
	}
	return &Excluder{patterns: patterns}, nil
}

// Excluded reports whether rel (canonical, "" = root) is hidden.
func (e *Excluder) Excluded(rel string) bool {
	if rel == "" {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	for _, p := range e.patterns {
		if ok, _ := doublestar.Match(p, rel); ok {
			return true
		}
		// A pattern like "Private/**" should also hide the folder itself.
		if strings.HasSuffix(p, "/**") {
			if ok, _ := doublestar.Match(strings.TrimSuffix(p, "/**"), rel); ok {
				return true
			}
		}
	}
	return false
}

// Resolve maps a canonical relative path to an absolute path inside root,
// verifying that symlinks do not escape. Missing files are allowed as long as
// the nearest existing ancestor stays inside root.
func Resolve(root, rel string) (string, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	probe := abs
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
				return "", fmt.Errorf("%w: path escapes the vault", ErrInvalidPath)
			}
			return abs, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("%w: path escapes the vault", ErrInvalidPath)
		}
		probe = parent
	}
}

// IsMarkdown reports whether rel names a Markdown note.
func IsMarkdown(rel string) bool {
	return strings.EqualFold(path.Ext(rel), ".md")
}

// Stem returns the note name without folder and extension.
func Stem(rel string) string {
	base := path.Base(rel)
	return strings.TrimSuffix(base, path.Ext(base))
}
