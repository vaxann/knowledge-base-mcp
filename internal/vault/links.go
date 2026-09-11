package vault

import (
	"path"
	"sort"
	"strings"
)

// Resolver maps link targets to canonical note paths using the
// shortest-unique-path rule common to wikilink editors.
type Resolver struct {
	byStem map[string][]string // lower-case stem -> paths
	paths  map[string]string   // lower-case path without .md -> path
}

// NewResolver indexes the given note paths.
func NewResolver(paths []string) *Resolver {
	r := &Resolver{byStem: map[string][]string{}, paths: map[string]string{}}
	for _, p := range paths {
		stem := strings.ToLower(Stem(p))
		r.byStem[stem] = append(r.byStem[stem], p)
		r.paths[strings.ToLower(strings.TrimSuffix(p, ".md"))] = p
	}
	for k := range r.byStem {
		sort.Slice(r.byStem[k], func(i, j int) bool {
			a, b := r.byStem[k][i], r.byStem[k][j]
			if len(a) != len(b) {
				return len(a) < len(b)
			}
			return a < b
		})
	}
	return r
}

// Resolve returns the canonical path for a link target written in note from
// (used for relative Markdown links), or "" when unresolvable.
func (r *Resolver) Resolve(from, target string) string {
	target = strings.TrimSpace(strings.TrimSuffix(target, ".md"))
	if target == "" {
		return ""
	}
	// Relative Markdown links (./x, ../x) resolve against the note's folder.
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		joined := path.Clean(path.Join(path.Dir(from), target))
		if p, ok := r.paths[strings.ToLower(joined)]; ok {
			return p
		}
		return ""
	}
	key := strings.ToLower(strings.TrimPrefix(target, "/"))
	if p, ok := r.paths[key]; ok {
		return p
	}
	if strings.Contains(key, "/") {
		// Path-qualified but not exact: match by suffix.
		for lp, p := range r.paths {
			if strings.HasSuffix(lp, "/"+key) {
				return p
			}
		}
		return ""
	}
	if cands := r.byStem[key]; len(cands) > 0 {
		return cands[0] // shortest path wins
	}
	return ""
}

// ResolveAll fills Link.Resolved for every link of a note.
func (r *Resolver) ResolveAll(n *Note) {
	for i := range n.Links {
		n.Links[i].Resolved = r.Resolve(n.Path, n.Links[i].Target)
	}
}
