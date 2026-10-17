package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// ContextRequest asks for a retrieval bundle.
type ContextRequest struct {
	Query    string
	MaxChars int
	Filters  Filters
	// Visible, when set, restricts the bundle to notes it accepts.
	Visible func(path string) bool
}

// ContextResult is the bundle.
type ContextResult struct {
	Text    string   `json:"text"`
	Sources []string `json:"sources"`
	Chunks  int      `json:"chunks"`
}

// Chunk is one heading-delimited section with its relevance.
type Chunk struct {
	Path    string
	Heading string
	Text    string
	Score   float64
}

// BuildContext ranks heading-delimited sections of the best-matching notes
// and concatenates them within the character budget.
func BuildContext(ctx context.Context, idx Indexer, v *vault.Vault, req ContextRequest) (ContextResult, error) {
	chunks, err := ContextChunks(ctx, idx, v, req)
	if err != nil {
		return ContextResult{}, err
	}
	return AssembleContext(chunks, req.MaxChars), nil
}

// ContextChunks returns the scored sections of the best-matching notes,
// unordered. Chunks of several vaults can be combined before assembly.
func ContextChunks(ctx context.Context, idx Indexer, v *vault.Vault, req ContextRequest) ([]Chunk, error) {
	res, err := idx.Search(ctx, Request{Query: req.Query, Limit: 20, Filters: req.Filters, Visible: req.Visible})
	if err != nil {
		return nil, err
	}
	qtoks := map[string]bool{}
	for _, t := range idx.Analyze(req.Query) {
		qtoks[t] = true
	}
	var chunks []Chunk
	for _, h := range res.Hits {
		n, err := v.ReadNote(h.Path)
		if err != nil {
			continue
		}
		for _, sec := range vault.Sections(n.Body) {
			text := strings.TrimSpace(sec.Text)
			if text == "" {
				continue
			}
			hits := 0
			for _, t := range idx.Analyze(text) {
				if qtoks[t] {
					hits++
				}
			}
			chunks = append(chunks, Chunk{Path: h.Path, Heading: sec.Heading.Text, Text: text, Score: h.Score * (1 + float64(hits))})
		}
	}
	return chunks, nil
}

// AssembleContext orders chunks by score and concatenates them, each under
// its source path, within the character budget.
func AssembleContext(chunks []Chunk, maxChars int) ContextResult {
	if maxChars <= 0 {
		maxChars = 8000
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].Score > chunks[j].Score })
	var b strings.Builder
	var sources []string
	seen := map[string]bool{}
	out := ContextResult{}
	for _, c := range chunks {
		header := fmt.Sprintf("## %s", c.Path)
		if c.Heading != "" {
			header += " › " + c.Heading
		}
		piece := header + "\n" + c.Text + "\n\n"
		if b.Len()+len(piece) > maxChars {
			if b.Len() == 0 {
				// Even the best chunk is too big: truncate it with a marker.
				room := maxChars - len(header) - len("\n[truncated]\n")
				if room > 0 {
					b.WriteString(header + "\n" + truncateRunes(c.Text, room) + "\n[truncated]\n")
					out.Chunks++
					sources = append(sources, c.Path)
				}
				break
			}
			continue
		}
		b.WriteString(piece)
		out.Chunks++
		if !seen[c.Path] {
			seen[c.Path] = true
			sources = append(sources, c.Path)
		}
	}
	out.Text = strings.TrimRight(b.String(), "\n")
	out.Sources = sources
	return out
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
