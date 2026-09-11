package kb

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

func testConfig(t *testing.T, vaultDir string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Vault.Path = vaultDir
	cfg.Vault.Exclude = []string{"Private/**"}
	cfg.Search.IndexDir = filepath.Join(t.TempDir(), "index")
	cfg.Git.PushDebounce = 0
	cfg.Git.PullInterval = 0
	cfg.Git.PullFreshness = 0
	cfg.Git.InstanceID = "test-instance"
	return cfg
}

func openService(t *testing.T, cfg config.Config) *Service {
	t.Helper()
	s, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "<nil>"
}

func TestOpenValidation(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, filepath.Join(t.TempDir(), "missing"))
	if _, err := Open(ctx, cfg, nil, "t"); err == nil || !strings.Contains(err.Error(), "KB_GIT_REMOTE") {
		t.Errorf("missing path: %v", err)
	}
	plain := testutil.CopyFixture(t)
	cfg = testConfig(t, plain)
	if _, err := Open(ctx, cfg, nil, "t"); err == nil || !strings.Contains(err.Error(), "Git clone") {
		t.Errorf("non-git: %v", err)
	}
	repo := testutil.NewVaultRepo(t)
	testutil.Git(t, repo, "checkout", "-q", "-b", "experiment")
	cfg = testConfig(t, repo)
	if _, err := Open(ctx, cfg, nil, "t"); err == nil || !strings.Contains(err.Error(), `"main"`) {
		t.Errorf("wrong branch: %v", err)
	}
}

func TestBootstrapClone(t *testing.T) {
	_, remote := testutil.NewVaultWithRemote(t)
	dir := filepath.Join(t.TempDir(), "data", "vault")
	cfg := testConfig(t, dir)
	cfg.Vault.Remote = remote
	s := openService(t, cfg)
	info := s.GetInfo(context.Background())
	if info.NoteCount != 8 || info.Remote != "origin" {
		t.Errorf("info after clone: %+v", info)
	}
}

func TestReadTools(t *testing.T) {
	ctx := context.Background()
	s := openService(t, testConfig(t, testutil.NewVaultRepo(t)))
	n, err := s.GetNote(ctx, "Projects/Alpha.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "Alpha project" || n.LastCommit == nil || n.ETag == "" || n.Frontmatter["type"] != "project" {
		t.Errorf("note view: %+v", n)
	}
	var resolved int
	for _, l := range n.Links {
		if l.Resolved != "" {
			resolved++
		}
	}
	if resolved < 5 {
		t.Errorf("links not resolved: %+v", n.Links)
	}
	if _, err := s.GetNote(ctx, "Nope.md"); code(err) != CodeNotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := s.GetNote(ctx, "../x.md"); code(err) != CodeInvalidPath {
		t.Errorf("traversal: %v", err)
	}
	sec, err := s.GetSection(ctx, "Projects/Alpha.md", "log")
	if err != nil || !strings.Contains(sec.Text, "kicked off") || sec.Level != 2 {
		t.Errorf("section: %+v %v", sec, err)
	}
	if _, err := s.GetSection(ctx, "Projects/Alpha.md", "Nope"); code(err) != CodeNotFound {
		t.Errorf("missing section: %v", err)
	}
	page, err := s.List(ctx, ListRequest{Recursive: true, Glob: "**/*.md", Limit: 3})
	if err != nil || len(page.Entries) != 3 || page.NextCursor == "" {
		t.Fatalf("list page 1: %+v %v", page, err)
	}
	page2, _ := s.List(ctx, ListRequest{Recursive: true, Glob: "**/*.md", Limit: 10, Cursor: page.NextCursor})
	if len(page2.Entries) != 5 || page2.NextCursor != "" || page2.Entries[0].Path <= page.Entries[2].Path {
		t.Errorf("list page 2: %+v", page2)
	}
	if _, err := s.List(ctx, ListRequest{Folder: "Private"}); code(err) != CodeNotFound {
		t.Errorf("excluded folder: %v", err)
	}
	bl, err := s.Backlinks(ctx, "Alpha")
	if err != nil || len(bl) != 4 {
		t.Errorf("backlinks by name: %+v %v", bl, err)
	}
	if tags := s.Tags(ctx, ""); tags[0].Tag != "project" {
		t.Errorf("tags: %+v", tags)
	}
	res, _ := s.Search(ctx, search.Request{Query: "паспорт"})
	if len(res.Hits) != 1 {
		t.Errorf("search: %+v", res)
	}
	g, _ := s.Grep(ctx, search.GrepRequest{Pattern: "[[Beta"})
	if len(g.Files) != 2 {
		t.Errorf("grep: %+v", g)
	}
	rows, _ := s.Query(ctx, search.QueryRequest{Where: `type eq "contact"`})
	if len(rows) != 1 {
		t.Errorf("query: %+v", rows)
	}
	if _, err := s.Query(ctx, search.QueryRequest{Where: `type ??? "x"`}); code(err) != CodeInvalidArgument {
		t.Errorf("bad query: %v", err)
	}
	c, _ := s.Context(ctx, search.ContextRequest{Query: "budget", MaxChars: 500})
	if c.Chunks == 0 {
		t.Error("empty context bundle")
	}
	if q := s.QuickOpen(ctx, "alic", 3); len(q) == 0 || q[0].Path != "Contacts/Alice.md" {
		t.Errorf("quick open: %+v", q)
	}
}

