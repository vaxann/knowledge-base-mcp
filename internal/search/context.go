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
}

// ContextResult is the bundle.
type ContextResult struct {
	Text    string   `json:"text"`
	Sources []string `json:"sources"`
	Chunks  int      `json:"chunks"`
}

type chunk struct {
	path    string
	heading string
	text    string
	score   float64
}

// BuildContext ranks heading-delimited sections of the best-matching notes
// and concatenates them within the character budget.
func BuildContext(ctx context.Context, idx Indexer, v *vault.Vault, req ContextRequest) (ContextResult, error) {
	if req.MaxChars <= 0 {
		req.MaxChars = 8000
	}
	res, err := idx.Search(ctx, Request{Query: req.Query, Limit: 20, Filters: req.Filters})
	if err != nil {
		return ContextResult{}, err
	}
	qtoks := map[string]bool{}
	for _, t := range idx.Analyze(req.Query) {
		qtoks[t] = true
	}
	var chunks []chunk
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
			chunks = append(chunks, chunk{path: h.Path, heading: sec.Heading.Text, text: text, score: h.Score * (1 + float64(hits))})
		}
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].score > chunks[j].score })
	var b strings.Builder
	var sources []string
	seen := map[string]bool{}
	out := ContextResult{}
	for _, c := range chunks {
		header := fmt.Sprintf("## %s", c.path)
		if c.heading != "" {
			header += " › " + c.heading
		}
		piece := header + "\n" + c.text + "\n\n"
		if b.Len()+len(piece) > req.MaxChars {
			if b.Len() == 0 {
				// Even the best chunk is too big: truncate it with a marker.
				room := req.MaxChars - len(header) - len("\n[truncated]\n")
				if room > 0 {
					b.WriteString(header + "\n" + truncateRunes(c.text, room) + "\n[truncated]\n")
					out.Chunks++
					sources = append(sources, c.path)
				}
				break
			}
			continue
		}
		b.WriteString(piece)
		out.Chunks++
		if !seen[c.path] {
			seen[c.path] = true
			sources = append(sources, c.path)
		}
	}
	out.Text = strings.TrimRight(b.String(), "\n")
	out.Sources = sources
	return out, nil
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
