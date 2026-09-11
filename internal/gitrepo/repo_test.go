package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

func open(t *testing.T, dir string) *Repo {
	t.Helper()
	r := New(dir, Options{AuthorName: "KB Test", AuthorEmail: "kb@test.local"})
	if !r.IsWorkTree(context.Background()) {
		t.Fatalf("%s is not a work tree", dir)
	}
	return r
}

func TestCommitAndLog(t *testing.T) {
	ctx := context.Background()
	dir := testutil.NewVaultRepo(t)
	r := open(t, dir)
	if b, _ := r.CurrentBranch(ctx); b != "main" {
		t.Fatalf("branch %q", b)
	}
	if err := os.WriteFile(filepath.Join(dir, "Notes", "New.md"), []byte("# New\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Stage(ctx, "Notes/New.md"); err != nil {
		t.Fatal(err)
	}
	hash, err := r.Commit(ctx, "kb_create_note: Notes/New.md\n\nKB-Client: test")
	if err != nil || len(hash) != 40 {
		t.Fatalf("commit: %v %q", err, hash)
	}
	commits, err := r.Log(ctx, LogOptions{Limit: 1})
	if err != nil || len(commits) != 1 {
		t.Fatalf("log: %v %+v", err, commits)
	}
	c := commits[0]
	if c.Hash != hash || c.Author != "KB Test" || !strings.HasPrefix(c.Message, "kb_create_note: Notes/New.md") || !strings.Contains(c.Message, "KB-Client: test") {
		t.Errorf("commit fields: %+v", c)
	}
	if len(c.Changes) != 1 || c.Changes[0].Status != "A" || c.Changes[0].Path != "Notes/New.md" {
		t.Errorf("changes: %+v", c.Changes)
	}
	if dirty, _ := r.IsDirty(ctx); dirty {
		t.Error("tree should be clean after commit")
	}
}

func TestHistoryOfDeletedAndRenamed(t *testing.T) {
	ctx := context.Background()
	dir := testutil.NewVaultRepo(t)
	r := open(t, dir)
	hist, err := r.Log(ctx, LogOptions{Path: "Inbox/Draft.md"})
	if err != nil || len(hist) != 2 {
		t.Fatalf("history of deleted note: %v %d", err, len(hist))
	}
	if hist[0].Changes[0].Status != "D" {
		t.Errorf("newest change should be D: %+v", hist[0].Changes)
	}
	data, err := r.Show(ctx, "HEAD~1", "Inbox/Draft.md")
	if err != nil || !strings.Contains(string(data), "will be deleted") {
		t.Errorf("show deleted note: %v %q", err, data)
	}
	if _, err := r.Show(ctx, "HEAD", "Inbox/Draft.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("show at HEAD should be not_found, got %v", err)
	}
	entries, err := r.LsTree(ctx, "HEAD~1", "Inbox")
	if err != nil || len(entries) != 1 || entries[0].Path != "Inbox/Draft.md" || entries[0].Kind != "note" {
		t.Errorf("ls-tree: %v %+v", err, entries)
	}
	root, err := r.LsTree(ctx, "HEAD", "")
	if err != nil || len(root) < 5 {
		t.Errorf("root ls-tree: %v %+v", err, root)
	}
	// rename with --follow
	testutil.Git(t, dir, "mv", "Notes/Meeting.md", "Notes/Renamed.md")
	testutil.Git(t, dir, "commit", "-q", "-m", "rename")
	hist, err = r.Log(ctx, LogOptions{Path: "Notes/Renamed.md"})
	if err != nil || len(hist) != 2 {
		t.Fatalf("follow rename: %v %d", err, len(hist))
	}
	if hist[0].Changes[0].Status != "R" || hist[0].Changes[0].OldPath != "Notes/Meeting.md" {
		t.Errorf("rename change: %+v", hist[0].Changes)
	}
	if _, err := r.RevParse(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown rev: %v", err)
	}
	diff, err := r.Diff(ctx, "HEAD~3", "HEAD~2", "")
	if err != nil || !strings.Contains(diff, "+++ b/Inbox/Draft.md") {
		t.Errorf("diff: %v %q", err, diff)
	}
	changes, err := r.DiffNameStatus(ctx, "HEAD~1", "HEAD")
	if err != nil || len(changes) != 1 || changes[0].Status != "R" || changes[0].OldPath != "Notes/Meeting.md" {
		t.Errorf("name-status: %v %+v", err, changes)
	}
}

func TestMergeConflictAndPush(t *testing.T) {
	ctx := context.Background()
	vault, remote := testutil.NewVaultWithRemote(t)
	r := open(t, vault)
	other := testutil.CloneRemote(t, remote)
	testutil.WriteAndPush(t, other, "Notes/Meeting.md", "# Meeting\n\nremote version\n", "remote edit")

	if err := os.WriteFile(filepath.Join(vault, "Notes", "Meeting.md"), []byte("# Meeting\n\nlocal version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Stage(ctx, "Notes/Meeting.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, "local edit"); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(ctx, "origin", "main"); !errors.Is(err, ErrPushRejected) {
		t.Fatalf("expected push rejection, got %v", err)
	}
	if err := r.Fetch(ctx, "origin"); err != nil {
		t.Fatal(err)
	}
	ahead, behind, err := r.AheadBehind(ctx, "origin", "main")
	if err != nil || ahead != 1 || behind != 1 {
		t.Fatalf("ahead/behind = %d/%d, %v", ahead, behind, err)
	}
	conflicted, err := r.Merge(ctx, "origin/main")
	if err != nil || !conflicted {
		t.Fatalf("expected conflict, got conflicted=%v err=%v", conflicted, err)
	}
	if !r.MergeInProgress(ctx) {
		t.Fatal("merge should be in progress")
	}
	un, err := r.UnmergedFiles(ctx)
	if err != nil || len(un) != 1 || un[0].Path != "Notes/Meeting.md" || un[0].Kind() != "content" {
		t.Fatalf("unmerged: %v %+v", err, un)
	}
	ours, ok, _ := r.StageContent(ctx, 2, "Notes/Meeting.md")
	theirs, ok2, _ := r.StageContent(ctx, 3, "Notes/Meeting.md")
	if !ok || !ok2 || !strings.Contains(string(ours), "local") || !strings.Contains(string(theirs), "remote") {
		t.Errorf("stages: %q %q", ours, theirs)
	}
	wt, _ := os.ReadFile(filepath.Join(vault, "Notes", "Meeting.md"))
	if !strings.Contains(string(wt), "<<<<<<<") {
		t.Error("work tree should contain markers")
	}
	// Resolve and complete the merge.
	if err := os.WriteFile(filepath.Join(vault, "Notes", "Meeting.md"), []byte("# Meeting\n\nmerged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Stage(ctx, "Notes/Meeting.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, "merge: origin/main (resolved: Notes/Meeting.md)"); err != nil {
		t.Fatal(err)
	}
	if r.MergeInProgress(ctx) {
		t.Fatal("merge should be complete")
	}
	if err := r.Push(ctx, "origin", "main"); err != nil {
		t.Fatalf("push after merge: %v", err)
	}
	ahead, behind, _ = r.AheadBehind(ctx, "origin", "main")
	if ahead != 0 || behind != 0 {
		t.Errorf("after push ahead/behind = %d/%d", ahead, behind)
	}
}

func TestModifyDeleteConflict(t *testing.T) {
	ctx := context.Background()
	vault, remote := testutil.NewVaultWithRemote(t)
	r := open(t, vault)
	other := testutil.CloneRemote(t, remote)
	testutil.Git(t, other, "rm", "-q", "Notes/Meeting.md")
	testutil.Git(t, other, "commit", "-q", "-m", "remote delete")
	testutil.Git(t, other, "push", "-q", "origin", "main")
	if err := os.WriteFile(filepath.Join(vault, "Notes", "Meeting.md"), []byte("# Meeting\n\nlocal edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = r.Stage(ctx, "Notes/Meeting.md")
	if _, err := r.Commit(ctx, "local edit"); err != nil {
		t.Fatal(err)
	}
	_ = r.Fetch(ctx, "origin")
	conflicted, err := r.Merge(ctx, "origin/main")
	if err != nil || !conflicted {
		t.Fatalf("expected conflict: %v %v", conflicted, err)
	}
	un, _ := r.UnmergedFiles(ctx)
	if len(un) != 1 || un[0].Kind() != "modify/delete" {
		t.Fatalf("unmerged: %+v", un)
	}
	if _, ok, _ := r.StageContent(ctx, 3, "Notes/Meeting.md"); ok {
		t.Error("theirs stage should be absent for a remote delete")
	}
	if err := r.RemoveFromIndexAndTree(ctx, "Notes/Meeting.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, "merge: resolved by delete"); err != nil {
		t.Fatal(err)
	}
	if r.MergeInProgress(ctx) {
		t.Error("merge should be complete")
	}
}

func TestTokenHelperEnv(t *testing.T) {
	r := New(t.TempDir(), Options{Token: "s3cret", Username: "bot"})
	joined := strings.Join(r.env, "\n")
	if !strings.Contains(joined, "KB_GIT_HELPER_TOKEN=s3cret") || !strings.Contains(joined, "credential.helper") {
		t.Error("credential helper not configured")
	}
	for _, line := range r.env {
		if strings.HasPrefix(line, "GIT_CONFIG_VALUE_") && strings.Contains(line, "s3cret") {
			t.Error("token must not appear in git config values")
		}
	}
	if !strings.Contains(joined, "KB_GIT_HELPER_USER=bot") {
		t.Error("username not passed")
	}
}

func TestCloneAndRedact(t *testing.T) {
	ctx := context.Background()
	_, remote := testutil.NewVaultWithRemote(t)
	dst := filepath.Join(t.TempDir(), "clone", "vault")
	if err := Clone(ctx, remote, dst, "main", Options{}); err != nil {
		t.Fatal(err)
	}
	if !New(dst, Options{}).IsWorkTree(ctx) {
		t.Error("clone is not a work tree")
	}
	if got := redact("fatal: https://user:token@example.com/x failed"); strings.Contains(got, "token") {
		t.Errorf("redact failed: %s", got)
	}
}
