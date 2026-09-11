package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

func TestCleanRel(t *testing.T) {
	good := map[string]string{"a/b.md": "a/b.md", "./a/./b.md": "a/b.md", "": "", ".": "", "a\\b.md": "a/b.md", "a/": "a"}
	for in, want := range good {
		got, err := CleanRel(in)
		if err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../x.md", "./a/../b.md", "/etc/passwd", "a/../../x", "C:\\x", "a\x00b"} {
		if _, err := CleanRel(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanRel(%q) should fail with invalid_path, got %v", bad, err)
		}
	}
}

func TestExcluder(t *testing.T) {
	ex, err := NewExcluder([]string{"Private/**", "*.log"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"":                   false,
		"Notes/x.md":         false,
		".obsidian/app.json": true,
		".git/HEAD":          true,
		"Notes/.hidden.md":   true,
		"Private":            true,
		"Private/secret.md":  true,
		"debug.log":          true,
	}
	for p, want := range cases {
		if got := ex.Excluded(p); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", p, got, want)
		}
	}
	if _, err := NewExcluder([]string{"[bad"}); err == nil {
		t.Error("invalid pattern should be rejected")
	}
}

func TestSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "o.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unsupported")
	}
	v, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Check("link/o.md"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("symlink escape must be rejected, got %v", err)
	}
	if _, _, err := v.Check("link/new.md"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("new file under escaping symlink must be rejected, got %v", err)
	}
	if _, _, err := v.Check("fresh/new.md"); err != nil {
		t.Errorf("new file inside vault must be allowed: %v", err)
	}
}

func openFixture(t *testing.T) *Vault {
	t.Helper()
	v, err := Open(testutil.CopyFixture(t), []string{"Private/**"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReadNoteAndParse(t *testing.T) {
	v := openFixture(t)
	n, err := v.ReadNote("Projects/Alpha.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "Alpha project" {
		t.Errorf("title = %q", n.Title)
	}
	fm := n.Frontmatter.Map()
	if fm["type"] != "project" || fm["priority"] != 3 {
		t.Errorf("frontmatter = %v", fm)
	}
	wantTags := []string{"project", "alpha"}
	if strings.Join(n.Tags, ",") != strings.Join(wantTags, ",") {
		t.Errorf("tags = %v", n.Tags)
	}
	var targets []string
	for _, l := range n.Links {
		targets = append(targets, l.Target)
	}
	joined := strings.Join(targets, "|")
	for _, want := range []string{"Beta", "Notes/Meeting", "Work/Meeting", "Alpha", "Budget 2026", "../Notes/Meeting"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing link target %q in %v", want, targets)
		}
	}
	if strings.Contains(joined, "example.com") {
		t.Error("external link must be ignored")
	}
	if len(n.Headings) != 5 || n.Headings[1].Text != "Log" || n.Headings[3].Level != 3 {
		t.Errorf("headings = %+v", n.Headings)
	}
}

func TestCodeBlocksIgnored(t *testing.T) {
	v := openFixture(t)
	n, err := v.ReadNote("Projects/Beta.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Links) != 1 || n.Links[0].Target != "Alpha" {
		t.Errorf("links inside code must be ignored: %+v", n.Links)
	}
	for _, tag := range n.Tags {
		if tag == "tag" {
			t.Error("tag inside code block must be ignored")
		}
	}
	if strings.Join(n.Tags, ",") != "project" {
		t.Errorf("tags = %v", n.Tags)
	}
}

func TestInvalidFrontmatterStillReadable(t *testing.T) {
	v := openFixture(t)
	n, err := v.ReadNote("Ideas/Invalid.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.Frontmatter.Err == nil {
		t.Fatal("expected frontmatter error")
	}
	if !strings.Contains(string(n.Body), "Invalid frontmatter") {
		t.Errorf("body lost: %q", n.Body)
	}
	if err := n.Frontmatter.Set("x", 1); err == nil {
		t.Error("Set on unparseable frontmatter must fail")
	}
}

func TestExcludedAndAttachments(t *testing.T) {
	v := openFixture(t)
	if _, err := v.ReadNote("Private/secret.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("excluded note must be not_found, got %v", err)
	}
	if _, err := v.ReadNote(".obsidian/app.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dot folder must be not_found, got %v", err)
	}
	entries, err := v.List("", false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Path)
	}
	joined := strings.Join(names, ",")
	if strings.Contains(joined, ".obsidian") || strings.Contains(joined, "Private") {
		t.Errorf("excluded entries listed: %v", names)
	}
	files, err := v.List("Files", false)
	if err != nil || len(files) != 1 || files[0].Kind != "attachment" || files[0].Size == 0 {
		t.Errorf("attachment listing: %+v, %v", files, err)
	}
	all, err := v.List("", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 9 {
		t.Errorf("recursive listing has %d entries: %v", len(all), all)
	}
}

func TestFrontmatterRoundTrip(t *testing.T) {
	v := openFixture(t)
	n, err := v.ReadNote("Projects/Alpha.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Frontmatter.Set("status", "done"); err != nil {
		t.Fatal(err)
	}
	out, err := Serialize(n.Frontmatter, n.Body, n.EOL)
	if err != nil {
		t.Fatal(err)
	}
	orig := strings.Split(string(n.Content), "\n")
	got := strings.Split(string(out), "\n")
	if len(orig) != len(got) {
		t.Fatalf("line count changed: %d -> %d\n%s", len(orig), len(got), out)
	}
	diff := 0
	for i := range orig {
		if orig[i] != got[i] {
			diff++
			if got[i] != "status: done" {
				t.Errorf("unexpected change at line %d: %q -> %q", i, orig[i], got[i])
			}
		}
	}
	if diff != 1 {
		t.Errorf("expected exactly one changed line, got %d", diff)
	}
	if !strings.Contains(string(out), "# a comment that must survive") {
		t.Error("comment lost")
	}
	if !strings.HasSuffix(string(out), string(n.Body)) {
		t.Error("body changed")
	}
}

func TestFrontmatterQuoting(t *testing.T) {
	var fm Frontmatter
	if err := fm.Set("url", "https://example.com/a: b"); err != nil {
		t.Fatal(err)
	}
	if err := fm.Set("phone", "+1 555: 0100"); err != nil {
		t.Fatal(err)
	}
	if err := fm.Set("flag", "yes"); err != nil {
		t.Fatal(err)
	}
	if err := fm.Set("list", []string{"a: b", "c"}); err != nil {
		t.Fatal(err)
	}
	out, err := Serialize(fm, []byte("body\n"), "\n")
	if err != nil {
		t.Fatal(err)
	}
	back := Parse("x.md", out)
	if back.Frontmatter.Err != nil {
		t.Fatalf("re-parse failed: %v\n%s", back.Frontmatter.Err, out)
	}
	m := back.Frontmatter.Map()
	if m["url"] != "https://example.com/a: b" || m["phone"] != "+1 555: 0100" || m["flag"] != "yes" {
		t.Errorf("values did not round-trip: %v\n%s", m, out)
	}
}

func TestCRLFPreserved(t *testing.T) {
	content := []byte("---\r\ntitle: X\r\n---\r\n# X\r\n\r\nbody\r\n")
	n := Parse("x.md", content)
	if n.EOL != "\r\n" || n.Title != "X" {
		t.Fatalf("eol=%q title=%q", n.EOL, n.Title)
	}
	if err := n.Frontmatter.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	out, _ := Serialize(n.Frontmatter, n.Body, n.EOL)
	if strings.Contains(strings.ReplaceAll(string(out), "\r\n", ""), "\n") {
		t.Errorf("mixed line endings:\n%q", out)
	}
}

func TestSections(t *testing.T) {
	v := openFixture(t)
	n, _ := v.ReadNote("Projects/Alpha.md")
	sec, err := FindSection(n.Body, "## Budget")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sec.Text, "Budget 2026") || !strings.Contains(sec.Text, "### Details") || strings.Contains(sec.Text, "Plain notes") {
		t.Errorf("section text wrong:\n%s", sec.Text)
	}
	if _, err := FindSection(n.Body, "Nope"); !errors.Is(err, ErrSectionNotFound) {
		t.Errorf("expected not found, got %v", err)
	}
	out, err := ReplaceSection(n.Body, "\n", "Log", "new text")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "## Log\n\nnew text\n\n## Budget") || strings.Contains(s, "kicked off") {
		t.Errorf("replace failed:\n%s", s)
	}
	out, err = InsertAfterHeading(n.Body, "\n", "Notes", "inserted")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "## Notes\n\ninserted\n\nPlain notes") {
		t.Errorf("insert failed:\n%s", out)
	}
	all := Sections(n.Body)
	if len(all) != 5 || all[0].Heading.Text != "Alpha" {
		t.Errorf("sections = %d", len(all))
	}
}

