// Package acl evaluates folder-level access rules. It is pure: it knows
// nothing about accounts, repositories or storage, only about the rules one
// account holds inside one repository.
package acl

import "strings"

// Level is what an account may do under a folder.
type Level string

// Access levels. Write includes read.
const (
	None  Level = "none"
	Read  Level = "read"
	Write Level = "write"
)

// Valid reports whether l is one of the three levels.
func (l Level) Valid() bool { return l == None || l == Read || l == Write }

// CanRead reports whether the level allows reading.
func (l Level) CanRead() bool { return l == Read || l == Write }

// CanWrite reports whether the level allows changing content.
func (l Level) CanWrite() bool { return l == Write }

// Rule gives a level on a folder. Path is repository-relative with forward
// slashes and no leading or trailing slash; "" is the repository root.
type Rule struct {
	Path  string
	Level Level
}

// Set is the rules of one account in one repository.
type Set []Rule

// within reports whether p is folder itself or lies below it, matching whole
// path segments.
func within(folder, p string) bool {
	if folder == "" {
		return true
	}
	return p == folder || strings.HasPrefix(p, folder+"/")
}

// Match returns the rule that decides p: the one on the longest folder that
// contains p. ok is false when no rule applies.
func (s Set) Match(p string) (r Rule, ok bool) {
	best := -1
	for _, c := range s {
		if !within(c.Path, p) {
			continue
		}
		if !ok || len(c.Path) > best {
			r, ok, best = c, true, len(c.Path)
		}
	}
	return r, ok
}

// Level returns the effective level on p; None when no rule applies.
func (s Set) Level(p string) Level {
	if r, ok := s.Match(p); ok {
		return r.Level
	}
	return None
}

// CanRead reports whether p is readable.
func (s Set) CanRead(p string) bool { return s.Level(p).CanRead() }

// CanWrite reports whether p is writable.
func (s Set) CanWrite(p string) bool { return s.Level(p).CanWrite() }

// Traversable reports whether folder should appear in listings: it is
// readable itself or a readable rule exists somewhere below it, so the caller
// can navigate to what was granted.
func (s Set) Traversable(folder string) bool {
	if s.CanRead(folder) {
		return true
	}
	for _, c := range s {
		if c.Level.CanRead() && c.Path != folder && within(folder, c.Path) {
			return true
		}
	}
	return false
}

// Any reports whether the set makes anything readable.
func (s Set) Any() bool { return s.Traversable("") }
