package search

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// GrepRequest describes an exact text search.
type GrepRequest struct {
	Pattern       string
	Regex         bool
	CaseSensitive bool
	Folder        string
	Glob          string
	ContextLines  int
	Limit         int
	MaxFileSize   int64
}

// GrepMatch is one matching line.
type GrepMatch struct {
	Line   int      `json:"line"` // 1-based
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// GrepFile groups matches by file.
type GrepFile struct {
	Path    string      `json:"path"`
	Matches []GrepMatch `json:"matches"`
}

// GrepResult is the grep response.
type GrepResult struct {
	Files     []GrepFile `json:"files"`
	Total     int        `json:"total"`
	Truncated bool       `json:"truncated"`
}

// Grep scans visible Markdown notes in parallel.
func Grep(ctx context.Context, v *vault.Vault, req GrepRequest) (GrepResult, error) {
	if req.Pattern == "" {
		return GrepResult{}, fmt.Errorf("pattern must not be empty")
	}
	if req.Limit <= 0 {
		req.Limit = 200
	}
	if req.Glob != "" && !doublestar.ValidatePattern(req.Glob) {
		return GrepResult{}, fmt.Errorf("invalid glob %q", req.Glob)
	}
	expr := req.Pattern
	if !req.Regex {
		expr = regexp.QuoteMeta(expr)
	}
	if !req.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return GrepResult{}, fmt.Errorf("invalid pattern: %w", err)
	}
	var paths []string
	err = v.Walk(req.Folder, func(rel string, info fs.FileInfo) error {
		if !vault.IsMarkdown(rel) {
			return nil
		}
		if req.MaxFileSize > 0 && info.Size() > req.MaxFileSize {
			return nil
		}
		if req.Glob != "" {
			if ok, _ := doublestar.Match(req.Glob, rel); !ok {
				return nil
			}
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return GrepResult{}, err
	}
	type job struct {
		i   int
		rel string
	}
	results := make([]GrepFile, len(paths))
	jobs := make(chan job)
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[j.i] = grepFile(v.Root, j.rel, re, req.ContextLines)
			}
		}()
	}
	for i, p := range paths {
		jobs <- job{i, p}
	}
	close(jobs)
	wg.Wait()
	out := GrepResult{}
	for _, f := range results {
		if len(f.Matches) == 0 {
			continue
		}
		remaining := req.Limit - out.Total
		if remaining <= 0 {
			out.Truncated = true
			break
		}
		if len(f.Matches) > remaining {
			f.Matches = f.Matches[:remaining]
			out.Truncated = true
		}
		out.Total += len(f.Matches)
		out.Files = append(out.Files, f)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, ctx.Err()
}

func grepFile(root, rel string, re *regexp.Regexp, ctxLines int) GrepFile {
	f := GrepFile{Path: rel}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return f
	}
	if !re.Match(data) {
		return f
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	for i, l := range lines {
		if !re.MatchString(l) {
			continue
		}
		m := GrepMatch{Line: i + 1, Text: l}
		if ctxLines > 0 {
			lo := i - ctxLines
			if lo < 0 {
				lo = 0
			}
			hi := i + ctxLines + 1
			if hi > len(lines) {
				hi = len(lines)
			}
			m.Before = append([]string(nil), lines[lo:i]...)
			m.After = append([]string(nil), lines[i+1:hi]...)
		}
		f.Matches = append(f.Matches, m)
	}
	return f
}