func TestResolver(t *testing.T) {
	r := NewResolver([]string{"Projects/Alpha.md", "Notes/Meeting.md", "Work/Meeting.md", "Ideas/Budget 2026.md"})
	cases := map[string]string{
		"Alpha":            "Projects/Alpha.md",
		"alpha":            "Projects/Alpha.md",
		"Work/Meeting":     "Work/Meeting.md",
		"Meeting":          "Work/Meeting.md", // shortest path wins
		"Budget 2026":      "Ideas/Budget 2026.md",
		"Missing":          "",
		"../Notes/Meeting": "Notes/Meeting.md",
	}
	for in, want := range cases {
		if got := r.Resolve("Projects/Alpha.md", in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteAtomicAndRename(t *testing.T) {
	v := openFixture(t)
	if err := v.WriteAtomic("New/Deep/note.md", []byte("# New\n")); err != nil {
		t.Fatal(err)
	}
	if !v.Exists("New/Deep/note.md") {
		t.Fatal("file missing after write")
	}
	if err := v.Rename("New/Deep/note.md", "Moved/note.md"); err != nil {
		t.Fatal(err)
	}
	if v.Exists("New/Deep/note.md") || !v.Exists("Moved/note.md") {
		t.Error("rename failed")
	}
	if err := v.Remove("Moved/note.md"); err != nil {
		t.Fatal(err)
	}
	if err := v.Remove("Moved/note.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second remove should be not_found, got %v", err)
	}
	if err := v.WriteAtomic("../escape.md", []byte("x")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("traversal must fail, got %v", err)
	}
	entries, _ := filepath.Glob(filepath.Join(v.Root, "New", "Deep", ".kb-*"))
	if len(entries) != 0 {
		t.Error("temp files left behind")
	}
}
