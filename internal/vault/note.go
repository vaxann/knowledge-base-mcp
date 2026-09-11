package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// Note is a parsed Markdown note.
type Note struct {
	Path        string
	Content     []byte
	Frontmatter Frontmatter
	Body        []byte
	EOL         string
	Title       string
	Headings    []Heading
	Tags        []string
	Links       []Link
	ETag        string
	Size        int64
	ModTime     time.Time
}

// Heading is an ATX heading inside the body.
type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	Line  int    `json:"line"` // 0-based line index in the body
}

// Link is an outgoing link found in the body.
type Link struct {
	Raw      string `json:"raw"`    // link text as written
	Target   string `json:"target"` // note name or path without extension
	Heading  string `json:"heading,omitempty"`
	Block    string `json:"block,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Embed    bool   `json:"embed,omitempty"`
	Markdown bool   `json:"markdown,omitempty"` // [text](path.md) style
	Line     int    `json:"line"`               // 0-based line index in the body
	Resolved string `json:"resolved,omitempty"` // canonical path if resolvable
	Context  string `json:"context,omitempty"`
}

// ETagOf hashes content.
func ETagOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Parse builds a Note from raw content.
func Parse(rel string, content []byte) *Note {
	n := &Note{Path: rel, Content: content, ETag: ETagOf(content), Size: int64(len(content))}
	n.Frontmatter, n.Body, n.EOL = Split(content)
	lines := splitLines(n.Body)
	n.Headings = parseHeadings(lines)
	n.Links = parseLinks(lines)
	n.Tags = parseTags(n.Frontmatter, lines)
	n.Title = titleOf(n)
	return n
}

func splitLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.Split(s, "\n")
}

func titleOf(n *Note) string {
	if v := n.Frontmatter.Get("title"); v != nil && v.Value != "" {
		return v.Value
	}
	for _, h := range n.Headings {
		if h.Level == 1 {
			return h.Text
		}
	}
	return Stem(n.Path)
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)

// codeFence tracks fenced code blocks while scanning lines.
type codeFence struct {
	open   bool
	marker string
}

func (c *codeFence) feed(line string) (inCode bool) {
	t := strings.TrimSpace(line)
	if c.open {
		if strings.HasPrefix(t, c.marker) {
			c.open = false
		}
		return true
	}
	if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
		c.open = true
		c.marker = t[:3]
		return true
	}
	return false
}

func parseHeadings(lines []string) []Heading {
	var out []Heading
	var fence codeFence
	for i, l := range lines {
		if fence.feed(l) {
			continue
		}
		if m := headingRe.FindStringSubmatch(l); m != nil {
			out = append(out, Heading{Level: len(m[1]), Text: m[2], Line: i})
		}
	}
	return out
}

var (
	wikiRe = regexp.MustCompile(`(!?)\[\[([^\]\|#\^]*)(?:#([^\]\|\^]*))?(?:\^([^\]\|]*))?(?:\|([^\]]*))?\]\]`)
	mdRe   = regexp.MustCompile(`(!?)\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	tagRe  = regexp.MustCompile(`(?:^|[\s(,;])#([\p{L}\p{N}_][\p{L}\p{N}_/\-]*)`)
)

func parseLinks(lines []string) []Link {
	var out []Link
	var fence codeFence
	for i, l := range lines {
		if fence.feed(l) {
			continue
		}
		clean := stripInlineCode(l)
		for _, m := range wikiRe.FindAllStringSubmatch(clean, -1) {
			target := strings.TrimSpace(m[2])
			if target == "" && m[3] == "" {
				continue
			}
			out = append(out, Link{
				Raw: m[0], Target: strings.TrimSuffix(target, ".md"), Heading: strings.TrimSpace(m[3]),
				Block: m[4], Alias: strings.TrimSpace(m[5]), Embed: m[1] == "!", Line: i, Context: strings.TrimSpace(l),
			})
		}
		for _, m := range mdRe.FindAllStringSubmatch(clean, -1) {
			href := m[3]
			if strings.Contains(href, "://") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "#") {
				continue
			}
			href = strings.ReplaceAll(href, "%20", " ")
			frag := ""
			if h := strings.Index(href, "#"); h >= 0 {
				href, frag = href[:h], href[h+1:]
			}
			if !IsMarkdown(href) {
				continue
			}
			out = append(out, Link{Raw: m[0], Target: strings.TrimSuffix(href, ".md"), Heading: frag, Alias: m[2], Embed: m[1] == "!", Markdown: true, Line: i, Context: strings.TrimSpace(l)})
		}
	}
	return out
}

var inlineCodeRe = regexp.MustCompile("`[^`]*`")

func stripInlineCode(l string) string {
	return inlineCodeRe.ReplaceAllStringFunc(l, func(s string) string { return strings.Repeat(" ", len(s)) })
}

func parseTags(fm Frontmatter, lines []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		t = strings.TrimPrefix(strings.TrimSpace(t), "#")
		if t == "" || isNumeric(t) || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	if v := fm.Get("tags"); v != nil {
		switch {
		case len(v.Content) > 0:
			for _, c := range v.Content {
				add(c.Value)
			}
		default:
			for _, t := range strings.FieldsFunc(v.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
				add(t)
			}
		}
	}
	var fence codeFence
	for _, l := range lines {
		if fence.feed(l) {
			continue
		}
		for _, m := range tagRe.FindAllStringSubmatch(stripInlineCode(l), -1) {
			add(m[1])
		}
	}
	return out
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
