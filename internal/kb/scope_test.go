package kb

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

// restricted is the rule set the access fixture is written for.
var restricted = acl.Set{
	{Path: "Open", Level: acl.Write},
	{Path: "Open/hidn-closed", Level: acl.None},
	{Path: "Shared/Team", Level: acl.Read},
	{Path: "ReadOnly", Level: acl.Read},
}

func accessService(t *testing.T) (*Service, context.Context, string) {
	t.Helper()
	dir, remote := testutil.NewAccessRepo(t)
	cfg := testConfig(t, dir)
	cfg.Vault.Exclude = nil
	s := openService(t, cfg)
	return s, WithUser(WithScope(context.Background(), restricted), "ann@example.com"), remote
}

// leaks fails the test when v, as JSON, mentions anything hidden.
func leaks(t *testing.T, what string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	for _, tok := range []string{testutil.HiddenToken, testutil.HiddenMarker} {
		if strings.Contains(string(raw), tok) {
			t.Errorf("%s leaks %q: %s", what, tok, raw)
		}
	}
}

func message(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	return ""
}

// sameAsMissing asserts that an operation answers a hidden existing path
// exactly as it answers a path that does not exist.
func sameAsMissing(t *testing.T, what, hidden, missing string, op func(path string) error) {
	t.Helper()
	he, me := op(hidden), op(missing)
	if code(he) != CodeNotFound || code(me) != CodeNotFound {
		t.Errorf("%s: codes %s / %s, want not_found for both", what, code(he), code(me))
		return
	}
	if got, want := strings.ReplaceAll(message(he), hidden, "<p>"), strings.ReplaceAll(message(me), missing, "<p>"); got != want {
		t.Errorf("%s: hidden answers %q, missing answers %q", what, message(he), message(me))
	}
}

func TestScopeReads(t *testing.T) {
	s, ctx, _ := accessService(t)
	if _, err := s.GetNote(ctx, "Open/Note.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, "Shared/Team/Plan.md"); err != nil {
		t.Fatal(err)
	}
	ops := map[string]func(string) error{
		"GetNote":      func(p string) error { _, err := s.GetNote(ctx, p); return err },
		"GetSection":   func(p string) error { _, err := s.GetSection(ctx, p, "Hidn secret"); return err },
		"StatFile":     func(p string) error { _, err := s.StatFile(ctx, p); return err },
		"ReadFile":     func(p string) error { _, _, err := s.ReadFile(ctx, p, 0); return err },
		"Backlinks":    func(p string) error { _, err := s.Backlinks(ctx, p); return err },
		"History":      func(p string) error { _, err := s.History(ctx, p, 0); return err },
		"ShowRevision": func(p string) error { _, err := s.ShowRevision(ctx, p, "HEAD"); return err },
		"LinkTarget":   func(p string) error { _, err := s.LinkTarget(ctx, "get", p); return err },
	}
	for name, op := range ops {
		sameAsMissing(t, name, "hidn-private/hidn-secret.md", "hidn-private/absent.md", op)
		sameAsMissing(t, name+" (closed subfolder)", "Open/hidn-closed/hidn-note.md", "Open/hidn-closed/absent.md", op)
	}
	folderOps := map[string]func(string) error{
		"List":   func(p string) error { _, err := s.List(ctx, ListRequest{Folder: p}); return err },
		"LsTree": func(p string) error { _, err := s.LsTree(ctx, "HEAD", p); return err },
		"Grep": func(p string) error {
			_, err := s.Grep(ctx, search.GrepRequest{Pattern: "zebra", Folder: p})
			return err
		},
	}
	for name, op := range folderOps {
		sameAsMissing(t, name, "hidn-private", "absent-folder", op)
		sameAsMissing(t, name+" (closed subfolder)", "Open/hidn-closed", "Open/absent", op)
	}
}

