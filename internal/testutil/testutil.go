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