func TestWriteTools(t *testing.T) {
	ctx := WithClient(context.Background(), "unit-test")
	dir := testutil.NewVaultRepo(t)
	s := openService(t, testConfig(t, dir))
	before := testutil.Git(t, dir, "rev-list", "--count", "HEAD")

	// create
	r, err := s.CreateNote(ctx, "Contacts/Bob.md", "# Bob\n\nA zebra keeper.\n", map[string]any{"type": "contact", "phone": "+1 555: 0199"}, false, "add bob")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNote(ctx, "Contacts/Bob.md")
	if n.Frontmatter["phone"] != "+1 555: 0199" || n.FrontmatterError != "" || !strings.HasPrefix(n.Content, "---\n") {
		t.Errorf("created note: %+v", n)
	}
	msg := testutil.Git(t, dir, "log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "kb_create_note: Contacts/Bob.md\n\nadd bob\n\nKB-Client: unit-test\nKB-Instance: test-instance") {
		t.Errorf("commit message:\n%s", msg)
	}
	if files := testutil.Git(t, dir, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(files) != "Contacts/Bob.md" {
		t.Errorf("commit touched: %q", files)
	}
	if res, _ := s.Search(ctx, search.Request{Query: "zebra"}); len(res.Hits) != 2 {
		t.Errorf("new note must be searchable immediately: %+v", res.Hits)
	}
	if _, err := s.CreateNote(ctx, "Contacts/Bob.md", "x", nil, false, ""); code(err) != CodeAlreadyExists {
		t.Errorf("exists: %v", err)
	}
	if _, err := s.CreateNote(ctx, "Contacts/Bob.txt", "x", nil, false, ""); code(err) != CodeInvalidPath {
		t.Errorf("extension: %v", err)
	}
	if _, err := s.CreateNote(ctx, "Files/scan.pdf", "x", nil, true, ""); code(err) != CodeUnsupportedFile {
		t.Errorf("attachment: %v", err)
	}
	if _, err := s.CreateNote(ctx, ".obsidian/x.md", "x", nil, false, ""); code(err) != CodeNotFound {
		t.Errorf("excluded: %v", err)
	}

	// replace with etag
	if _, err := s.ReplaceNote(ctx, "Contacts/Bob.md", "# Bob2\n", "stale", ""); code(err) != CodeConflict {
		t.Errorf("stale etag: %v", err)
	} else {
		var e *Error
		errors.As(err, &e)
		if e.Details["etag"] != r.ETag || !strings.Contains(e.Details["content"].(string), "zebra") {
			t.Errorf("conflict error must carry current content: %+v", e.Details)
		}
	}
	r2, err := s.ReplaceNote(ctx, "Contacts/Bob.md", "---\nbroken: a: b\n---\n# Bob2\n", r.ETag, "")
	if err != nil || len(r2.Warnings) != 1 || !strings.Contains(r2.Warnings[0], "invalid_frontmatter") {
		t.Errorf("replace with bad yaml must warn: %+v %v", r2, err)
	}
	r3, err := s.ReplaceNote(ctx, "Contacts/Bob.md", "<<<<<<< HEAD\na\n=======\nb\n>>>>>>> x\n", r2.ETag, "")
	if err != nil || len(r3.Warnings) == 0 || !strings.Contains(r3.Warnings[0], CodeMarkersPresent) {
		t.Errorf("markers warning: %+v %v", r3, err)
	}

	// patch: frontmatter-only edit leaves body untouched
	alpha, _ := s.GetNote(ctx, "Projects/Alpha.md")
	p, err := s.PatchNote(ctx, "Projects/Alpha.md", []PatchOp{{Op: "set_frontmatter", Fields: map[string]any{"status": "done"}}}, alpha.ETag, "")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetNote(ctx, "Projects/Alpha.md")
	if after.Body != alpha.Body || after.Frontmatter["status"] != "done" || after.Frontmatter["priority"] != 3 || !strings.Contains(after.Content, "# a comment") {
		t.Errorf("frontmatter patch changed more than the key:\n%s", after.Content)
	}
	p, err = s.PatchNote(ctx, "Projects/Alpha.md", []PatchOp{
		{Op: "replace_section", Heading: "## Log", Content: "replaced log"},
		{Op: "append", Content: "appended line"},
		{Op: "find_replace", Find: "Zebra", Replace: "Giraffe"},
		{Op: "remove_frontmatter", Keys: []string{"priority"}},
	}, p.ETag, "")
	if err != nil {
		t.Fatal(err)
	}
	after, _ = s.GetNote(ctx, "Projects/Alpha.md")
	if !strings.Contains(after.Body, "## Log\n\nreplaced log\n\n## Budget") || !strings.HasSuffix(after.Body, "appended line\n") || strings.Contains(after.Body, "Zebra") || after.Frontmatter["priority"] != nil {
		t.Errorf("multi-op patch:\n%s", after.Content)
	}
	_, err = s.PatchNote(ctx, "Projects/Alpha.md", []PatchOp{{Op: "append", Content: "x"}, {Op: "insert_after_heading", Heading: "Nope", Content: "y"}}, "", "")
	if code(err) != CodePatchFailed {
		t.Errorf("missing heading: %v", err)
	}
	unchanged, _ := s.GetNote(ctx, "Projects/Alpha.md")
	if unchanged.ETag != after.ETag {
		t.Error("failed patch must leave the note unchanged")
	}
	if _, err := s.PatchNote(ctx, "Ideas/Invalid.md", []PatchOp{{Op: "set_frontmatter", Fields: map[string]any{"a": 1}}}, "", ""); code(err) != CodePatchFailed {
		t.Errorf("set_frontmatter on unparseable header: %v", err)
	}
	inv, err := s.PatchNote(ctx, "Ideas/Invalid.md", []PatchOp{{Op: "append", Content: "tail"}}, "", "")
	if err != nil {
		t.Fatalf("append on unparseable header must work: %v", err)
	}
	invNote, _ := s.GetNote(ctx, "Ideas/Invalid.md")
	if !strings.Contains(invNote.Content, "broken: value: with: colons") || !strings.HasSuffix(invNote.Content, "tail\n") || inv.ETag != invNote.ETag {
		t.Errorf("raw header must be preserved:\n%s", invNote.Content)
	}

	// move: no link rewriting, referencing notes reported
	mv, err := s.MoveNote(ctx, "Projects/Beta.md", "Archive/Beta Old.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(mv.ReferencingNotes, ",") != "Projects/Alpha.md,Work/Meeting.md" {
		t.Errorf("referencing notes: %v", mv.ReferencingNotes)
	}
	if files := testutil.Git(t, dir, "show", "--name-status", "--format=", "HEAD"); !strings.Contains(files, "R100") {
		t.Errorf("move must be a git rename: %q", files)
	}
	alphaAfter, _ := s.GetNote(ctx, "Projects/Alpha.md")
	if !strings.Contains(alphaAfter.Body, "[[Beta]]") {
		t.Error("links must not be rewritten")
	}
	if _, err := s.MoveNote(ctx, "Archive/Beta Old.md", "Projects/Alpha.md", ""); code(err) != CodeAlreadyExists {
		t.Errorf("move onto existing: %v", err)
	}

	// delete + restore from history
	del, err := s.DeleteNote(ctx, "Archive/Beta Old.md", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.v.Exists("Archive/Beta Old.md") {
		t.Error("file still exists")
	}
	if res, _ := s.Search(ctx, search.Request{Query: "паспорт"}); len(res.Hits) != 0 {
		t.Error("deleted note still searchable")
	}
	if _, err := s.DeleteNote(ctx, "Archive/Beta Old.md", "", ""); code(err) != CodeNotFound {
		t.Errorf("double delete: %v", err)
	}
	hist, err := s.History(ctx, "Archive/Beta Old.md", 10)
	if err != nil || len(hist) != 3 || hist[0].Hash != del.Commit {
		t.Errorf("history of deleted+renamed note: %+v %v", hist, err)
	}
	tree, err := s.LsTree(ctx, del.Commit+"~1", "Archive")
	if err != nil || len(tree) != 1 || tree[0].Path != "Archive/Beta Old.md" {
		t.Errorf("ls-tree before delete: %+v %v", tree, err)
	}
	rev, err := s.ShowRevision(ctx, "Archive/Beta Old.md", "HEAD~1")
	if err != nil || !strings.Contains(rev.Content, "паспорта") {
		t.Errorf("show revision: %+v %v", rev, err)
	}
	if _, err := s.ShowRevision(ctx, "Archive/Beta Old.md", "HEAD"); code(err) != CodeNotFound {
		t.Errorf("show at HEAD: %v", err)
	}
	rs, err := s.Restore(ctx, "Archive/Beta Old.md", "HEAD~1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.v.Exists("Archive/Beta Old.md") {
		t.Error("restore did not recreate the file")
	}
	if res, _ := s.Search(ctx, search.Request{Query: "паспорт"}); len(res.Hits) != 1 {
		t.Error("restored note must be searchable")
	}
	if msg := testutil.Git(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "kb_restore: Archive/Beta Old.md (from ") {
		t.Errorf("restore subject: %q", msg)
	}
	if rs.Commit == "" {
		t.Error("restore commit missing")
	}
	diff, err := s.Diff(ctx, "HEAD~1", "", "Archive/Beta Old.md")
	if err != nil || !strings.Contains(diff, "+++ b/Archive/Beta Old.md") {
		t.Errorf("diff: %v %q", err, diff)
	}
	if _, err := s.Diff(ctx, "nope", "", ""); code(err) != CodeNotFound {
		t.Errorf("diff unknown rev: %v", err)
	}
	log, err := s.Log(ctx, LogRequest{Limit: 3})
	if err != nil || len(log.Commits) != 3 || log.NextCursor != 3 {
		t.Errorf("log page: %+v %v", log, err)
	}
	log2, _ := s.Log(ctx, LogRequest{Limit: 100, Cursor: 3})
	if len(log2.Commits) == 0 || log2.NextCursor != 0 || log2.Commits[0].Hash == log.Commits[0].Hash {
		t.Errorf("log page 2: %+v", log2)
	}
	folderLog, _ := s.Log(ctx, LogRequest{Folder: "Contacts", Limit: 50})
	for _, c := range folderLog.Commits {
		for _, ch := range c.Changes {
			if !strings.HasPrefix(ch.Path, "Contacts/") {
				t.Errorf("folder log leaked %s", ch.Path)
			}
		}
	}
	afterCount := testutil.Git(t, dir, "rev-list", "--count", "HEAD")
	if strings.TrimSpace(afterCount) == strings.TrimSpace(before) {
		t.Error("no commits were made")
	}
	if dirty, _ := s.repo.IsDirty(ctx); dirty {
		t.Error("work tree must be clean after writes")
	}
}

func TestReadOnly(t *testing.T) {
	cfg := testConfig(t, testutil.NewVaultRepo(t))
	cfg.Server.ReadOnly = true
	s := openService(t, cfg)
	if _, err := s.CreateNote(context.Background(), "x.md", "x", nil, false, ""); code(err) != CodeReadOnly {
		t.Errorf("read-only: %v", err)
	}
	if !s.ReadOnly() {
		t.Error("ReadOnly() false")
	}
}

func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	dir := testutil.NewVaultRepo(t)
	s := openService(t, testConfig(t, dir))
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "Projects/Alpha.md"
			if i%2 == 1 {
				path = "Notes/Meeting.md"
			}
			if _, err := s.PatchNote(ctx, path, []PatchOp{{Op: "append", Content: "line"}}, "", ""); err != nil {
				errs <- err
			}
			_, _ = s.GetNote(ctx, path)
			_, _ = s.Search(ctx, search.Request{Query: "line"})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := strings.TrimSpace(testutil.Git(t, dir, "rev-list", "--count", "HEAD")); n != "13" {
		t.Errorf("expected 13 commits, got %s", n)
	}
}

func TestSyncWithRemote(t *testing.T) {
	ctx := context.Background()
	vaultDir, remote := testutil.NewVaultWithRemote(t)
	s := openService(t, testConfig(t, vaultDir))
	other := testutil.CloneRemote(t, remote)

	// Local write is pushed by kb_sync_now.
	if _, err := s.CreateNote(ctx, "Notes/FromServer.md", "# From server\n", nil, false, ""); err != nil {
		t.Fatal(err)
	}
	st, err := s.SyncNow(ctx)
	if err != nil || st.State != "ok" || st.Ahead != 0 {
		t.Fatalf("sync: %+v %v", st, err)
	}
	testutil.Git(t, other, "pull", "-q")
	if _, err := os.Stat(filepath.Join(other, "Notes", "FromServer.md")); err != nil {
		t.Error("remote did not receive the note")
	}

	// Remote write is picked up before the next local write.
	testutil.WriteAndPush(t, other, "Notes/FromOther.md", "# From other\n\nzebra remote\n", "other add")
	w, err := s.PatchNote(ctx, "Projects/Alpha.md", []PatchOp{{Op: "append", Content: "x"}}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := s.Search(ctx, search.Request{Query: "zebra"}); len(res.Hits) != 2 {
		t.Errorf("pulled note must be indexed: %+v", res.Hits)
	}
	parents := strings.Fields(testutil.Git(t, vaultDir, "log", "-1", "--format=%P", w.Commit))
	remoteHead := strings.TrimSpace(testutil.Git(t, other, "rev-parse", "HEAD"))
	if len(parents) != 1 || parents[0] != remoteHead {
		t.Errorf("write must land on remote head: parents=%v remote=%s", parents, remoteHead)
	}

	// Push rejected -> pull (clean merge) -> push again.
	if _, err := s.SyncNow(ctx); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, other, "pull", "-q")
	testutil.WriteAndPush(t, other, "Notes/Race.md", "# Race\n", "race")
	s.stateMu.Lock()
	s.lastPull = time.Now().Add(time.Hour) // pretend we are fresh so the write skips the pull
	s.stateMu.Unlock()
	s.cfg.Git.PullFreshness = time.Hour
	if _, err := s.CreateNote(ctx, "Notes/Local.md", "# Local\n", nil, false, ""); err != nil {
		t.Fatal(err)
	}
	s.cfg.Git.PullFreshness = 0
	st, err = s.SyncNow(ctx)
	if err != nil || st.Ahead != 0 || st.Behind != 0 || st.State != "ok" {
		t.Fatalf("rejected push must recover: %+v %v", st, err)
	}
	testutil.Git(t, other, "pull", "-q")
	if _, err := os.Stat(filepath.Join(other, "Notes", "Local.md")); err != nil {
		t.Error("remote did not receive Local.md after recovery")
	}
}

func TestFrozenConflict(t *testing.T) {
	ctx := WithClient(context.Background(), "resolver")
	vaultDir, remote := testutil.NewVaultWithRemote(t)
	cfg := testConfig(t, vaultDir)
	s := openService(t, cfg)
	other := testutil.CloneRemote(t, remote)

	// Both sides edit the same lines of the same note, plus a second conflict.
	testutil.WriteAndPush(t, other, "Notes/Meeting.md", "# Meeting\n\nremote version\n", "remote edit")
	testutil.WriteAndPush(t, other, "Work/Meeting.md", "---\ntype: meeting\n---\n# Work meeting\n\nremote\n", "remote edit 2")
	s.cfg.Git.PullFreshness = time.Hour
	s.stateMu.Lock()
	s.lastPull = time.Now()
	s.stateMu.Unlock()
	if _, err := s.ReplaceNote(ctx, "Notes/Meeting.md", "# Meeting\n\nlocal version\n", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceNote(ctx, "Work/Meeting.md", "---\ntype: meeting\n---\n# Work meeting\n\nlocal\n", "", ""); err != nil {
		t.Fatal(err)
	}
	s.cfg.Git.PullFreshness = 0

	// The next write triggers the pull, which conflicts: write refused.
	_, err := s.PatchNote(ctx, "Notes/Meeting.md", []PatchOp{{Op: "append", Content: "more"}}, "", "")
	if code(err) != CodeMergeConflict {
		t.Fatalf("expected merge_conflict, got %v", err)
	}
	var e *Error
	errors.As(err, &e)
	conflicts := e.Details["conflicts"].([]Conflict)
	if len(conflicts) != 2 {
		t.Fatalf("conflicts: %+v", conflicts)
	}
	var meeting Conflict
	for _, c := range conflicts {
		if c.Path == "Notes/Meeting.md" {
			meeting = c
		}
	}
	if meeting.Kind != "content" || meeting.Ours == nil || meeting.Theirs == nil || meeting.Base == nil || meeting.Content == nil {
		t.Fatalf("conflict payload incomplete: %+v", meeting)
	}
	if !strings.Contains(*meeting.Ours, "local version") || !strings.Contains(*meeting.Theirs, "remote version") || !strings.Contains(*meeting.Content, "<<<<<<<") {
		t.Errorf("sides: ours=%q theirs=%q", *meeting.Ours, *meeting.Theirs)
	}
	if rc := e.Details["remote_commits"].([]gitrepo.Commit); len(rc) != 2 {
		t.Errorf("remote commits: %+v", rc)
	}
	// Nothing committed or pushed.
	if strings.TrimSpace(testutil.Git(t, vaultDir, "log", "-1", "--format=%s")) != "kb_replace_note: Work/Meeting.md" {
		t.Error("merge must not be committed")
	}
	remoteLog := testutil.Git(t, other, "log", "origin/main", "-1", "--format=%s")
	if strings.Contains(remoteLog, "kb_") {
		t.Error("nothing must be pushed while in conflict")
	}
	// State, reads, blocked writes.
	st, _ := s.Status(ctx)
	if st.State != "conflict" || len(st.Conflicts) != 2 {
		t.Errorf("status: %+v", st)
	}
	if info := s.GetInfo(ctx); info.SyncState != "conflict" {
		t.Errorf("info state: %s", info.SyncState)
	}
	if _, err := s.CreateNote(ctx, "Notes/Other.md", "x", nil, false, ""); code(err) != CodeMergeConflict {
		t.Errorf("writes must be blocked: %v", err)
	}
	if _, err := s.SyncNow(ctx); code(err) != CodeMergeConflict {
		t.Errorf("sync_now must report the conflict: %v", err)
	}
	nv, err := s.GetNote(ctx, "Notes/Meeting.md")
	if err != nil || nv.Conflict == nil || !strings.Contains(nv.Content, "=======") {
		t.Errorf("get note during conflict: %+v %v", nv, err)
	}
	if alpha, err := s.GetNote(ctx, "Projects/Alpha.md"); err != nil || alpha.Conflict != nil {
		t.Errorf("other notes read normally: %v", err)
	}
	rep, _ := s.Conflicts(ctx)
	if len(rep.Conflicts) != 2 {
		t.Errorf("kb_conflicts: %+v", rep)
	}

	// Restart in conflict state keeps it.
	_ = s.Close()
	s = openService(t, cfg)
	if st, _ := s.Status(ctx); st.State != "conflict" {
		t.Fatalf("conflict must survive restart: %+v", st)
	}

	// Bad resolutions.
	if _, err := s.ResolveConflict(ctx, "", []Resolution{{Path: "Notes/Meeting.md", Content: "<<<<<<< a\nx\n=======\ny\n>>>>>>> b\n"}}); code(err) != CodeMarkersPresent {
		t.Errorf("markers: %v", err)
	}
	if _, err := s.ResolveConflict(ctx, "", []Resolution{{Path: "Projects/Alpha.md", Content: "x"}}); code(err) != CodeNotFound {
		t.Errorf("not conflicted path: %v", err)
	}
	if _, err := s.ResolveConflict(ctx, "", []Resolution{{Path: "Notes/Meeting.md", Take: "mine"}}); code(err) != CodeInvalidArgument {
		t.Errorf("bad take: %v", err)
	}
	// Partial resolution.
	rr, err := s.ResolveConflict(ctx, "", []Resolution{{Path: "Work/Meeting.md", Take: "theirs"}})
	if err != nil || rr.Complete || len(rr.Remaining) != 1 || rr.Remaining[0] != "Notes/Meeting.md" {
		t.Fatalf("partial: %+v %v", rr, err)
	}
	if _, err := s.CreateNote(ctx, "Notes/Other.md", "x", nil, false, ""); code(err) != CodeMergeConflict {
		t.Errorf("writes stay blocked after partial resolution: %v", err)
	}
	// Complete resolution.
	rr, err = s.ResolveConflict(ctx, "merged both", []Resolution{{Path: "Notes/Meeting.md", Content: "# Meeting\n\nmerged by client\n"}})
	if err != nil || !rr.Complete || rr.Commit == "" || len(rr.Remaining) != 0 {
		t.Fatalf("complete: %+v %v", rr, err)
	}
	msg := testutil.Git(t, vaultDir, "log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "merge: origin/main (resolved: ") || !strings.Contains(msg, "merged both") || !strings.Contains(msg, "KB-Client: resolver") {
		t.Errorf("merge commit message:\n%s", msg)
	}
	if parents := strings.Fields(testutil.Git(t, vaultDir, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("expected a merge commit, parents=%v", parents)
	}
	st, err = s.SyncNow(ctx)
	if err != nil || st.State != "ok" || st.Ahead != 0 || len(st.Conflicts) != 0 {
		t.Fatalf("after resolution: %+v %v", st, err)
	}
	testutil.Git(t, other, "pull", "-q")
	data, _ := os.ReadFile(filepath.Join(other, "Notes", "Meeting.md"))
	if string(data) != "# Meeting\n\nmerged by client\n" {
		t.Errorf("remote content: %q", data)
	}
	work, _ := s.GetNote(ctx, "Work/Meeting.md")
	if !strings.Contains(work.Body, "remote") || strings.Contains(work.Body, "local") {
		t.Errorf("take theirs failed: %q", work.Body)
	}
	if res, _ := s.Search(ctx, search.Request{Query: "merged"}); len(res.Hits) != 1 {
		t.Error("resolved note must be indexed")
	}
	if _, err := s.ResolveConflict(ctx, "", nil); code(err) != CodeNotInConflict {
		t.Errorf("no conflict: %v", err)
	}
}

func TestDeleteModifyConflictResolvedByDelete(t *testing.T) {
	ctx := context.Background()
	vaultDir, remote := testutil.NewVaultWithRemote(t)
	s := openService(t, testConfig(t, vaultDir))
	other := testutil.CloneRemote(t, remote)
	testutil.Git(t, other, "rm", "-q", "Notes/Meeting.md")
	testutil.Git(t, other, "commit", "-q", "-m", "remote delete")
	testutil.Git(t, other, "push", "-q", "origin", "main")
	s.cfg.Git.PullFreshness = time.Hour
	s.stateMu.Lock()
	s.lastPull = time.Now()
	s.stateMu.Unlock()
	if _, err := s.PatchNote(ctx, "Notes/Meeting.md", []PatchOp{{Op: "append", Content: "local"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	s.cfg.Git.PullFreshness = 0
	_, err := s.SyncNow(ctx)
	if code(err) != CodeMergeConflict {
		t.Fatalf("expected conflict: %v", err)
	}
	rep, _ := s.Conflicts(ctx)
	if len(rep.Conflicts) != 1 || rep.Conflicts[0].Kind != "modify/delete" || rep.Conflicts[0].Theirs != nil {
		t.Fatalf("conflict: %+v", rep.Conflicts)
	}
	rr, err := s.ResolveConflict(ctx, "", []Resolution{{Path: "Notes/Meeting.md", Delete: true}})
	if err != nil || !rr.Complete {
		t.Fatalf("resolve by delete: %+v %v", rr, err)
	}
	if s.v.Exists("Notes/Meeting.md") {
		t.Error("note should be gone")
	}
	if _, ok := s.cat.Get("Notes/Meeting.md"); ok {
		t.Error("catalog still has the deleted note")
	}
}

func TestIndexCatchUpOnRestart(t *testing.T) {
	ctx := context.Background()
	dir := testutil.NewVaultRepo(t)
	cfg := testConfig(t, dir)
	s := openService(t, cfg)
	_ = s.Close()
	// Change the vault behind the server's back (simulates a pull done by a
	// previous process whose index stamp is older than HEAD).
	if err := os.WriteFile(filepath.Join(dir, "Notes", "Later.md"), []byte("# Later\n\nunicorn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, dir, "add", "-A")
	testutil.Git(t, dir, "commit", "-q", "-m", "external")
	s = openService(t, cfg)
	if res, _ := s.Search(ctx, search.Request{Query: "unicorn"}); len(res.Hits) != 1 {
		t.Errorf("index must catch up with HEAD on restart: %+v", res)
	}
	info, err := s.Reindex(ctx)
	if err != nil || info.IndexedDocs != 9 {
		t.Errorf("reindex: %+v %v", info, err)
	}
}

func TestPushLoop(t *testing.T) {
	ctx := context.Background()
	vaultDir, remote := testutil.NewVaultWithRemote(t)
	cfg := testConfig(t, vaultDir)
	cfg.Git.PushDebounce = 50 * time.Millisecond
	s := openService(t, cfg)
	s.Start(ctx)
	if _, err := s.CreateNote(ctx, "Notes/Loop.md", "# Loop\n", nil, false, ""); err != nil {
		t.Fatal(err)
	}
	other := testutil.CloneRemote(t, remote)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		testutil.Git(t, other, "pull", "-q")
		if _, err := os.Stat(filepath.Join(other, "Notes", "Loop.md")); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("push loop did not push within 5s")
}