func TestScopeWrites(t *testing.T) {
	s, ctx, _ := accessService(t)
	head := func() string { h, _ := s.repo.Head(ctx); return h }
	before := head()
	writes := map[string]func(string) error{
		"create":  func(p string) error { _, err := s.CreateNote(ctx, p, "x", nil, true, ""); return err },
		"replace": func(p string) error { _, err := s.ReplaceNote(ctx, p, "x", "", ""); return err },
		"patch": func(p string) error {
			_, err := s.PatchNote(ctx, p, []PatchOp{{Op: "append", Content: "x"}}, "", "")
			return err
		},
		"delete":  func(p string) error { _, err := s.DeleteNote(ctx, p, "", ""); return err },
		"restore": func(p string) error { _, err := s.Restore(ctx, p, "HEAD~1", ""); return err },
		"upload":  func(p string) error { _, err := s.PutFile(ctx, p, []byte("x"), true, ""); return err },
		"upload link": func(p string) error {
			_, err := s.LinkTarget(ctx, "put", p)
			return err
		},
	}
	for name, op := range writes {
		if err := op("ReadOnly/Doc.md"); code(err) != CodeForbidden {
			t.Errorf("%s on a read-only path: %v, want forbidden", name, err)
		}
		if err := op("hidn-private/hidn-secret.md"); code(err) != CodeNotFound {
			t.Errorf("%s on a hidden path: %v, want not_found", name, err)
		}
		if err := op("hidn-private/new.md"); code(err) != CodeNotFound {
			t.Errorf("%s of a new file in a hidden folder: %v, want not_found", name, err)
		}
	}
	moves := []struct {
		from, to, want string
	}{
		{"Open/Note.md", "ReadOnly/Note.md", CodeForbidden},
		{"ReadOnly/Doc.md", "Open/Doc.md", CodeForbidden},
		{"Open/Note.md", "hidn-private/Note.md", CodeNotFound},
		{"hidn-private/hidn-secret.md", "Open/Got.md", CodeNotFound},
		{"Open/Note.md", "Open/hidn-closed/Note.md", CodeNotFound},
	}
	for _, m := range moves {
		if _, err := s.MoveNote(ctx, m.from, m.to, ""); code(err) != m.want {
			t.Errorf("move %s -> %s: %v, want %s", m.from, m.to, err, m.want)
		}
	}
	if head() != before {
		t.Fatal("a refused write must not create a commit")
	}
	res, err := s.PatchNote(ctx, "Open/Note.md", []PatchOp{{Op: "append", Content: "allowed"}}, "", "")
	if err != nil || res.Commit == "" {
		t.Fatalf("write on a writable path: %v", err)
	}
	msg := testutil.Git(t, s.repo.Dir, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "KB-User: ann@example.com") || !strings.HasPrefix(msg, "kb_patch_note: Open/Note.md") {
		t.Errorf("commit message: %q", msg)
	}
	// A move inside the writable area reports only readable referencing notes.
	mv, err := s.MoveNote(ctx, "Open/Note.md", "Open/Renamed.md", "")
	if err != nil {
		t.Fatal(err)
	}
	leaks(t, "move result", mv)

	// Server-wide read-only mode wins over an account's write level.
	s.cfg.Server.ReadOnly = true
	if _, err := s.PatchNote(ctx, "Open/Sub/Deep.md", []PatchOp{{Op: "append", Content: "x"}}, "", ""); code(err) != CodeReadOnly {
		t.Errorf("read-only server: %v", err)
	}
}

func paths[T any](items []T, get func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = get(it)
	}
	return out
}

