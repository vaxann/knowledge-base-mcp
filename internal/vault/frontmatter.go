package vault

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is an order-preserving YAML header.
type Frontmatter struct {
	// Node is the mapping node of the header; nil when absent or invalid.
	Node *yaml.Node
	// Raw is the header text between the --- fences (without them).
	Raw string
	// Err is set when Raw could not be parsed as a YAML mapping.
	Err error
	// Present reports whether a --- block was found at all.
	Present bool
}

// Split separates a note into frontmatter and body. The body keeps its
// original bytes. Line endings are detected from the content.
func Split(content []byte) (fm Frontmatter, body []byte, eol string) {
	eol = "\n"
	if bytes.Contains(content, []byte("\r\n")) {
		eol = "\r\n"
	}
	text := string(content)
	if !strings.HasPrefix(text, "---") {
		return fm, content, eol
	}
	rest := text[3:]
	// The opening fence must be alone on its line.
	nl := strings.Index(rest, "\n")
	if nl < 0 || strings.TrimSpace(rest[:nl]) != "" {
		return fm, content, eol
	}
	rest = rest[nl+1:]
	end := findClosingFence(rest)
	if end < 0 {
		return fm, content, eol
	}
	fm.Present = true
	fm.Raw = rest[:end]
	after := rest[end:]
	// Skip the closing fence line.
	if nl := strings.Index(after, "\n"); nl >= 0 {
		after = after[nl+1:]
	} else {
		after = ""
	}
	body = []byte(after)
	fm.Node, fm.Err = parseMapping(fm.Raw)
	return fm, body, eol
}

func findClosingFence(s string) int {
	off := 0
	for {
		nl := strings.Index(s[off:], "\n")
		line := s[off:]
		if nl >= 0 {
			line = s[off : off+nl]
		}
		t := strings.TrimRight(line, "\r")
		if t == "---" || t == "..." {
			return off
		}
		if nl < 0 {
			return -1
		}
		off += nl + 1
	}
}

func parseMapping(raw string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		// Empty header: treat as empty mapping.
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("frontmatter must be a YAML mapping, got %s at line %d", kindName(m.Kind), m.Line)
	}
	return m, nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "sequence"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.MappingNode:
		return "mapping"
	default:
		return "document"
	}
}

// Map decodes the frontmatter into a generic map (nil if absent/invalid).
func (f Frontmatter) Map() map[string]any {
	if f.Node == nil {
		return nil
	}
	out := map[string]any{}
	if err := f.Node.Decode(&out); err != nil {
		return nil
	}
	return out
}

// Get returns the value node for key, or nil.
func (f Frontmatter) Get(key string) *yaml.Node {
	if f.Node == nil {
		return nil
	}
	for i := 0; i+1 < len(f.Node.Content); i += 2 {
		if f.Node.Content[i].Value == key {
			return f.Node.Content[i+1]
		}
	}
	return nil
}

// Set replaces or appends key with value (any Go value), preserving the
// position and formatting of all other keys.
func (f *Frontmatter) Set(key string, value any) error {
	if f.Node == nil {
		if f.Err != nil {
			return f.Err
		}
		f.Node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		f.Present = true
	}
	var v yaml.Node
	if err := v.Encode(value); err != nil {
		return err
	}
	quoteAmbiguous(&v)
	for i := 0; i+1 < len(f.Node.Content); i += 2 {
		if f.Node.Content[i].Value == key {
			old := f.Node.Content[i+1]
			// Keep comments attached to the old value.
			v.HeadComment, v.LineComment, v.FootComment = old.HeadComment, old.LineComment, old.FootComment
			f.Node.Content[i+1] = &v
			return nil
		}
	}
	f.Node.Content = append(f.Node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &v)
	return nil
}

// Remove deletes key; returns whether it existed.
func (f *Frontmatter) Remove(key string) bool {
	if f.Node == nil {
		return false
	}
	for i := 0; i+1 < len(f.Node.Content); i += 2 {
		if f.Node.Content[i].Value == key {
			f.Node.Content = append(f.Node.Content[:i], f.Node.Content[i+2:]...)
			return true
		}
	}
	return false
}

// quoteAmbiguous forces double-quoted style on scalars that YAML would
// otherwise misread (yaml.v3 already handles most, this covers editor quirks).
func quoteAmbiguous(n *yaml.Node) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!str" && n.Style == 0 && needsQuotes(n.Value) {
			n.Style = yaml.DoubleQuotedStyle
		}
	case yaml.SequenceNode, yaml.MappingNode:
		for _, c := range n.Content {
			quoteAmbiguous(c)
		}
	}
}

func needsQuotes(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return true
	}
	switch s[0] {
	case '*', '&', '!', '|', '>', '%', '@', '`', '[', ']', '{', '}', ',', '#', '?', '-', ':':
		return true
	}
	return false
}

// Encode renders the header text (without fences), or "" when absent.
// If the header was not modified it is returned verbatim.
func (f Frontmatter) Encode() (string, error) {
	if f.Node == nil {
		if f.Err != nil {
			return f.Raw, nil
		}
		return "", nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if len(f.Node.Content) == 0 {
		return "", nil
	}
	if err := enc.Encode(f.Node); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Join assembles a note from header text (without fences), body and eol.
func Join(header string, body []byte, eol string) []byte {
	var out bytes.Buffer
	if header != "" {
		header = strings.ReplaceAll(header, "\r\n", "\n")
		if !strings.HasSuffix(header, "\n") {
			header += "\n"
		}
		out.WriteString("---" + eol)
		out.WriteString(strings.ReplaceAll(header, "\n", eol))
		out.WriteString("---" + eol)
	}
	out.Write(body)
	return out.Bytes()
}

// Serialize writes back a note whose frontmatter may have been edited. When
// the frontmatter node is untouched, callers should pass the original header
// text to preserve it byte-for-byte; this helper always re-encodes.
func Serialize(fm Frontmatter, body []byte, eol string) ([]byte, error) {
	header, err := fm.Encode()
	if err != nil {
		return nil, err
	}
	return Join(header, body, eol), nil
}

// ValidateFrontmatter parses raw content and reports a YAML problem, if any.
func ValidateFrontmatter(content []byte) error {
	fm, _, _ := Split(content)
	if fm.Err != nil {
		return fmt.Errorf("invalid_frontmatter: %w", fm.Err)
	}
	return nil
}

// ErrNoFrontmatter is returned by frontmatter patch operations on notes
// whose header cannot be parsed.
var ErrNoFrontmatter = errors.New("frontmatter cannot be parsed")
