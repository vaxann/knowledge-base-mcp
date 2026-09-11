package vault

import (
	"errors"
	"strings"
)

// ErrSectionNotFound is returned when a heading does not exist.
var ErrSectionNotFound = errors.New("section not found")

// Section is the text under a heading up to the next heading of the same or
// higher level.
type Section struct {
	Heading   Heading
	StartLine int // heading line (0-based in body)
	EndLine   int // exclusive
	Text      string
}

// FindSection locates the first heading matching text (case-insensitive,
// with or without leading #'s) in a note body.
func FindSection(body []byte, heading string) (Section, error) {
	lines := splitLines(body)
	heads := parseHeadings(lines)
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(heading), "#")))
	for i, h := range heads {
		if strings.ToLower(h.Text) != want {
			continue
		}
		end := len(lines)
		for _, next := range heads[i+1:] {
			if next.Level <= h.Level {
				end = next.Line
				break
			}
		}
		body := strings.Join(lines[h.Line+1:end], "\n")
		return Section{Heading: h, StartLine: h.Line, EndLine: end, Text: body}, nil
	}
	return Section{}, ErrSectionNotFound
}

// Sections splits the body into all heading-delimited sections. Text before
// the first heading is returned as a section with an empty heading.
func Sections(body []byte) []Section {
	lines := splitLines(body)
	heads := parseHeadings(lines)
	var out []Section
	if len(heads) == 0 || heads[0].Line > 0 {
		end := len(lines)
		if len(heads) > 0 {
			end = heads[0].Line
		}
		if t := strings.TrimSpace(strings.Join(lines[:end], "\n")); t != "" {
			out = append(out, Section{EndLine: end, Text: t})
		}
	}
	for i, h := range heads {
		end := len(lines)
		if i+1 < len(heads) {
			end = heads[i+1].Line
		}
		out = append(out, Section{Heading: h, StartLine: h.Line, EndLine: end, Text: strings.Join(lines[h.Line+1:end], "\n")})
	}
	return out
}

// ReplaceSection replaces the text under heading (keeping the heading line).
func ReplaceSection(body []byte, eol, heading, content string) ([]byte, error) {
	sec, err := FindSection(body, heading)
	if err != nil {
		return nil, err
	}
	lines := splitLines(body)
	newLines := append([]string{}, lines[:sec.StartLine+1]...)
	newLines = append(newLines, "")
	newLines = append(newLines, strings.Split(strings.TrimRight(content, "\n"), "\n")...)
	newLines = append(newLines, "")
	newLines = append(newLines, lines[sec.EndLine:]...)
	return []byte(strings.Join(newLines, eol)), nil
}

// InsertAfterHeading inserts content right after the heading line.
func InsertAfterHeading(body []byte, eol, heading, content string) ([]byte, error) {
	sec, err := FindSection(body, heading)
	if err != nil {
		return nil, err
	}
	lines := splitLines(body)
	newLines := append([]string{}, lines[:sec.StartLine+1]...)
	newLines = append(newLines, "")
	newLines = append(newLines, strings.Split(strings.TrimRight(content, "\n"), "\n")...)
	newLines = append(newLines, lines[sec.StartLine+1:]...)
	return []byte(strings.Join(newLines, eol)), nil
}