func TestScopeListingsAndLinks(t *testing.T) {
	s, ctx, _ := accessService(t)
	list := func(folder string, recursive bool) []string {
		t.Helper()
		res, err := s.List(ctx, ListRequest{Folder: folder, Recursive: recursive})
		if err != nil {
			t.Fatalf("list %q: %v", folder, err)
		}
		leaks(t, "list "+folder, res)
		out := []string{}
		for _, e := range res.Entries {
			out = append(out, e.Path)
		}
		return out
	}
	eq := func(what string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	eq("root", list("", false), "Open", "ReadOnly", "Shared")
	// Shared itself is not readable: it only leads to Team.
	eq("pass-through folder", list("Shared", false), "Shared/Team")
	eq("deep grant", list("Shared/Team", false), "Shared/Team/Plan.md")
	eq("open folder", list("Open", false), "Open/Moved.md", "Open/Note.md", "Open/Sub", "Open/file.bin")
	eq("recursive", list("Open", true), "Open/Moved.md", "Open/Note.md", "Open/Sub/Deep.md", "Open/file.bin")
	eq("recursive root", list("", true), "Open/Moved.md", "Open/Note.md", "Open/Sub/Deep.md", "Open/file.bin", "ReadOnly/Doc.md", "Shared/Team/Plan.md")

	n, err := s.GetNote(ctx, "Open/Note.md")
	if err != nil {
		t.Fatal(err)
	}
	resolved := map[string]string{}
	for _, l := range n.Links {
		resolved[l.Target] = l.Resolved
	}
	if resolved["Plan"] != "Shared/Team/Plan.md" || resolved["Doc"] != "ReadOnly/Doc.md" {
		t.Errorf("readable links must resolve: %v", resolved)
	}
	if r, ok := resolved["Target"]; !ok || r != "" {
		t.Errorf("a link to a hidden note must be reported unresolved: %v", resolved)
	}
	bl, err := s.Backlinks(ctx, "Open/Note.md")
	if err != nil {
		t.Fatal(err)
	}
	leaks(t, "backlinks", bl)
	if len(bl) != 0 {
		t.Errorf("only hidden notes link here, so backlinks must be empty: %+v", bl)
	}
	all, _ := s.Backlinks(context.Background(), "Open/Note.md")
	if len(all) != 2 {
		t.Fatalf("fixture changed: unrestricted backlinks = %+v", all)
	}
	plan, _ := s.Backlinks(ctx, "Shared/Team/Plan.md")
	if len(plan) != 1 || plan[0].Source != "Open/Note.md" {
		t.Errorf("backlink from a readable note: %+v", plan)
	}
}

func TestScopeSearch(t *testing.T) {
	s, ctx, _ := accessService(t)
	res, err := s.Search(ctx, search.Request{Query: "QUOKKA*"})
	if err != nil || len(res.Hits) != 0 || res.Total != 0 {
		t.Errorf("a match only in hidden notes: %+v %v", res, err)
	}
	if un, _ := s.Search(context.Background(), search.Request{Query: "QUOKKA*"}); un.Total < 5 {
		t.Fatalf("fixture changed: unrestricted hits = %d", un.Total)
	}
	// zebra is in 4 readable and 4 hidden notes.
	res, err = s.Search(ctx, search.Request{Query: "zebra", Limit: 20})
	if err != nil || res.Total != 4 || len(res.Hits) != 4 {
		t.Fatalf("zebra: total=%d hits=%d err=%v", res.Total, len(res.Hits), err)
	}
	leaks(t, "search", res)
	for _, h := range res.Hits {
		if h.Snippet == "" || h.Title == "" {
			t.Errorf("hit without snippet or title: %+v", h)
		}
	}
	// A page is filled with readable notes only, and paging has no gaps.
	seen := map[string]bool{}
	for off := 0; off < 4; off += 2 {
		page, err := s.Search(ctx, search.Request{Query: "zebra", Limit: 2, Offset: off})
		if err != nil || len(page.Hits) != 2 || page.Total != 4 {
			t.Fatalf("page at %d: %+v %v", off, page, err)
		}
		for _, h := range page.Hits {
			if seen[h.Path] {
				t.Errorf("duplicate across pages: %s", h.Path)
			}
			seen[h.Path] = true
		}
	}
	if beyond, _ := s.Search(ctx, search.Request{Query: "zebra", Limit: 2, Offset: 4}); len(beyond.Hits) != 0 {
		t.Errorf("offset past the readable hits: %+v", beyond.Hits)
	}
	if in, _ := s.Search(ctx, search.Request{Query: "zebra", Filters: search.Filters{Folder: "hidn-private"}}); in.Total != 0 {
		t.Errorf("folder filter on a hidden folder: %+v", in)
	}

	g, err := s.Grep(ctx, search.GrepRequest{Pattern: "zebra"})
	if err != nil || g.Total != 4 {
		t.Errorf("grep: total=%d err=%v", g.Total, err)
	}
	leaks(t, "grep", g)
	if g, _ := s.Grep(ctx, search.GrepRequest{Pattern: testutil.HiddenMarker}); g.Total != 0 {
		t.Errorf("grep for hidden content: %+v", g)
	}

	rows, err := s.Query(ctx, search.QueryRequest{Where: ""})
	if err != nil || len(rows) != 5 {
		t.Errorf("query: %d rows, err=%v", len(rows), err)
	}
	leaks(t, "query", rows)
	if rows, _ := s.Query(ctx, search.QueryRequest{Where: `type eq "hidnkind"`}); len(rows) != 0 {
		t.Errorf("query on a hidden note's frontmatter: %+v", rows)
	}

	tags := s.Tags(ctx, "")
	leaks(t, "tags", tags)
	if len(tags) != 1 || tags[0].Tag != "open" || tags[0].Count != 2 {
		t.Errorf("tags: %+v", tags)
	}
	if all := s.Tags(context.Background(), ""); len(all) != 2 {
		t.Fatalf("fixture changed: unrestricted tags = %+v", all)
	}

	if c := s.QuickOpen(ctx, "hidn", 10); len(c) != 0 {
		t.Errorf("quick open on hidden names: %+v", c)
	}
	if c := s.QuickOpen(ctx, "plan", 10); len(c) != 1 || c[0].Path != "Shared/Team/Plan.md" {
		t.Errorf("quick open: %+v", c)
	}

	bundle, err := s.Context(ctx, search.ContextRequest{Query: "zebra"})
	if err != nil || bundle.Chunks == 0 {
		t.Errorf("context: %+v %v", bundle, err)
	}
	leaks(t, "context", bundle)

	if n := s.GetInfo(ctx).NoteCount; n != 5 {
		t.Errorf("note count for the account = %d, want 5", n)
	}
}

func TestScopeHistory(t *testing.T) {
	s, ctx, _ := accessService(t)
	res, err := s.Log(ctx, LogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	leaks(t, "log", res)
	type entry struct {
		msg     string
		partial bool
		paths   string
	}
	var got []entry
	for _, c := range res.Commits {
		got = append(got, entry{c.Message, c.Partial, strings.Join(paths(c.Changes, func(ch gitrepo.Change) string { return ch.Status + " " + ch.Path }), ",")})
	}
	want := []entry{
		{"edit the moved note", false, "M Open/Moved.md"},
		{"", true, "A Open/Moved.md"}, // moved in from a hidden folder
		{"edit the open note", false, "M Open/Note.md"},
		{"", true, "M Open/Note.md"}, // mixed commit
		{"", true, "A Open/Moved.md,A Open/Note.md,A Open/Sub/Deep.md,A Open/file.bin,A ReadOnly/Doc.md,A Shared/Team/Plan.md"},
	}
	if len(got) != len(want) {
		t.Fatalf("log has %d commits, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if i == len(want)-1 {
			// The initial commit: compare the set regardless of order.
			if got[i].partial != true || got[i].msg != "" || strings.Contains(got[i].paths, "Moved") {
				t.Errorf("initial commit: %+v", got[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("commit %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Paging counts visible commits only.
	var paged []string
	cursor := 0
	for {
		page, err := s.Log(ctx, LogRequest{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Commits {
			paged = append(paged, c.Hash)
		}
		if page.NextCursor == 0 {
			break
		}
		if page.NextCursor != cursor+2 {
			t.Fatalf("cursor jumped from %d to %d", cursor, page.NextCursor)
		}
		cursor = page.NextCursor
	}
	if strings.Join(paged, ",") != strings.Join(paths(res.Commits, func(c gitrepo.Commit) string { return c.Hash }), ",") {
		t.Errorf("paged log differs from the full log")
	}

	if in, _ := s.Log(ctx, LogRequest{Folder: "hidn-private"}); len(in.Commits) != 0 {
		t.Errorf("log of a hidden folder: %+v", in.Commits)
	}
	if in, _ := s.Log(ctx, LogRequest{Folder: "Shared/Team"}); len(in.Commits) != 1 || !in.Commits[0].Partial {
		t.Errorf("log of a readable folder: %+v", in.Commits)
	}

	hist, err := s.History(ctx, "Open/Moved.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	leaks(t, "history", hist)
	if len(hist) != 2 || hist[0].Message != "edit the moved note" || !hist[1].Partial {
		t.Errorf("history must stop where the note came out of a hidden folder: %+v", hist)
	}
	if full, _ := s.History(context.Background(), "Open/Moved.md", 0); len(full) != 3 {
		t.Fatalf("fixture changed: unrestricted history = %d", len(full))
	}

	diff, err := s.Diff(ctx, "HEAD~6", "", "")
	if err != nil || !strings.Contains(diff, "Open/Note.md") {
		t.Fatalf("whole diff: %v", err)
	}
	leaks(t, "diff", diff)
	if d, err := s.Diff(ctx, "HEAD~6", "", "hidn-private/hidn-secret.md"); err != nil || d != "" {
		t.Errorf("diff of a hidden path must be empty like a missing one: %q %v", d, err)
	}
	if d, err := s.Diff(ctx, "HEAD~6", "HEAD", "Open"); err != nil || !strings.Contains(d, "Open/Note.md") || strings.Contains(d, "hidn") {
		t.Errorf("diff of a folder: %v", err)
	}

	tree, err := s.LsTree(ctx, "HEAD~6", "")
	if err != nil {
		t.Fatal(err)
	}
	leaks(t, "ls-tree", tree)
	if got := strings.Join(paths(tree, func(e gitrepo.TreeEntry) string { return e.Path }), ","); got != "Open,ReadOnly,Shared" {
		t.Errorf("ls-tree root = %s", got)
	}
	if _, err := s.Restore(ctx, "ReadOnly/Doc.md", "HEAD", ""); code(err) != CodeForbidden {
		t.Errorf("restore on a read-only path: %v", err)
	}
	n, err := s.GetNote(ctx, "Open/Note.md")
	if err != nil || n.LastCommit == nil || n.LastCommit.Message != "edit the open note" {
		t.Errorf("last commit of a note: %+v %v", n.LastCommit, err)
	}
	leaks(t, "note view", n)
}

func TestScopeConflicts(t *testing.T) {
	s, ctx, remote := accessService(t)
	owner := context.Background()
	other := testutil.CloneRemote(t, remote)
	testutil.WriteAndPush(t, other, "Open/Note.md", "# Open note\n\nremote\n", "remote edit of the open note")
	testutil.WriteAndPush(t, other, "hidn-private/hidn-secret.md", "# Hidn secret\n\nremote "+testutil.HiddenMarker+"\n", "hidn: remote edit")
	s.cfg.Git.PullFreshness = time.Hour
	s.stateMu.Lock()
	s.lastPull = time.Now()
	s.stateMu.Unlock()
	//nolint:gosec // fixture note paths, not credentials
	for p, body := range map[string]string{"Open/Note.md": "# Open note\n\nlocal\n", "hidn-private/hidn-secret.md": "# Hidn secret\n\nlocal\n"} {
		if _, err := s.ReplaceNote(owner, p, body, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Git.PullFreshness = 0

	_, err := s.PatchNote(ctx, "Open/Sub/Deep.md", []PatchOp{{Op: "append", Content: "x"}}, "", "")
	if code(err) != CodeMergeConflict {
		t.Fatalf("expected merge_conflict, got %v", err)
	}
	var e *Error
	errors.As(err, &e)
	leaks(t, "merge_conflict error", e.Details)
	if c := e.Details["conflicts"].([]Conflict); len(c) != 1 || c[0].Path != "Open/Note.md" || e.Details["hidden"] != 1 {
		t.Errorf("error details: %+v", e.Details)
	}
	st, err := s.Status(ctx)
	if err != nil || st.State != "conflict" || len(st.Conflicts) != 1 || st.Conflicts[0] != "Open/Note.md" || st.HiddenConflicts != 1 {
		t.Errorf("status: %+v %v", st, err)
	}
	leaks(t, "status", st)
	rep, err := s.Conflicts(ctx)
	if err != nil || len(rep.Conflicts) != 1 || rep.Hidden != 1 {
		t.Errorf("conflict report: %+v %v", rep, err)
	}
	leaks(t, "conflict report", rep)
	if len(rep.RemoteCommits) != 1 || rep.RemoteCommits[0].Message != "remote edit of the open note" {
		t.Errorf("remote commits must show only the readable one: %+v", rep.RemoteCommits)
	}
	if _, err := s.ResolveConflict(ctx, "", []Resolution{{Path: "Open/Note.md", Take: "ours"}}); code(err) != CodeForbidden {
		t.Errorf("resolving without write on every conflicted path: %v", err)
	}
	if !s.repo.MergeInProgress(ctx) {
		t.Fatal("a refused resolution must leave the merge frozen")
	}
	// The owner sees and resolves everything.
	full, _ := s.Conflicts(owner)
	if len(full.Conflicts) != 2 || full.Hidden != 0 {
		t.Fatalf("unrestricted report: %+v", full)
	}
	done, err := s.ResolveConflict(owner, "", []Resolution{{Path: "Open/Note.md", Take: "ours"}, {Path: "hidn-private/hidn-secret.md", Take: "theirs"}})
	if err != nil || !done.Complete {
		t.Fatalf("owner resolve: %+v %v", done, err)
	}
}

func TestMountedVault(t *testing.T) {
	dir, _ := testutil.NewAccessRepo(t)
	cfg := testConfig(t, dir)
	cfg.Vault.Exclude = nil
	s, err := OpenWith(context.Background(), cfg, nil, "test", Options{Mount: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := WithUser(WithScope(context.Background(), restricted), "ann@example.com")

	n, err := s.GetNote(ctx, "notes/Open/Note.md")
	if err != nil || n.Path != "notes/Open/Note.md" {
		t.Fatalf("mounted read: %+v %v", n, err)
	}
	for _, l := range n.Links {
		if l.Resolved != "" && !strings.HasPrefix(l.Resolved, "notes/") {
			t.Errorf("resolved link without the alias: %+v", l)
		}
	}
	for _, p := range []string{"Open/Note.md", "other/Open/Note.md"} {
		if _, err := s.GetNote(ctx, p); code(err) != CodeNotFound {
			t.Errorf("%s outside the mount: %v", p, err)
		}
	}
	if _, err := s.GetNote(ctx, "notes/../other/x.md"); code(err) != CodeInvalidPath {
		t.Errorf("escape from the mount: %v", err)
	}
	_, err = s.GetNote(ctx, "notes/hidn-private/hidn-secret.md")
	if code(err) != CodeNotFound || !strings.Contains(message(err), "notes/hidn-private/hidn-secret.md") {
		t.Errorf("errors must name the path as the caller wrote it: %v", err)
	}
	root, err := s.List(ctx, ListRequest{Folder: "notes"})
	if err != nil || len(root.Entries) != 3 || root.Entries[0].Path != "notes/Open" {
		t.Fatalf("mounted root: %+v %v", root, err)
	}
	res, err := s.Search(ctx, search.Request{Query: "zebra", Filters: search.Filters{Folder: "notes/Open"}})
	if err != nil || res.Total != 2 || !strings.HasPrefix(res.Hits[0].Path, "notes/Open/") {
		t.Errorf("mounted search with a folder: %+v %v", res, err)
	}
	if res, _ := s.Search(ctx, search.Request{Query: "zebra", Filters: search.Filters{Folder: "elsewhere/Open"}}); res.Total != 0 {
		t.Errorf("a folder of another repository matches nothing here: %+v", res)
	}
	rows, _ := s.Query(ctx, search.QueryRequest{Where: `folder eq "notes/Open"`, Select: []string{"folder"}})
	if len(rows) != 2 || rows[0]["path"] != "notes/Open/Moved.md" || rows[0]["folder"] != "notes/Open" {
		t.Errorf("mounted query: %+v", rows)
	}
	g, _ := s.Grep(ctx, search.GrepRequest{Pattern: "zebra", Glob: "notes/Open/**"})
	if g.Total != 2 || !strings.HasPrefix(g.Files[0].Path, "notes/Open/") {
		t.Errorf("mounted grep with a glob: %+v", g)
	}
	w, err := s.PatchNote(ctx, "notes/Open/Note.md", []PatchOp{{Op: "append", Content: "x"}}, "", "")
	if err != nil || w.Path != "notes/Open/Note.md" {
		t.Fatalf("mounted write: %+v %v", w, err)
	}
	msg := testutil.Git(t, dir, "log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "kb_patch_note: Open/Note.md\n") || !strings.Contains(msg, "KB-User: ann@example.com") {
		t.Errorf("the commit subject is relative to the repository, without the alias: %q", msg)
	}
	lg, err := s.Log(ctx, LogRequest{Folder: "notes", Limit: 1})
	if err != nil || len(lg.Commits) != 1 || lg.Commits[0].Changes[0].Path != "notes/Open/Note.md" {
		t.Errorf("mounted log: %+v %v", lg, err)
	}
	d, err := s.Diff(ctx, "HEAD~1", "HEAD", "notes")
	if err != nil || !strings.Contains(d, "b/notes/Open/Note.md") {
		t.Errorf("mounted diff headers: %v\n%s", err, d)
	}
	// An unrestricted caller of a mounted vault gets the same presentation.
	un, err := s.Log(context.Background(), LogRequest{Limit: 1})
	if err != nil || un.Commits[0].Changes[0].Path != "notes/Open/Note.md" {
		t.Errorf("unrestricted mounted log: %+v %v", un, err)
	}
}

func TestClassicCommitHasNoUserTrailer(t *testing.T) {
	dir, _ := testutil.NewAccessRepo(t)
	cfg := testConfig(t, dir)
	cfg.Vault.Exclude = nil
	s := openService(t, cfg)
	if _, err := s.PatchNote(context.Background(), "Open/Note.md", []PatchOp{{Op: "append", Content: "x"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if msg := testutil.Git(t, dir, "log", "-1", "--format=%B"); strings.Contains(msg, "KB-User") {
		t.Errorf("a classic commit must not carry KB-User: %q", msg)
	}
}
