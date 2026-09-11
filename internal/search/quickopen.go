package search

import (
	"sort"
	"strings"
	"unicode"

	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// Candidate is a quick-open match.
type Candidate struct {
	Path  string  `json:"path"`
	Title string  `json:"title"`
	Score float64 `json:"score"`
}

// QuickOpen ranks notes by prefix, substring and fuzzy matches of text
// against titles and paths.
func (c *Catalog) QuickOpen(text string, limit int) []Candidate {
	tokens := strings.Fields(strings.ToLower(text))
	if len(tokens) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Candidate
	for _, n := range c.notes {
		haystacks := []string{strings.ToLower(vault.Stem(n.Path)), strings.ToLower(n.Title), strings.ToLower(n.Path)}
		total := 0.0
		for _, tok := range tokens {
			best := 0.0
			for hi, h := range haystacks {
				s := tokenScore(tok, h)
				if hi == 2 {
					s *= 0.8 // path matches are slightly weaker than title matches
				}
				if s > best {
					best = s
				}
			}
			if best == 0 {
				total = 0
				break
			}
			total += best
		}
		if total > 0 {
			out = append(out, Candidate{Path: n.Path, Title: n.Title, Score: total / float64(len(tokens))})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func tokenScore(tok, h string) float64 {
	if h == "" {
		return 0
	}
	if h == tok {
		return 1
	}
	words := strings.FieldsFunc(h, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	for _, w := range words {
		if w == tok {
			return 0.95
		}
	}
	if strings.HasPrefix(h, tok) {
		return 0.9
	}
	for _, w := range words {
		if strings.HasPrefix(w, tok) {
			return 0.85
		}
	}
	if strings.Contains(h, tok) {
		return 0.7
	}
	if span := subsequenceSpan(tok, h); span > 0 {
		return 0.5 * float64(len([]rune(tok))) / float64(span)
	}
	return 0
}

// subsequenceSpan returns the length of the shortest window of h that
// contains tok as a subsequence, or 0 when it is not a subsequence.
func subsequenceSpan(tok, h string) int {
	t, hr := []rune(tok), []rune(h)
	best := 0
	for start := 0; start < len(hr); start++ {
		if hr[start] != t[0] {
			continue
		}
		i, j := start, 0
		for i < len(hr) && j < len(t) {
			if hr[i] == t[j] {
				j++
			}
			i++
		}
		if j == len(t) {
			span := i - start
			if best == 0 || span < best {
				best = span
			}
		}
	}
	return best
}
