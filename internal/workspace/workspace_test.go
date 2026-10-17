package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/search"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

// env is a multi-user workspace with an administrator and one regular account.
type env struct {
	t     *testing.T
	ws    *Workspace
	st    *store.Store
	cfg   config.Config
	logs  *bytes.Buffer
	admin context.Context
	ann   context.Context
}

func testKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func baseConfig(t *testing.T, dataDir string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.HTTP.Listen = "127.0.0.1:0"
	cfg.Server.DataDir = dataDir
	cfg.Search.IndexDir = filepath.Join(dataDir, "index")
	cfg.Server.Accounts = config.Accounts{AdminEmail: "admin@example.com", MailFrom: "KB <kb@example.com>", ResendAPIKey: "re_test", SecretKey: testKey(7)}
	cfg.Git.PushDebounce, cfg.Git.PullInterval, cfg.Git.PullFreshness = 0, 0, 0
	cfg.Git.InstanceID = "test-instance"
	return cfg
}

// open starts a workspace over cfg, creating the two accounts if needed.
func open(t *testing.T, cfg config.Config) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(cfg.DataDirOrDefault(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = st.Update(func(s *store.State) error {
		if _, err := s.EnsureAdmin(cfg.Server.Accounts.AdminEmail, time.Now()); err != nil {
			return err
		}
		if s.AccountByEmail("ann@example.com") == nil {
			_, err := s.AddAccount("ann@example.com", false, time.Now())
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	ws, err := NewMulti(context.Background(), Options{Config: cfg, Store: st, Version: "test", AllowLocalRemotes: true,
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	e := &env{t: t, ws: ws, st: st, cfg: cfg, logs: logs}
	e.admin, e.ann = e.as("admin@example.com"), e.as("ann@example.com")
	e.ready()
	return e
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return open(t, baseConfig(t, filepath.Join(t.TempDir(), "data")))
}

func (e *env) as(email string) context.Context {
	a := e.st.Snapshot().AccountByEmail(email)
	if a == nil {
		e.t.Fatalf("no account %s", email)
	}
	return WithCaller(context.Background(), Caller{AccountID: a.ID, Email: a.Email, Admin: a.Admin})
}

func (e *env) ready() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.ws.WaitReady(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// add registers a local repository and waits for it.
func (e *env) add(alias, remote string) RepoView {
	e.t.Helper()
	if _, err := e.ws.AddRepo(context.Background(), AddRepoRequest{Alias: alias, URL: remote}); err != nil {
		e.t.Fatal(err)
	}
	e.ready()
	v, _ := e.ws.Repo(context.Background(), alias)
	if v.Status != StatusOK {
		e.t.Fatalf("repository %s: status %s (%s)", alias, v.Status, v.Reason)
	}
	return v
}

func (e *env) grant(email, alias, path string, level acl.Level) {
	e.t.Helper()
	err := e.st.Update(func(s *store.State) error {
		return s.SetRule(s.AccountByEmail(email).ID, s.RepoByAlias(alias).ID, path, level)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) clear(email, alias, path string) {
	e.t.Helper()
	_ = e.st.Update(func(s *store.State) error {
		s.ClearRule(s.AccountByEmail(email).ID, s.RepoByAlias(alias).ID, path)
		return nil
	})
}

// grantFixture gives ann the rules the access fixture is written for.
func (e *env) grantFixture(alias string) {
	e.grant("ann@example.com", alias, "Open", acl.Write)
	e.grant("ann@example.com", alias, "Open/hidn-closed", acl.None)
	e.grant("ann@example.com", alias, "Shared/Team", acl.Read)
	e.grant("ann@example.com", alias, "ReadOnly", acl.Read)
}

func code(err error) string {
	var ke *kb.Error
	if errors.As(err, &ke) {
		return ke.Code
	}
	if err == nil {
		return "<nil>"
	}
	return "<" + err.Error() + ">"
}

func listed(t *testing.T, w *Workspace, ctx context.Context, folder string, recursive bool) []string {
	t.Helper()
	res, err := w.List(ctx, kb.ListRequest{Folder: folder, Recursive: recursive, Limit: 1000})
	if err != nil {
		t.Fatalf("list %q: %v", folder, err)
	}
	out := []string{}
	for _, en := range res.Entries {
		out = append(out, en.Path)
	}
	return out
}

func eq(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func noLeak(t *testing.T, what string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	for _, tok := range []string{testutil.HiddenToken, testutil.HiddenMarker} {
		if strings.Contains(string(raw), tok) {
			t.Errorf("%s leaks %q: %s", what, tok, raw)
		}
	}
}

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in, kind, host string
		ok             bool
	}{
		{"https://example.com/org/notes.git", remoteHTTPS, "example.com", true},
		{"https://git.example.com:8443/org/notes", remoteHTTPS, "git.example.com", true},
		{"ssh://git@example.com/org/notes.git", remoteSSH, "example.com", true},
		{"ssh://git@example.com:2222/org/notes.git", remoteSSH, "example.com", true},
		{"git@example.com:org/notes.git", remoteSSH, "example.com", true},
		{"https://user:secret@example.com/org/notes.git", "", "", false},
		{"https://token@example.com/org/notes.git", "", "", false},
		{"ssh://git:pw@example.com/org/notes.git", "", "", false},
		{"http://example.com/org/notes.git", "", "", false},
		{"git://example.com/org/notes.git", "", "", false},
		{"file:///srv/notes", "", "", false},
		{"/srv/notes.git", "", "", false},
		{"ext::sh -c 'id'", "", "", false},
		{"ext::ssh://example.com/x", "", "", false},
		{"--upload-pack=touch /tmp/x", "", "", false},
		{"-oProxyCommand=id@example.com:x", "", "", false},
		{"https://example.com/org/notes.git?x=1", "", "", false},
		{"https://example.com/a b", "", "", false},
		{"ssh://example.com", "", "", false},
		{"", "", "", false},
		{"notes", "", "", false},
	}
	for _, c := range cases {
		kind, host, err := parseRemote(c.in, false)
		if (err == nil) != c.ok || kind != c.kind || host != c.host {
			t.Errorf("parseRemote(%q) = %q, %q, %v; want %q, %q, ok=%v", c.in, kind, host, err, c.kind, c.host, c.ok)
		}
	}
	if kind, _, err := parseRemote("/srv/notes.git", true); err != nil || kind != remoteLocal {
		t.Errorf("local paths are for tests only: %q %v", kind, err)
	}
}

func TestAddRepoValidation(t *testing.T) {
	e := newEnv(t)
	_, remote := testutil.NewAccessRepo(t)
	ctx := context.Background()
	cases := []struct {
		name string
		req  AddRepoRequest
		want string
	}{
		{"invalid alias", AddRepoRequest{Alias: "My Notes", URL: remote}, "alias"},
		{"empty alias", AddRepoRequest{URL: remote}, "alias"},
		{"unsupported URL", AddRepoRequest{Alias: "a", URL: "http://example.com/x.git", Token: "t"}, "repository URL"},
		{"credentials in URL", AddRepoRequest{Alias: "a", URL: "https://u:p@example.com/x.git", Token: "t"}, "credentials"}, //nolint:gosec // the case under test: such a URL must be refused
		{"https without token", AddRepoRequest{Alias: "a", URL: "https://example.com/x.git"}, "token is required"},
		{"https with key", AddRepoRequest{Alias: "a", URL: "https://example.com/x.git", GenerateKey: true}, "not an SSH key"},
		{"ssh with token", AddRepoRequest{Alias: "a", URL: "git@example.com:o/x.git", Token: "t"}, "not an access token"},
		{"ssh without key", AddRepoRequest{Alias: "a", URL: "git@example.com:o/x.git"}, "needs an SSH key"},
		{"ssh with both", AddRepoRequest{Alias: "a", URL: "git@example.com:o/x.git", GenerateKey: true, SSHKey: "x"}, "not both"},
		{"bad branch", AddRepoRequest{Alias: "a", URL: remote, Branch: "-x"}, "branch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.ws.AddRepo(ctx, c.req)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, c.want)
			}
			if strings.Contains(err.Error(), "u:p") {
				t.Error("an error must not echo credentials")
			}
		})
	}
	if n := len(e.st.Snapshot().Repos); n != 0 {
		t.Fatalf("rejected requests must store nothing, got %d repositories", n)
	}
	e.add("notes", remote)
	if _, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "notes", URL: remote}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate alias: %v", err)
	}
	if v, _ := e.ws.Repo(ctx, "notes"); v.Status != StatusOK {
		t.Errorf("the existing repository must be unaffected: %+v", v)
	}
}

func TestNewRepositoryIsVisibleToNobody(t *testing.T) {
	e := newEnv(t)
	_, remote := testutil.NewAccessRepo(t)
	e.add("notes", remote)
	if n := len(e.st.Snapshot().Rules); n != 0 {
		t.Fatalf("adding a repository must create no rules, got %d", n)
	}
	for name, ctx := range map[string]context.Context{"administrator": e.admin, "account": e.ann, "anonymous": context.Background()} {
		eq(t, name+" root", listed(t, e.ws, ctx, "", false))
		if _, err := e.ws.GetNote(ctx, "notes/Open/Note.md"); code(err) != kb.CodeNotFound {
			t.Errorf("%s read: %v", name, err)
		}
		if res, err := e.ws.Search(ctx, search.Request{Query: "zebra"}); err != nil || res.Total != 0 {
			t.Errorf("%s search: %+v %v", name, res, err)
		}
		if info := e.ws.Info(ctx).(MultiInfo); len(info.Repositories) != 0 {
			t.Errorf("%s info: %+v", name, info)
		}
	}
	// The administrator grants itself access like anyone else.
	e.grant("admin@example.com", "notes", "", acl.Read)
	eq(t, "administrator root after a grant", listed(t, e.ws, e.admin, "", false), "notes")
	if _, err := e.ws.GetNote(e.admin, "notes/hidn-private/hidn-secret.md"); err != nil {
		t.Errorf("administrator read after a grant: %v", err)
	}
	if _, err := e.ws.PatchNote(e.admin, "notes/Open/Note.md", []kb.PatchOp{{Op: "append", Content: "x"}}, "", ""); code(err) != kb.CodeForbidden {
		t.Errorf("a read rule does not let the administrator write: %v", err)
	}
	eq(t, "another account still sees nothing", listed(t, e.ws, e.ann, "", false))
}

func TestRoutingAndUnifiedTree(t *testing.T) {
	e := newEnv(t)
	_, r1 := testutil.NewAccessRepo(t)
	_, r2 := testutil.NewAccessRepo(t)
	e.add("a", r1)
	e.add("b", r2)
	e.grantFixture("a")
	e.grant("ann@example.com", "b", "", acl.Write)
	ctx := e.ann

	eq(t, "root", listed(t, e.ws, ctx, "", false), "a", "b")
	eq(t, "repository root", listed(t, e.ws, ctx, "a", false), "a/Open", "a/ReadOnly", "a/Shared")
	eq(t, "pass-through", listed(t, e.ws, ctx, "a/Shared", false), "a/Shared/Team")
	all := listed(t, e.ws, ctx, "", true)
	if len(all) != 6+13 || all[0] != "a/Open/Moved.md" || !strings.HasPrefix(all[len(all)-1], "b/") {
		t.Errorf("recursive root listing: %d entries %v", len(all), all)
	}
	// Paging over the combined tree has no gaps or duplicates.
	var paged []string
	cursor := ""
	for {
		res, err := e.ws.List(ctx, kb.ListRequest{Recursive: true, Limit: 4, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range res.Entries {
			paged = append(paged, en.Path)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	eq(t, "paged recursive listing", paged, all...)
	if got := listed(t, e.ws, ctx, "", true); len(got) == 0 {
		t.Fatal("unreachable")
	}
	globbed, err := e.ws.List(ctx, kb.ListRequest{Recursive: true, Glob: "a/**/*.md"})
	if err != nil || len(globbed.Entries) != 5 {
		t.Errorf("glob on the full path: %d %v", len(globbed.Entries), err)
	}

	n, err := e.ws.GetNote(ctx, "a/Open/Note.md")
	if err != nil || n.Path != "a/Open/Note.md" {
		t.Fatalf("read: %+v %v", n, err)
	}
	type call struct {
		name string
		run  func(path string) error
	}
	reads := []call{
		{"get", func(p string) error { _, err := e.ws.GetNote(ctx, p); return err }},
		{"section", func(p string) error { _, err := e.ws.GetSection(ctx, p, "x"); return err }},
		{"backlinks", func(p string) error { _, err := e.ws.Backlinks(ctx, p); return err }},
		{"file", func(p string) error { _, _, err := e.ws.ReadFile(ctx, p, 0); return err }},
		{"history", func(p string) error { _, err := e.ws.History(ctx, p, 0); return err }},
		{"show", func(p string) error { _, err := e.ws.ShowRevision(ctx, p, ""); return err }},
		{"list", func(p string) error { _, err := e.ws.List(ctx, kb.ListRequest{Folder: p}); return err }},
		{"link", func(p string) error {
			_, _, err := e.ws.SignedLink(ctx, "https://kb.example.com", "get", p, 0)
			return err
		}},
	}
	writes := []call{
		{"create", func(p string) error { _, err := e.ws.CreateNote(ctx, p, "x", nil, false, ""); return err }},
		{"replace", func(p string) error { _, err := e.ws.ReplaceNote(ctx, p, "x", "", ""); return err }},
		{"patch", func(p string) error {
			_, err := e.ws.PatchNote(ctx, p, []kb.PatchOp{{Op: "append", Content: "x"}}, "", "")
			return err
		}},
		{"delete", func(p string) error { _, err := e.ws.DeleteNote(ctx, p, "", ""); return err }},
		{"restore", func(p string) error { _, err := e.ws.Restore(ctx, p, "HEAD", ""); return err }},
		{"upload", func(p string) error { _, err := e.ws.PutFile(ctx, p, []byte("x"), true, ""); return err }},
		{"move from", func(p string) error { _, err := e.ws.MoveNote(ctx, p, "b/x.md", ""); return err }},
		{"move to", func(p string) error { _, err := e.ws.MoveNote(ctx, "b/Open/Note.md", p, ""); return err }},
	}
	for _, c := range append(append([]call{}, reads...), writes...) {
		if err := c.run("nope/a.md"); code(err) != kb.CodeNotFound {
			t.Errorf("%s with an unknown alias: %v, want not_found", c.name, err)
		}
		if err := c.run("a/../b/note.md"); code(err) != kb.CodeInvalidPath {
			t.Errorf("%s escaping into another repository: %v, want invalid_path", c.name, err)
		}
		if err := c.run("/etc/passwd"); code(err) != kb.CodeInvalidPath {
			t.Errorf("%s with an absolute path: %v, want invalid_path", c.name, err)
		}
	}
	for _, c := range writes {
		if err := c.run("plan.md"); code(err) != kb.CodeInvalidPath {
			t.Errorf("%s at the root: %v, want invalid_path", c.name, err)
		}
	}
	if _, err := e.ws.GetNote(ctx, "plan.md"); code(err) != kb.CodeNotFound {
		t.Errorf("read at the root: %v", err)
	}
	if _, err := e.ws.MoveNote(ctx, "a/Open/Note.md", "b/Note.md", ""); code(err) != kb.CodeInvalidArgument {
		t.Errorf("move across repositories: %v, want invalid_argument", err)
	}
	if _, err := e.ws.GetNote(ctx, "a/Open/Note.md"); err != nil {
		t.Errorf("a refused move must change nothing: %v", err)
	}
	// Links resolve inside their own repository only.
	bl, err := e.ws.Backlinks(ctx, "b/hidn-private/Target.md")
	if err != nil || len(bl) != 1 || bl[0].Source != "b/Open/Note.md" {
		t.Errorf("backlinks stay inside the repository: %+v %v", bl, err)
	}
	w, err := e.ws.CreateNote(ctx, "b/New/Idea.md", "# Idea\n", nil, false, "")
	if err != nil || w.Path != "b/New/Idea.md" {
		t.Fatalf("write: %+v %v", w, err)
	}
	if _, err := e.ws.GetNote(ctx, "a/New/Idea.md"); code(err) != kb.CodeNotFound {
		t.Errorf("a write must land in one repository only: %v", err)
	}
}

func TestFanOut(t *testing.T) {
	e := newEnv(t)
	_, r1 := testutil.NewAccessRepo(t)
	_, r2 := testutil.NewAccessRepo(t)
	e.add("a", r1)
	e.add("b", r2)
	e.grantFixture("a")
	e.grantFixture("b")
	ctx := e.ann

	res, err := e.ws.Search(ctx, search.Request{Query: "zebra", Limit: 20})
	if err != nil || res.Total != 8 || len(res.Hits) != 8 {
		t.Fatalf("search across two repositories: total=%d hits=%d err=%v", res.Total, len(res.Hits), err)
	}
	noLeak(t, "search", res)
	inA, inB := 0, 0
	for i, h := range res.Hits {
		if strings.HasPrefix(h.Path, "a/") {
			inA++
		} else if strings.HasPrefix(h.Path, "b/") {
			inB++
		}
		if i > 0 && res.Hits[i-1].Score < h.Score {
			t.Errorf("hits are not ranked: %v", res.Hits)
		}
	}
	if inA != 4 || inB != 4 {
		t.Errorf("hits per repository: a=%d b=%d", inA, inB)
	}
	seen := map[string]bool{}
	for off := 0; off < 8; off += 3 {
		page, err := e.ws.Search(ctx, search.Request{Query: "zebra", Limit: 3, Offset: off})
		if err != nil || page.Total != 8 {
			t.Fatal(page, err)
		}
		for _, h := range page.Hits {
			if seen[h.Path] {
				t.Errorf("duplicate across pages: %s", h.Path)
			}
			seen[h.Path] = true
		}
	}
	if len(seen) != 8 {
		t.Errorf("paging returned %d distinct hits, want 8", len(seen))
	}
	narrowed, err := e.ws.Search(ctx, search.Request{Query: "zebra", Filters: search.Filters{Folder: "a"}})
	if err != nil || narrowed.Total != 4 {
		t.Errorf("narrowing to one repository: %+v %v", narrowed, err)
	}
	for _, h := range narrowed.Hits {
		if !strings.HasPrefix(h.Path, "a/") {
			t.Errorf("hit outside the folder: %s", h.Path)
		}
	}
	if deeper, _ := e.ws.Search(ctx, search.Request{Query: "zebra", Filters: search.Filters{Folder: "b/Open"}}); deeper.Total != 2 {
		t.Errorf("narrowing to a folder: %+v", deeper)
	}
	for _, folder := range []string{"nope", "a/hidn-private", "a/Absent"} {
		if none, err := e.ws.Search(ctx, search.Request{Query: "zebra", Filters: search.Filters{Folder: folder}}); err != nil || none.Total != 0 {
			t.Errorf("search in %s: %+v %v", folder, none, err)
		}
	}

	g, err := e.ws.Grep(ctx, search.GrepRequest{Pattern: "zebra"})
	if err != nil || g.Total != 8 || len(g.Files) != 8 || g.Files[0].Path != "a/Open/Note.md" {
		t.Errorf("grep: %+v %v", g, err)
	}
	noLeak(t, "grep", g)
	if g, _ := e.ws.Grep(ctx, search.GrepRequest{Pattern: "zebra", Limit: 5}); g.Total != 5 || !g.Truncated {
		t.Errorf("grep limit across repositories: %+v", g)
	}
	if g, _ := e.ws.Grep(ctx, search.GrepRequest{Pattern: "zebra", Folder: "b/Shared"}); g.Total != 1 {
		t.Errorf("grep in a folder: %+v", g)
	}

	rows, err := e.ws.Query(ctx, search.QueryRequest{Sort: "path desc", Limit: 3})
	if err != nil || len(rows) != 3 || rows[0]["path"] != "b/Shared/Team/Plan.md" {
		t.Errorf("query with a combined sort and limit: %+v %v", rows, err)
	}
	if rows, _ := e.ws.Query(ctx, search.QueryRequest{}); len(rows) != 10 {
		t.Errorf("query: %d rows, want 10", len(rows))
	}
	noLeak(t, "query", rows)

	tags := e.ws.Tags(ctx, "")
	if len(tags) != 1 || tags[0].Tag != "open" || tags[0].Count != 4 {
		t.Errorf("tags are summed over repositories: %+v", tags)
	}
	c := e.ws.QuickOpen(ctx, "plan", 10)
	if len(c) != 2 || c[0].Path != "a/Shared/Team/Plan.md" || c[1].Path != "b/Shared/Team/Plan.md" {
		t.Errorf("quick open: %+v", c)
	}
	if c := e.ws.QuickOpen(ctx, "plan", 1); len(c) != 1 {
		t.Errorf("quick open limit: %+v", c)
	}
	bundle, err := e.ws.Context(ctx, search.ContextRequest{Query: "zebra"})
	if err != nil || len(bundle.Sources) != 8 {
		t.Errorf("context across repositories: %+v %v", bundle.Sources, err)
	}
	noLeak(t, "context", bundle)
	if one, _ := e.ws.Context(ctx, search.ContextRequest{Query: "zebra", Filters: search.Filters{Folder: "b"}}); len(one.Sources) != 4 {
		t.Errorf("context in one repository: %+v", one.Sources)
	}
}

func TestHistoryAndSyncAcrossRepositories(t *testing.T) {
	e := newEnv(t)
	_, r1 := testutil.NewAccessRepo(t)
	_, r2 := testutil.NewAccessRepo(t)
	e.add("a", r1)
	e.add("b", r2)
	e.grant("ann@example.com", "a", "", acl.Write)
	ctx := e.ann

	// One reachable repository: tools that need one take it.
	lg, err := e.ws.Log(ctx, kb.LogRequest{Limit: 1})
	if err != nil || len(lg.Commits) != 1 || !strings.HasPrefix(lg.Commits[0].Changes[0].Path, "a/") {
		t.Fatalf("log with a single repository: %+v %v", lg, err)
	}
	if _, err := e.ws.Diff(ctx, "HEAD~1", "", ""); err != nil {
		t.Errorf("diff with a single repository: %v", err)
	}
	if tree, err := e.ws.LsTree(ctx, "", ""); err != nil || tree[0].Path != "a/Open" {
		t.Errorf("ls-tree with a single repository: %+v %v", tree, err)
	}

	e.grant("ann@example.com", "b", "", acl.Write)
	_, err = e.ws.Log(ctx, kb.LogRequest{})
	if code(err) != kb.CodeInvalidArgument || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("ambiguous log: %v", err)
	}
	if _, err := e.ws.Diff(ctx, "HEAD~1", "", ""); code(err) != kb.CodeInvalidArgument {
		t.Errorf("ambiguous diff: %v", err)
	}
	if _, err := e.ws.LsTree(ctx, "", ""); code(err) != kb.CodeInvalidArgument {
		t.Errorf("ambiguous ls-tree: %v", err)
	}
	lg, err = e.ws.Log(ctx, kb.LogRequest{Folder: "b", Limit: 2})
	if err != nil || len(lg.Commits) != 2 || !strings.HasPrefix(lg.Commits[0].Changes[0].Path, "b/") {
		t.Errorf("log of one repository: %+v %v", lg, err)
	}
	if d, err := e.ws.Diff(ctx, "HEAD~1", "HEAD", "b"); err != nil || !strings.Contains(d, "b/b/Open/Moved.md") {
		t.Errorf("diff of one repository: %v\n%s", err, d)
	}

	st, err := e.ws.Status(ctx, "")
	rep, _ := st.(SyncReport)
	if err != nil || len(rep.Repositories) != 2 || rep.Repositories[0].Repository != "a" || rep.Repositories[0].State != "ok" {
		t.Fatalf("status of every repository: %+v %v", st, err)
	}
	one, err := e.ws.SyncNow(ctx, "a")
	if rep := one.(SyncReport); err != nil || len(rep.Repositories) != 1 || rep.Repositories[0].Repository != "a" {
		t.Errorf("sync of one repository: %+v %v", one, err)
	}
	if _, err := e.ws.Status(ctx, "nope"); code(err) != kb.CodeNotFound {
		t.Errorf("status of an unknown repository: %v", err)
	}

	// A frozen merge in a does not block b.
	other := testutil.CloneRemote(t, r1)
	testutil.WriteAndPush(t, other, "Open/Note.md", "# Open note\n\nremote\n", "remote edit")
	// The local side changes the same note without having seen the remote edit.
	m, _ := e.ws.mountByAlias("a")
	if err := os.WriteFile(filepath.Join(m.svc.Vault().Root, "Open", "Note.md"), []byte("# Open note\n\nlocal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, m.svc.Vault().Root, "commit", "-q", "-am", "local edit")
	all, err := e.ws.SyncNow(ctx, "")
	rep = all.(SyncReport)
	if err != nil || len(rep.Repositories) != 2 {
		t.Fatalf("sync of every repository: %+v %v", all, err)
	}
	if rep.Repositories[0].Error == nil || rep.Repositories[0].Error.Code != kb.CodeMergeConflict || rep.Repositories[0].State != "conflict" {
		t.Errorf("repository a must report its conflict: %+v", rep.Repositories[0])
	}
	if rep.Repositories[1].Error != nil || rep.Repositories[1].State != "ok" {
		t.Errorf("repository b must be unaffected: %+v", rep.Repositories[1])
	}
	if _, err := e.ws.SyncNow(ctx, "a"); code(err) != kb.CodeMergeConflict {
		t.Errorf("sync of the conflicted repository alone: %v", err)
	}
	if _, err := e.ws.PatchNote(ctx, "a/Open/Sub/Deep.md", []kb.PatchOp{{Op: "append", Content: "x"}}, "", ""); code(err) != kb.CodeMergeConflict {
		t.Errorf("write in the conflicted repository: %v", err)
	}
	if _, err := e.ws.PatchNote(ctx, "b/Open/Sub/Deep.md", []kb.PatchOp{{Op: "append", Content: "x"}}, "", ""); err != nil {
		t.Errorf("write in the other repository must succeed: %v", err)
	}
	cf, err := e.ws.Conflicts(ctx, "")
	if crep := cf.(ConflictsReport); err != nil || len(crep.Repositories) != 1 || crep.Repositories[0].Repository != "a" || crep.Repositories[0].Conflicts[0].Path != "a/Open/Note.md" {
		t.Errorf("conflicts: %+v %v", cf, err)
	}
	if _, err := e.ws.ResolveConflict(ctx, "", []kb.Resolution{{Path: "a/Open/Note.md", Take: "ours"}, {Path: "b/Open/Note.md", Take: "ours"}}); code(err) != kb.CodeInvalidArgument {
		t.Errorf("one call resolves one repository: %v", err)
	}
	done, err := e.ws.ResolveConflict(ctx, "", []kb.Resolution{{Path: "a/Open/Note.md", Take: "ours"}})
	if err != nil || !done.Complete || done.Resolved[0] != "a/Open/Note.md" {
		t.Errorf("resolve: %+v %v", done, err)
	}

	info := e.ws.Info(ctx).(MultiInfo)
	if info.Account != "ann@example.com" || len(info.Repositories) != 2 || info.Repositories[0].Alias != "a" || info.Repositories[0].NoteCount != 11 {
		t.Errorf("info: %+v", info)
	}
	raw, _ := json.Marshal(info)
	if strings.Contains(string(raw), e.cfg.DataDirOrDefault()) || strings.Contains(string(raw), r1) {
		t.Errorf("info must not carry filesystem paths or remote URLs: %s", raw)
	}

	if _, err := e.ws.Reindex(ctx, ""); code(err) != kb.CodeForbidden {
		t.Errorf("reindex by a regular account: %v", err)
	}
	// The administrator holds no rule, yet may rebuild any index; the result
	// says nothing about content.
	out, err := e.ws.Reindex(e.admin, "b")
	raw, _ = json.Marshal(out)
	if err != nil || string(raw) != `{"repositories":[{"repository":"b","status":"reindexed"}]}` {
		t.Errorf("reindex by the administrator: %s %v", raw, err)
	}
	if _, err := e.ws.Reindex(e.admin, "nope"); code(err) != kb.CodeNotFound {
		t.Errorf("reindex of an unknown repository: %v", err)
	}
}

func TestRuleChangesApplyAtOnceAndLinks(t *testing.T) {
	e := newEnv(t)
	_, remote := testutil.NewAccessRepo(t)
	e.add("notes", remote)
	e.grantFixture("notes")
	ctx := e.ann
	base := "https://kb.example.com"

	if _, err := e.ws.GetNote(ctx, "notes/Open/Note.md"); err != nil {
		t.Fatal(err)
	}
	e.grant("ann@example.com", "notes", "Open", acl.None)
	if _, err := e.ws.GetNote(ctx, "notes/Open/Note.md"); code(err) != kb.CodeNotFound {
		t.Errorf("a rule change must apply to the next call of the same caller: %v", err)
	}
	e.grant("ann@example.com", "notes", "Open", acl.Write)

	verify := func(link, kind string) bool {
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		route := "/files/"
		if kind == "put" {
			route = "/upload/"
		}
		_, ok := e.ws.VerifyLink(context.Background(), kind, strings.TrimPrefix(u.Path, route), u.Query())
		return ok
	}
	get, exp, err := e.ws.SignedLink(ctx, base, "get", "notes/Open/file.bin", time.Minute)
	if err != nil || !strings.HasPrefix(get, base+"/files/notes/Open/file.bin?") || time.Until(exp) < 30*time.Second {
		t.Fatalf("download link: %s %v", get, err)
	}
	if !strings.Contains(get, "u=") {
		t.Error("a link of a multi-user server names its issuer")
	}
	if !verify(get, "get") {
		t.Fatal("a fresh link must verify")
	}
	if verify(strings.Replace(get, "/files/notes/", "/files/other/", 1), "get") {
		t.Error("a link with another alias must not verify")
	}
	if verify(strings.Replace(get, "file.bin", "Note.md", 1), "get") {
		t.Error("a link for another file must not verify")
	}
	adminID := e.st.Snapshot().AccountByEmail("admin@example.com").ID
	annID := e.st.Snapshot().AccountByEmail("ann@example.com").ID
	if verify(strings.Replace(get, "u="+annID, "u="+adminID, 1), "get") {
		t.Error("a link cannot be moved to another account")
	}
	if verify(get, "put") {
		t.Error("a download link is not an upload link")
	}
	for name, p := range map[string]string{"hidden": "notes/hidn-private/hidn-blob.bin", "missing": "notes/Open/absent.bin"} {
		if _, _, err := e.ws.SignedLink(ctx, base, "get", p, 0); code(err) != kb.CodeNotFound {
			t.Errorf("download link for a %s file: %v", name, err)
		}
	}
	if _, _, err := e.ws.SignedLink(ctx, base, "put", "notes/ReadOnly/", 0); code(err) != kb.CodeForbidden {
		t.Errorf("upload link into a read-only folder: %v", err)
	}
	if _, _, err := e.ws.SignedLink(ctx, "", "get", "notes/Open/file.bin", 0); code(err) != kb.CodeInvalidArgument {
		t.Errorf("link without a public URL: %v", err)
	}
	put, _, err := e.ws.SignedLink(ctx, base, "put", "notes/Open/", time.Minute)
	if err != nil || !strings.Contains(put, "/upload/notes/Open/?") || !verify(put, "put") {
		t.Fatalf("upload link: %s %v", put, err)
	}

	// Access removed after the link was issued.
	e.grant("ann@example.com", "notes", "Open", acl.Read)
	if verify(put, "put") {
		t.Error("an upload link must stop working when write access is removed")
	}
	if !verify(get, "get") {
		t.Error("the download link still has the read level it needs")
	}
	e.grant("ann@example.com", "notes", "Open", acl.None)
	if verify(get, "get") {
		t.Error("a download link must stop working when access is removed")
	}
	e.grant("ann@example.com", "notes", "Open", acl.Write)
	if !verify(get, "get") {
		t.Fatal("restoring access restores the link")
	}
	if err := e.st.Update(func(s *store.State) error { s.Accounts[annID].Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if verify(get, "get") {
		t.Error("a link issued by a since-disabled account must not work")
	}
	if _, err := e.ws.GetNote(ctx, "notes/Open/Note.md"); code(err) != kb.CodeNotFound {
		t.Errorf("a disabled account sees nothing: %v", err)
	}
}

func TestImportConfiguredVault(t *testing.T) {
	vaultDir, _ := testutil.NewAccessRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := baseConfig(t, dataDir)
	cfg.Vault.Path = vaultDir
	cfg.Vault.Alias = "main"
	cfg.Git.Token = "ghp_environment_token_value"

	// The deployment ran in classic mode before: its index exists.
	classic, err := kb.Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := classic.Close(); err != nil {
		t.Fatal(err)
	}

	e := open(t, cfg)
	v, ok := e.ws.Repo(context.Background(), "main")
	if !ok || v.Status != StatusOK || v.Credential != store.CredHost {
		t.Fatalf("imported repository: %+v", v)
	}
	logs := e.logs.String()
	if strings.Contains(logs, "cloning vault") || strings.Contains(logs, "building index") || !strings.Contains(logs, "index up to date") {
		t.Errorf("the import must reuse clone and index in place:\n%s", logs)
	}
	r := e.st.Snapshot().RepoByAlias("main")
	if r.VaultPath != vaultDir || len(r.Secret) != 0 {
		t.Errorf("registry entry: %+v", r)
	}
	state, _ := os.ReadFile(e.st.Path())
	if strings.Contains(string(state), cfg.Git.Token) {
		t.Error("the environment token must not be copied into the state file")
	}
	// The administrator, and nobody else, can use the migrated vault.
	eq(t, "administrator root", listed(t, e.ws, e.admin, "", false), "main")
	if _, err := e.ws.PatchNote(e.admin, "main/Open/Note.md", []kb.PatchOp{{Op: "append", Content: "x"}}, "", ""); err != nil {
		t.Errorf("administrator write: %v", err)
	}
	if msg := testutil.Git(t, vaultDir, "log", "-1", "--format=%B"); !strings.Contains(msg, "KB-User: admin@example.com") || !strings.HasPrefix(msg, "kb_patch_note: Open/Note.md") {
		t.Errorf("commit message: %q", msg)
	}
	eq(t, "another account", listed(t, e.ws, e.ann, "", false))
	// The rule is an ordinary one.
	e.clear("admin@example.com", "main", "")
	eq(t, "administrator without the rule", listed(t, e.ws, e.admin, "", false))

	// Restart: served again, not imported twice.
	_ = e.ws.Close()
	e2 := open(t, cfg)
	if n := len(e2.st.Snapshot().Repos); n != 1 {
		t.Fatalf("repositories after restart = %d", n)
	}
	if err := e2.ws.RemoveRepo(context.Background(), "main", true); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(vaultDir); len(entries) != 0 {
		t.Errorf("the imported clone must be emptied: %d entries left", len(entries))
	}
	if _, err := os.Stat(e2.st.Path()); err != nil {
		t.Errorf("removing the imported repository must keep the state file: %v", err)
	}
	_ = e2.ws.Close()
	e3 := open(t, cfg)
	if n := len(e3.st.Snapshot().Repos); n != 0 {
		t.Errorf("a removed import must not come back on restart: %d repositories", n)
	}
}

func TestRestartAndSecretKey(t *testing.T) {
	_, r1 := testutil.NewAccessRepo(t)
	_, r2 := testutil.NewAccessRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := baseConfig(t, dataDir)
	e := open(t, cfg)
	if _, err := e.ws.AddRepo(context.Background(), AddRepoRequest{Alias: "one", URL: r1, Token: "token-number-one"}); err != nil {
		t.Fatal(err)
	}
	e.ready()
	e.grant("ann@example.com", "one", "", acl.Read)
	state, _ := os.ReadFile(e.st.Path())
	if strings.Contains(string(state), "token-number-one") {
		t.Fatal("the state file must not contain a token in clear text")
	}
	if v, _ := e.ws.Repo(context.Background(), "one"); v.Hint != "-one" || v.Status != StatusOK {
		t.Errorf("only the last four characters of a token are shown: %+v", v)
	}
	if strings.Contains(e.logs.String(), "token-number-one") {
		t.Error("a token must never be logged")
	}
	_ = e.ws.Close()

	// Same key: everything is served again without being added again.
	e = open(t, cfg)
	if v, _ := e.ws.Repo(context.Background(), "one"); v.Status != StatusOK {
		t.Fatalf("after restart: %+v", v)
	}
	if _, err := e.ws.GetNote(e.ann, "one/Open/Note.md"); err != nil {
		t.Errorf("read after restart: %v", err)
	}
	_ = e.ws.Close()

	// Another key: the repository runs on its last clone and asks for a
	// credential; a repository added under the new key is unaffected.
	cfg.Server.Accounts.SecretKey = testKey(9)
	e = open(t, cfg)
	v, _ := e.ws.Repo(context.Background(), "one")
	if v.Status != StatusCredentialInvalid {
		t.Fatalf("with another secret key: %+v", v)
	}
	if _, err := e.ws.GetNote(e.ann, "one/Open/Note.md"); err != nil {
		t.Errorf("the last clone keeps being served: %v", err)
	}
	e.add("two", r2)
	if err := e.ws.ReplaceCredential(context.Background(), "one", AddRepoRequest{Token: "token-number-two"}); err != nil {
		t.Fatal(err)
	}
	e.ready()
	if v, _ := e.ws.Repo(context.Background(), "one"); v.Status != StatusOK || v.Hint != "-two" {
		t.Errorf("after entering the credential again: %+v", v)
	}
}

func TestFailedCloneStaysRegistered(t *testing.T) {
	e := newEnv(t)
	missing := filepath.Join(t.TempDir(), "absent.git")
	v, err := e.ws.AddRepo(context.Background(), AddRepoRequest{Alias: "gone", URL: missing})
	if err != nil || v.Status != StatusCloning {
		t.Fatalf("add: %+v %v", v, err)
	}
	e.ready()
	v, _ = e.ws.Repo(context.Background(), "gone")
	if v.Status != StatusError || v.Reason == "" {
		t.Fatalf("a failed clone must leave the repository registered in the error state: %+v", v)
	}
	e.grant("ann@example.com", "gone", "", acl.Read)
	if _, err := e.ws.GetNote(e.ann, "gone/a.md"); code(err) != kb.CodeNotFound {
		t.Errorf("a repository that is not ready serves nothing: %v", err)
	}
	eq(t, "root", listed(t, e.ws, e.ann, "", false))
	// Once the remote exists a retry succeeds.
	_, remote := testutil.NewAccessRepo(t)
	if err := os.Rename(remote, missing); err != nil {
		t.Fatal(err)
	}
	if err := e.ws.Connect("gone"); err != nil {
		t.Fatal(err)
	}
	e.ready()
	if v, _ := e.ws.Repo(context.Background(), "gone"); v.Status != StatusOK {
		t.Errorf("after a retry: %+v", v)
	}
	eq(t, "root after the retry", listed(t, e.ws, e.ann, "", false), "gone")
}

func TestRemoveRepo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, remote := testutil.NewAccessRepo(t)
	v := e.add("notes", remote)
	e.grant("ann@example.com", "notes", "", acl.Write)
	dir := filepath.Join(e.cfg.DataDirOrDefault(), "repos", v.ID)
	head := func() string { return strings.TrimSpace(testutil.Git(t, remote, "rev-parse", "main")) }

	t.Run("unpushed work is pushed first", func(t *testing.T) {
		before := head()
		m, _ := e.ws.mountByAlias("notes")
		// A commit the push loop has not sent yet.
		if err := os.WriteFile(filepath.Join(m.svc.Vault().Root, "Open", "local.md"), []byte("# Local\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.Git(t, m.svc.Vault().Root, "add", "-A")
		testutil.Git(t, m.svc.Vault().Root, "commit", "-q", "-m", "local only")
		if err := e.ws.RemoveRepo(ctx, "notes", false); err != nil {
			t.Fatal(err)
		}
		if head() == before {
			t.Error("local commits must reach the remote before the clone is deleted")
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("local files must be gone: %v", err)
		}
		if _, err := e.ws.GetNote(e.ann, "notes/Open/Note.md"); code(err) != kb.CodeNotFound {
			t.Errorf("paths of a removed repository: %v", err)
		}
		if n := len(e.st.Snapshot().Rules); n != 0 {
			t.Errorf("rules of a removed repository must go: %d left", n)
		}
		if out := testutil.Git(t, remote, "log", "--oneline", "main"); !strings.Contains(out, "local only") {
			t.Errorf("the remote must hold the pushed commit and be otherwise untouched: %s", out)
		}
	})

	t.Run("unreachable remote needs a confirmation", func(t *testing.T) {
		_, remote := testutil.NewAccessRepo(t)
		v := e.add("off", remote)
		dir := filepath.Join(e.cfg.DataDirOrDefault(), "repos", v.ID)
		m, _ := e.ws.mountByAlias("off")
		if err := os.WriteFile(filepath.Join(m.svc.Vault().Root, "Open", "local.md"), []byte("# Local\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.Git(t, m.svc.Vault().Root, "add", "-A")
		testutil.Git(t, m.svc.Vault().Root, "commit", "-q", "-m", "local only")
		if err := os.Rename(remote, remote+".away"); err != nil {
			t.Fatal(err)
		}
		err := e.ws.RemoveRepo(ctx, "off", false)
		var ue *UnpushedError
		if !errors.As(err, &ue) || ue.Commits != 1 {
			t.Fatalf("removal with unpushed work: %v", err)
		}
		if _, ok := e.ws.Repo(ctx, "off"); !ok {
			t.Fatal("a refused removal must keep the repository")
		}
		if err := e.ws.RemoveRepo(ctx, "off", true); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("local files must be gone: %v", err)
		}
	})

	if err := e.ws.RemoveRepo(ctx, "nope", false); err == nil {
		t.Error("removing an unknown repository must fail")
	}
}

func TestDataDirLock(t *testing.T) {
	e := newEnv(t)
	_, err := NewMulti(context.Background(), Options{Config: e.cfg, Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("a second instance on the same data directory must be refused: %v", err)
	}
}

func TestClassicWorkspaceIsUnrestricted(t *testing.T) {
	dir, _ := testutil.NewAccessRepo(t)
	cfg := config.Default()
	cfg.Vault.Path = dir
	cfg.Search.IndexDir = filepath.Join(t.TempDir(), "idx")
	cfg.Git.PushDebounce, cfg.Git.PullInterval = 0, 0
	svc, err := kb.Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	w := NewClassic(svc, nil)
	t.Cleanup(func() { _ = w.Close() })
	ctx := context.Background()
	if w.Multi() {
		t.Fatal("classic workspace reports multi")
	}
	if _, err := w.GetNote(ctx, "hidn-private/hidn-secret.md"); err != nil {
		t.Errorf("classic read: %v", err)
	}
	res, err := w.PatchNote(ctx, "Open/Note.md", []kb.PatchOp{{Op: "append", Content: "x"}}, "", "")
	if err != nil || res.Path != "Open/Note.md" {
		t.Errorf("classic write keeps unprefixed paths: %+v %v", res, err)
	}
	if st, err := w.Status(ctx, ""); err != nil {
		t.Error(err)
	} else if _, ok := st.(kb.SyncStatus); !ok {
		t.Errorf("classic status keeps its shape: %T", st)
	}
	if _, ok := w.Info(ctx).(kb.Info); !ok {
		t.Error("classic info keeps its shape")
	}
	link, _, err := w.SignedLink(ctx, "https://kb.example.com", "get", "Open/file.bin", 0)
	if err != nil || strings.Contains(link, "u=") {
		t.Errorf("classic links keep their format: %s %v", link, err)
	}
}
