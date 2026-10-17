// Package testutil builds temporary Git repositories from the fixture vault.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// FixtureDir returns the absolute path of testdata/vault.
func FixtureDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil source")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "vault")
}

// Git runs git in dir and fails the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := GitOutput(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// GitOutput runs git in dir and returns combined output.
func GitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+os.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CopyFixture copies the fixture vault into a fresh temp dir (no git).
func CopyFixture(t testing.TB) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "vault")
	src := FixtureDir(t)
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644) //nolint:gosec // fixture copy inside a temp dir
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// NewVaultRepo copies the fixture into a temp dir, initialises a Git repo on
// branch main, adds history containing a note that was created and then
// deleted (Inbox/Draft.md), and returns the work tree path.
func NewVaultRepo(t testing.TB) string {
	t.Helper()
	dir := CopyFixture(t)
	Git(t, dir, "init", "-q", "-b", "main")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "-m", "fixture: initial vault")
	draft := filepath.Join(dir, "Inbox", "Draft.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte("# Draft\n\nA note that will be deleted.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "-m", "fixture: add draft")
	Git(t, dir, "rm", "-q", "Inbox/Draft.md")
	Git(t, dir, "commit", "-q", "-m", "fixture: delete draft")
	return dir
}

// NewVaultWithRemote creates a bare remote, a vault clone tracking it and
// returns (vaultDir, remoteDir). The vault has the same history as NewVaultRepo.
func NewVaultWithRemote(t testing.TB) (vault, remote string) {
	t.Helper()
	src := NewVaultRepo(t)
	remote = filepath.Join(t.TempDir(), "remote.git")
	Git(t, src, "init", "-q", "--bare", "-b", "main", remote)
	Git(t, src, "remote", "add", "origin", remote)
	Git(t, src, "push", "-q", "-u", "origin", "main")
	return src, remote
}

// CloneRemote clones a bare remote into a fresh temp dir (a "competing clone").
func CloneRemote(t testing.TB, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	Git(t, filepath.Dir(dir), "clone", "-q", "-b", "main", remote, dir)
	return dir
}

// WriteAndPush writes a file in a clone, commits and pushes it.
func WriteAndPush(t testing.TB, clone, rel, content, msg string) {
	t.Helper()
	p := filepath.Join(clone, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, clone, "add", "-A")
	Git(t, clone, "commit", "-q", "-m", msg)
	Git(t, clone, "push", "-q", "origin", "main")
}

// Access-rule fixture. Everything a restricted account must never see carries
// one of these tokens: HiddenToken in every hidden file name, folder name and
// commit message, HiddenMarker in every hidden file's content.
const (
	HiddenToken  = "hidn"
	HiddenMarker = "QUOKKA-MARKER-7731"
)

func writeFile(t testing.TB, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// NewAccessRepo builds a synthetic repository for access-rule tests, with a
// bare remote, and returns (workTree, remote).
//
// The rules the tests give a restricted account are: write on Open, none on
// Open/hidn-closed, read on Shared/Team and read on ReadOnly. Everything
// else (hidn-private, Shared/hidn-other, Shared/hidn-root.md) is hidden.
//
// History, oldest first: the initial layout (touches hidden paths), a commit
// that touches only hidden paths, a mixed commit, a commit that touches only
// readable paths, a hidden note that is then moved into Open, and an edit of
// the moved note.
func NewAccessRepo(t testing.TB) (workTree, remote string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "access")
	m := HiddenMarker
	files := map[string]string{
		"Open/Note.md":                  "---\ntype: note\ntags: [open]\n---\n# Open note\n\nPublic zebra text. See [[Target]], [[Plan]] and [[Doc]].\n",
		"Open/Sub/Deep.md":              "# Deep\n\nA deep zebra note. #open\n",
		"Open/file.bin":                 "open-bytes",
		"Open/hidn-closed/hidn-note.md": "# Hidn closed\n\n" + m + " zebra. #hidnonly See [[Note]].\n",
		"hidn-private/hidn-secret.md":   "---\ntype: hidnkind\n---\n# Hidn secret\n\n" + m + " zebra budget. See [[Note]].\n",
		// Target is linked from the readable note by its bare name, so only its
		// folder carries the token.
		"hidn-private/Target.md":           "# Target\n\n" + m + "\n",
		"hidn-private/hidn-blob.bin":       m,
		"hidn-private/hidn-deep/hidn-x.md": "# Hidn x\n\n" + m + "\n",
		"Shared/Team/Plan.md":              "# Plan\n\nTeam plan zebra.\n",
		"Shared/hidn-other/hidn-else.md":   "# Hidn else\n\n" + m + " zebra\n",
		"Shared/hidn-root.md":              "# Hidn root\n\n" + m + " zebra\n",
		"ReadOnly/Doc.md":                  "# Doc\n\nRead only zebra.\n",
	}
	for rel, content := range files {
		writeFile(t, dir, rel, content)
	}
	Git(t, dir, "init", "-q", "-b", "main")
	commit := func(msg string) {
		Git(t, dir, "add", "-A")
		Git(t, dir, "commit", "-q", "-m", msg)
	}
	commit("fixture: initial " + HiddenToken + " layout")
	writeFile(t, dir, "hidn-private/hidn-secret.md", files["hidn-private/hidn-secret.md"]+"\nMore "+m+".\n")
	commit(HiddenToken + ": edit the secret note")
	writeFile(t, dir, "Open/Note.md", files["Open/Note.md"]+"\nMixed edit.\n")
	writeFile(t, dir, "hidn-private/hidn-secret.md", files["hidn-private/hidn-secret.md"]+"\nMixed "+m+".\n")
	commit("mixed commit mentioning " + HiddenToken + "-secret")
	writeFile(t, dir, "Open/Note.md", files["Open/Note.md"]+"\nMixed edit.\nOpen edit.\n")
	commit("edit the open note")
	writeFile(t, dir, "hidn-private/hidn-moved.md", "# Moved\n\nA note that used to live elsewhere.\n")
	commit(HiddenToken + ": add a note to move")
	Git(t, dir, "mv", "hidn-private/hidn-moved.md", "Open/Moved.md")
	Git(t, dir, "commit", "-q", "-m", HiddenToken+": move the note out")
	writeFile(t, dir, "Open/Moved.md", "# Moved\n\nA note that used to live elsewhere.\n\nEdited after the move.\n")
	commit("edit the moved note")
	remote = filepath.Join(t.TempDir(), "access.git")
	Git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	Git(t, dir, "remote", "add", "origin", remote)
	Git(t, dir, "push", "-q", "-u", "origin", "main")
	return dir, remote
}
