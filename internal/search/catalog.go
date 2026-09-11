// Package search holds the in-memory catalog of note metadata, the
// full-text index, grep, structured queries and retrieval bundles.
package search

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// NoteMeta is the catalog entry for one note.
type NoteMeta struct {
	Path           string
	Title          string
	Frontmatter    map[string]any
	FrontmatterErr string
	Tags           []string
	Links          []vault.Link
	Headings       []vault.Heading
	Size           int64
	ModTime        time.Time
	ETag           string
}

// MetaOf builds catalog metadata from a parsed note.
func MetaOf(n *vault.Note) *NoteMeta {
	m := &NoteMeta{Path: n.Path, Title: n.Title, Frontmatter: n.Frontmatter.Map(), Tags: n.Tags,
		Links: append([]vault.Link(nil), n.Links...), Headings: n.Headings, Size: n.Size, ModTime: n.ModTime, ETag: n.ETag}
	if n.Frontmatter.Err != nil {
		m.FrontmatterErr = n.Frontmatter.Err.Error()
	}
	return m
}

// Backlink is an incoming link.
type Backlink struct {
	Source  string `json:"source"`
	Line    int    `json:"line"`
	Context string `json:"context"`
	Raw     string `json:"raw"`
}

// Catalog is the in-memory metadata store.
type Catalog struct {
	mu        sync.RWMutex
	notes     map[string]*NoteMeta
	files     map[string]vault.Entry
	resolver  *vault.Resolver
	backlinks map[string][]Backlink
}

// NewCatalog creates an empty catalog.
func NewCatalog() *Catalog {
	c := &Catalog{notes: map[string]*NoteMeta{}, files: map[string]vault.Entry{}}
	c.relink()
	return c
}

// Replace swaps in a full snapshot.
func (c *Catalog) Replace(notes []*NoteMeta, files []vault.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notes = make(map[string]*NoteMeta, len(notes))
	for _, n := range notes {
		c.notes[n.Path] = n
	}
	c.files = make(map[string]vault.Entry, len(files))
	for _, f := range files {
		c.files[f.Path] = f
	}
	c.relink()
}

// Upsert adds or updates a note.
func (c *Catalog) Upsert(n *NoteMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notes[n.Path] = n
	c.relink()
}

// UpsertFile adds or updates an attachment entry.
func (c *Catalog) UpsertFile(e vault.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[e.Path] = e
}

// Delete removes a note or attachment.
func (c *Catalog) Delete(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.notes[path]; ok {
		delete(c.notes, path)
		c.relink()
		return
	}
	delete(c.files, path)
}

// relink rebuilds the resolver and backlink graph; caller holds the lock.
func (c *Catalog) relink() {
	paths := make([]string, 0, len(c.notes))
	for p := range c.notes {
		paths = append(paths, p)
	}
	c.resolver = vault.NewResolver(paths)
	c.backlinks = map[string][]Backlink{}
	sort.Strings(paths)
	for _, p := range paths {
		n := c.notes[p]
		for i := range n.Links {
			l := &n.Links[i]
			l.Resolved = c.resolver.Resolve(p, l.Target)
			if l.Resolved != "" && l.Resolved != p {
				c.backlinks[l.Resolved] = append(c.backlinks[l.Resolved], Backlink{Source: p, Line: l.Line, Context: l.Context, Raw: l.Raw})
			}
		}
	}
}

// Get returns a note's metadata.
func (c *Catalog) Get(path string) (*NoteMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.notes[path]
	return n, ok
}

// Notes returns all notes sorted by path.
func (c *Catalog) Notes() []*NoteMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*NoteMeta, 0, len(c.notes))
	for _, n := range c.notes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Count returns the number of notes.
func (c *Catalog) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.notes)
}

// Resolve maps a link target to a path.
func (c *Catalog) Resolve(from, target string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resolver.Resolve(from, target)
}

// Backlinks returns incoming links for a path.
func (c *Catalog) Backlinks(path string) []Backlink {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Backlink(nil), c.backlinks[path]...)
}

// TagCount pairs a tag with its usage count.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Tags returns tags (optionally filtered by prefix) sorted by count then name.
func (c *Catalog) Tags(prefix string) []TagCount {
	c.mu.RLock()
	defer c.mu.RUnlock()
	counts := map[string]int{}
	for _, n := range c.notes {
		for _, t := range n.Tags {
			if prefix == "" || strings.HasPrefix(strings.ToLower(t), strings.ToLower(prefix)) {
				counts[t]++
			}
		}
	}
	out := make([]TagCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, TagCount{Tag: t, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}
