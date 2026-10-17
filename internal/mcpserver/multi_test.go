package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

// multi is a multi-user server over one repository ("notes", the access
// fixture) with an in-process session that acts as a chosen account.
type multi struct {
	t    *testing.T
	srv  *Server
	ws   *workspace.Workspace
	st   *store.Store
	sess *mcp.ClientSession
}

func newMulti(t *testing.T) *multi {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := config.Default()
	cfg.Server.HTTP.Listen = "127.0.0.1:0"
	cfg.Server.DataDir = dataDir
	cfg.Search.IndexDir = filepath.Join(dataDir, "index")
	cfg.Server.Accounts = config.Accounts{AdminEmail: "admin@example.com", MailFrom: "kb@example.com", ResendAPIKey: "re_test",
		SecretKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))}
	cfg.Git.PushDebounce, cfg.Git.PullInterval, cfg.Git.PullFreshness = 0, 0, 0
	st, err := store.Open(filepath.Join(dataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *store.State) error {
		if _, err := s.EnsureAdmin("admin@example.com", time.Now()); err != nil {
			return err
		}
		_, err := s.AddAccount("ann@example.com", false, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws, err := workspace.NewMulti(context.Background(), workspace.Options{Config: cfg, Store: st, Log: log, Version: "test", AllowLocalRemotes: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	_, remote := testutil.NewAccessRepo(t)
	if _, err := ws.AddRepo(context.Background(), workspace.AddRepoRequest{Alias: "notes", URL: remote}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ws.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	m := &multi{t: t, ws: ws, st: st, srv: NewWorkspace(ws, "test", log)}
	m.srv.SetPublicURL("https://kb.example.com")
	m.grant("Open", acl.Write)
	m.grant("Open/hidn-closed", acl.None)
	m.grant("Shared/Team", acl.Read)
	m.grant("ReadOnly", acl.Read)
	m.as("ann@example.com")
	ct, stt := mcp.NewInMemoryTransports()
	go func() { _ = m.srv.MCP().Run(context.Background(), stt) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	m.sess, err = client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.sess.Close() })
	return m
}

func (m *multi) grant(path string, level acl.Level) {
	m.t.Helper()
	if err := m.st.Update(func(s *store.State) error {
		return s.SetRule(s.AccountByEmail("ann@example.com").ID, s.RepoByAlias("notes").ID, path, level)
	}); err != nil {
		m.t.Fatal(err)
	}
}

// as makes every following call act as the given account.
func (m *multi) as(email string) {
	a := m.st.Snapshot().AccountByEmail(email)
	m.srv.testCaller = &workspace.Caller{AccountID: a.ID, Email: a.Email, Admin: a.Admin}
}

// everything returns all a tool result shows to the client, as one string.
func everything(res *mcp.CallToolResult) string {
	var b strings.Builder
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		b.Write(raw)
	}
	for _, c := range res.Content {
		raw, _ := json.Marshal(c)
		b.Write(raw)
		if er, ok := c.(*mcp.EmbeddedResource); ok && er.Resource != nil {
			b.Write(er.Resource.Blob)
			b.WriteString(er.Resource.Text)
		}
	}
	return b.String()
}

func TestMultiToolSurface(t *testing.T) {
	m := newMulti(t)
	res, err := m.sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != wantTools {
		t.Errorf("tool count = %d, want %d: tool names do not change in multi-user mode", len(res.Tools), wantTools)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		if !strings.HasPrefix(tool.Name, "kb_") {
			t.Errorf("tool %s lacks the kb_ prefix", tool.Name)
		}
		byName[tool.Name] = tool
	}
	for _, name := range []string{"kb_get_note", "kb_list", "kb_search", "kb_create_note", "kb_move_note", "kb_log", "kb_diff", "kb_get_file", "kb_upload_link"} {
		if !strings.Contains(byName[name].Description, "repository alias") {
			t.Errorf("%s must explain alias-prefixed paths in multi-user mode: %q", name, byName[name].Description)
		}
	}
	for _, name := range []string{"kb_sync_status", "kb_sync_now", "kb_conflicts", "kb_reindex"} {
		raw, _ := json.Marshal(byName[name].InputSchema)
		if !strings.Contains(string(raw), `"repository"`) {
			t.Errorf("%s must take an optional repository: %s", name, raw)
		}
	}
	if init := m.sess.InitializeResult(); !strings.Contains(init.Instructions, "repository alias") {
		t.Errorf("instructions must describe the unified tree: %q", init.Instructions)
	}

	// A classic server says nothing about aliases.
	classic, _ := newSession(t, false)
	cres, _ := classic.ListTools(context.Background(), nil)
	for _, tool := range cres.Tools {
		raw, _ := json.Marshal(tool)
		if strings.Contains(string(raw), "alias") || strings.Contains(string(raw), `"repository"`) {
			t.Errorf("classic tool %s mentions aliases or repositories: %s", tool.Name, raw)
		}
	}
	if init := classic.InitializeResult(); strings.Contains(init.Instructions, "alias") {
		t.Errorf("classic instructions mention aliases: %q", init.Instructions)
	}
}

func TestMultiResources(t *testing.T) {
	m := newMulti(t)
	ctx := context.Background()
	read := func(uri string) (*mcp.ReadResourceResult, error) {
		return m.sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	}
	note, err := read("kb://note/notes/Open/Note.md")
	if err != nil || !strings.Contains(note.Contents[0].Text, "Public zebra") {
		t.Fatalf("readable note resource: %v", err)
	}
	root, err := read("kb://folder/")
	if err != nil || !strings.Contains(root.Contents[0].Text, `"path":"notes"`) {
		t.Fatalf("root resource: %v %+v", err, root)
	}
	shared, err := read("kb://folder/notes/Shared")
	if err != nil || strings.Contains(shared.Contents[0].Text, testutil.HiddenToken) || !strings.Contains(shared.Contents[0].Text, "notes/Shared/Team") {
		t.Errorf("pass-through folder resource: %v", err)
	}
	if file, err := read("kb://file/notes/Open/file.bin"); err != nil || string(file.Contents[0].Blob) != "open-bytes" {
		t.Errorf("readable file resource: %v", err)
	}
	for _, uri := range []string{
		"kb://note/notes/hidn-private/hidn-secret.md", "kb://note/notes/hidn-private/absent.md", "kb://note/Open/Note.md",
		"kb://file/notes/hidn-private/hidn-blob.bin", "kb://folder/notes/hidn-private", "kb://folder/nope", "kb://note/nope/a.md",
	} {
		res, err := read(uri)
		if err == nil {
			t.Errorf("%s must not be found: %+v", uri, res)
			continue
		}
		if strings.Contains(err.Error(), testutil.HiddenMarker) {
			t.Errorf("%s: the error leaks content: %v", uri, err)
		}
	}
	// A hidden resource and a missing one are reported alike.
	_, hidden := read("kb://note/notes/hidn-private/hidn-secret.md")
	_, missing := read("kb://note/notes/hidn-private/absent.md")
	if strings.ReplaceAll(hidden.Error(), "hidn-secret", "x") != strings.ReplaceAll(missing.Error(), "absent", "x") {
		t.Errorf("hidden: %v\nmissing: %v", hidden, missing)
	}
}

// leakCases gives, for every tool, calls a restricted account might make to
// get at hidden content. Every registered tool must have at least one case:
// the test fails for a tool that is missing here, so a new tool cannot be
// added without deciding how it treats hidden content.
var leakCases = map[string][]map[string]any{
	"kb_get_note":    {{"path": "notes/Open/Note.md"}, {"path": "notes/hidn-private/hidn-secret.md"}, {"path": "notes/Open/hidn-closed/hidn-note.md"}, {"path": "notes/hidn-private/Target.md"}},
	"kb_get_section": {{"path": "notes/Open/Note.md", "heading": "Open note"}, {"path": "notes/hidn-private/hidn-secret.md", "heading": "Hidn secret"}},
	"kb_list": {{}, {"recursive": true}, {"folder": "notes"}, {"folder": "notes", "recursive": true}, {"folder": "notes/Shared"}, {"folder": "notes/Open", "recursive": true},
		{"folder": "notes/hidn-private"}, {"folder": "notes/Open/hidn-closed"}, {"recursive": true, "glob": "**/hidn*"}},
	"kb_backlinks": {{"path": "notes/Open/Note.md"}, {"path": "notes/hidn-private/Target.md"}, {"path": "notes/Target"}, {"path": "notes/hidn-secret"}},
	"kb_tags":      {{}, {"prefix": "hidn"}},
	"kb_search": {{"query": "zebra", "limit": 50}, {"query": "QUOKKA*"}, {"query": "hidn*"}, {"query": "budget"}, {"query": "zebra", "filters": map[string]any{"folder": "notes/hidn-private"}},
		{"query": "zebra", "filters": map[string]any{"frontmatter": map[string]any{"type": "hidnkind"}}}, {"query": "zebra", "filters": map[string]any{"tags": []string{"hidnonly"}}}},
	"kb_grep": {{"pattern": "zebra"}, {"pattern": "QUOKKA"}, {"pattern": "hidn"}, {"pattern": ".", "regex": true, "glob": "**/hidn*"}, {"pattern": "zebra", "folder": "notes/hidn-private"},
		{"pattern": "zebra", "folder": "notes/Open", "context_lines": 3}},
	"kb_query":         {{}, {"where": `type eq "hidnkind"`}, {"where": `path contains "hidn"`}, {"where": `tags contains "hidnonly"`}, {"sort": "path desc", "select": []string{"folder", "tags", "type"}}},
	"kb_context":       {{"query": "zebra"}, {"query": "QUOKKA*"}, {"query": "zebra budget", "max_chars": 20000}, {"query": "zebra", "filters": map[string]any{"folder": "notes/hidn-private"}}},
	"kb_quick_open":    {{"text": "hidn"}, {"text": "secret"}, {"text": "Target"}, {"text": "note"}},
	"kb_log":           {{}, {"limit": 100}, {"folder": "notes"}, {"folder": "notes/hidn-private"}, {"folder": "notes/Open"}, {"since": "2000-01-01"}},
	"kb_history":       {{"path": "notes/Open/Moved.md"}, {"path": "notes/Open/Note.md"}, {"path": "notes/hidn-private/hidn-secret.md"}, {"path": "notes/hidn-private/hidn-moved.md"}},
	"kb_ls_tree":       {{}, {"revision": "HEAD~6"}, {"folder": "notes/hidn-private"}, {"folder": "notes/Shared"}, {"revision": "HEAD~2", "folder": "notes/hidn-private"}},
	"kb_show_revision": {{"path": "notes/Open/Note.md", "revision": "HEAD~4"}, {"path": "notes/hidn-private/hidn-secret.md"}, {"path": "notes/hidn-private/hidn-moved.md", "revision": "HEAD~2"}},
	"kb_diff":          {{"from": "HEAD~6"}, {"from": "HEAD~6", "to": "HEAD"}, {"from": "HEAD~6", "path": "notes/hidn-private"}, {"from": "HEAD~6", "path": "notes/hidn-private/hidn-secret.md"}, {"from": "HEAD~2", "to": "HEAD~1", "path": "notes"}},
	"kb_sync_status":   {{}, {"repository": "notes"}},
	"kb_conflicts":     {{}, {"repository": "notes"}},
	"kb_sync_now":      {{"repository": "notes"}},
	"kb_info":          {{}},
	"kb_reindex":       {{}, {"repository": "notes"}},
	"kb_get_file":      {{"path": "notes/Open/file.bin"}, {"path": "notes/hidn-private/hidn-blob.bin"}, {"path": "notes/hidn-private/hidn-secret.md"}},
	"kb_file_link":     {{"path": "notes/Open/file.bin"}, {"path": "notes/hidn-private/hidn-blob.bin"}},
	"kb_upload_link":   {{"path": "notes/Open/"}, {"path": "notes/hidn-private/"}, {"path": "notes/ReadOnly/"}},
	"kb_upload_file":   {{"path": "notes/hidn-private/x.bin", "content_base64": "eA=="}, {"path": "notes/ReadOnly/x.bin", "content_base64": "eA=="}, {"path": "notes/hidn-private/hidn-blob.bin", "content_base64": "eA==", "overwrite": true}},
	"kb_create_note":   {{"path": "notes/hidn-private/new.md", "content": "x"}, {"path": "notes/hidn-private/hidn-secret.md", "content": "x"}, {"path": "notes/ReadOnly/new.md", "content": "x"}},
	"kb_replace_note":  {{"path": "notes/hidn-private/hidn-secret.md", "content": "x"}, {"path": "notes/hidn-private/hidn-secret.md", "content": "x", "etag": "stale"}, {"path": "notes/ReadOnly/Doc.md", "content": "x"}},
	"kb_patch_note": {{"path": "notes/hidn-private/hidn-secret.md", "operations": []map[string]any{{"op": "append", "content": "x"}}},
		{"path": "notes/hidn-private/hidn-secret.md", "operations": []map[string]any{{"op": "find_replace", "find": "QUOKKA", "replace": "x"}}}},
	"kb_move_note":        {{"from": "notes/hidn-private/hidn-secret.md", "to": "notes/Open/got.md"}, {"from": "notes/Open/Sub/Deep.md", "to": "notes/hidn-private/Deep.md"}, {"from": "notes/Open/Sub/Deep.md", "to": "notes/Open/Sub/Deeper.md"}},
	"kb_delete_note":      {{"path": "notes/hidn-private/hidn-secret.md"}, {"path": "notes/hidn-private/hidn-secret.md", "etag": "stale"}, {"path": "notes/ReadOnly/Doc.md"}},
	"kb_restore":          {{"path": "notes/hidn-private/hidn-secret.md", "revision": "HEAD~3"}, {"path": "notes/hidn-private/hidn-moved.md", "revision": "HEAD~2"}, {"path": "notes/ReadOnly/Doc.md", "revision": "HEAD"}},
	"kb_resolve_conflict": {{"resolutions": []map[string]any{{"path": "notes/hidn-private/hidn-secret.md", "take": "theirs"}}}},
}

// strings collects every string inside a JSON-like value.
func stringsIn(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case map[string]any:
		for _, e := range x {
			stringsIn(e, out)
		}
	case []map[string]any:
		for _, e := range x {
			stringsIn(e, out)
		}
	case []string:
		*out = append(*out, x...)
	case []any:
		for _, e := range x {
			stringsIn(e, out)
		}
	}
}

// TestNothingHiddenLeaks calls every registered tool as a restricted account
// and requires that nothing hidden shows up anywhere in what comes back: no
// hidden file or folder name, no hidden content, no commit message that
// mentions them. What the caller typed itself may be echoed back.
func TestNothingHiddenLeaks(t *testing.T) {
	m := newMulti(t)
	names := m.srv.ToolNames()
	sort.Strings(names)
	for _, name := range names {
		if len(leakCases[name]) == 0 {
			t.Errorf("tool %s is registered but has no case in the leak matrix", name)
		}
	}
	for name := range leakCases {
		found := false
		for _, n := range names {
			found = found || n == name
		}
		if !found {
			t.Errorf("the leak matrix has cases for %s, which is not a registered tool", name)
		}
	}
	for _, name := range names {
		for i, args := range leakCases[name] {
			res, err := m.sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Errorf("%s case %d: protocol error: %v", name, i, err)
				continue
			}
			got := everything(res)
			var typed []string
			stringsIn(args, &typed)
			// Longest first, so a path is removed before its parts.
			sort.Slice(typed, func(a, b int) bool { return len(typed[a]) > len(typed[b]) })
			for _, s := range typed {
				// A folder may be echoed without its trailing slash.
				if s = strings.TrimSuffix(s, "/"); s != "" {
					got = strings.ReplaceAll(got, s, "")
				}
			}
			for _, tok := range []string{testutil.HiddenToken, testutil.HiddenMarker} {
				if idx := strings.Index(got, tok); idx >= 0 {
					from, to := max(0, idx-160), min(len(got), idx+160)
					t.Errorf("%s case %d (%v) leaks %q: …%s…", name, i, args, tok, got[from:to])
				}
			}
		}
	}
	// The same calls as the owner do reach hidden content, so the matrix
	// would notice if the fixture stopped containing any.
	if err := m.st.Update(func(s *store.State) error {
		return s.SetRule(s.AccountByEmail("admin@example.com").ID, s.RepoByAlias("notes").ID, "", acl.Write)
	}); err != nil {
		t.Fatal(err)
	}
	m.as("admin@example.com")
	for _, probe := range []struct {
		tool string
		args map[string]any
	}{
		{"kb_search", map[string]any{"query": "zebra", "limit": 50}},
		{"kb_list", map[string]any{"recursive": true}},
		{"kb_log", map[string]any{"limit": 100}},
		{"kb_tags", map[string]any{}},
		{"kb_diff", map[string]any{"from": "HEAD~6"}},
	} {
		res, err := m.sess.CallTool(context.Background(), &mcp.CallToolParams{Name: probe.tool, Arguments: probe.args})
		if err != nil || !strings.Contains(everything(res), testutil.HiddenToken) {
			t.Errorf("%s as the owner must show hidden content (is the fixture intact?): %v", probe.tool, err)
		}
	}
}

// TestHiddenLooksMissing requires every path-taking tool to answer a hidden
// existing path exactly as a path that does not exist.
func TestHiddenLooksMissing(t *testing.T) {
	m := newMulti(t)
	const hidden, missing = "notes/hidn-private/hidn-secret.md", "notes/hidn-private/absent-file.md"
	const hiddenDir, missingDir = "notes/hidn-private", "notes/absent-folder"
	ops := []map[string]any{{"op": "append", "content": "x"}}
	cases := []struct {
		tool string
		args func(p string) map[string]any
		dir  bool
	}{
		{"kb_get_note", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_get_section", func(p string) map[string]any { return map[string]any{"path": p, "heading": "x"} }, false},
		{"kb_backlinks", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_history", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_show_revision", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_diff", func(p string) map[string]any { return map[string]any{"from": "HEAD~3", "path": p} }, false},
		{"kb_get_file", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_file_link", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_create_note", func(p string) map[string]any { return map[string]any{"path": p, "content": "x"} }, false},
		{"kb_replace_note", func(p string) map[string]any { return map[string]any{"path": p, "content": "x"} }, false},
		{"kb_patch_note", func(p string) map[string]any { return map[string]any{"path": p, "operations": ops} }, false},
		{"kb_delete_note", func(p string) map[string]any { return map[string]any{"path": p} }, false},
		{"kb_restore", func(p string) map[string]any { return map[string]any{"path": p, "revision": "HEAD~3"} }, false},
		{"kb_upload_file", func(p string) map[string]any {
			return map[string]any{"path": p, "content_base64": "eA==", "overwrite": true}
		}, false},
		{"kb_move_note", func(p string) map[string]any { return map[string]any{"from": p, "to": "notes/Open/landed.md"} }, false},
		{"kb_list", func(p string) map[string]any { return map[string]any{"folder": p} }, true},
		{"kb_ls_tree", func(p string) map[string]any { return map[string]any{"folder": p} }, true},
		{"kb_grep", func(p string) map[string]any { return map[string]any{"pattern": "zebra", "folder": p} }, true},
		{"kb_log", func(p string) map[string]any { return map[string]any{"folder": p} }, true},
		{"kb_search", func(p string) map[string]any {
			return map[string]any{"query": "zebra", "filters": map[string]any{"folder": p}}
		}, true},
		{"kb_upload_link", func(p string) map[string]any { return map[string]any{"path": p + "/"} }, true},
	}
	normal := func(res *mcp.CallToolResult, p string) string {
		s := strings.ReplaceAll(everything(res), p, "<path>")
		// Timings differ from call to call.
		if i := strings.Index(s, `"took_ms"`); i >= 0 {
			s = s[:i]
		}
		return s
	}
	for _, c := range cases {
		h, m2 := hidden, missing
		if c.dir {
			h, m2 = hiddenDir, missingDir
		}
		hres, err := m.sess.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args(h)})
		if err != nil {
			t.Fatal(err)
		}
		mres, err := m.sess.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args(m2)})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := normal(hres, h), normal(mres, m2); got != want || hres.IsError != mres.IsError {
			t.Errorf("%s tells a hidden path from a missing one:\nhidden:  %s\nmissing: %s", c.tool, got, want)
		}
	}
}

func TestMultiWriteLevels(t *testing.T) {
	m := newMulti(t)
	ops := []map[string]any{{"op": "append", "content": "x"}}
	mustFail(t, m.sess, "kb_patch_note", map[string]any{"path": "notes/ReadOnly/Doc.md", "operations": ops}, "forbidden")
	mustFail(t, m.sess, "kb_move_note", map[string]any{"from": "notes/Open/Note.md", "to": "notes/ReadOnly/Note.md"}, "forbidden")
	mustFail(t, m.sess, "kb_upload_link", map[string]any{"path": "notes/ReadOnly/"}, "forbidden")
	mustFail(t, m.sess, "kb_restore", map[string]any{"path": "notes/ReadOnly/Doc.md", "revision": "HEAD"}, "forbidden")
	mustFail(t, m.sess, "kb_reindex", map[string]any{}, "forbidden")
	mustFail(t, m.sess, "kb_create_note", map[string]any{"path": "plan.md", "content": "x"}, "invalid_path")
	mustFail(t, m.sess, "kb_get_note", map[string]any{"path": "notes/../other/x.md"}, "invalid_path")
	out := mustOK(t, m.sess, "kb_patch_note", map[string]any{"path": "notes/Open/Note.md", "operations": ops})
	if out["path"] != "notes/Open/Note.md" || out["commit"] == "" {
		t.Errorf("write result: %v", out)
	}
	link := mustOK(t, m.sess, "kb_file_link", map[string]any{"path": "notes/Open/file.bin"})
	if u, _ := link["url"].(string); !strings.HasPrefix(u, "https://kb.example.com/files/notes/Open/file.bin?") {
		t.Errorf("link: %v", link)
	}
	// A call that carries no account at all sees nothing.
	m.srv.testCaller = &workspace.Caller{}
	if got := mustOK(t, m.sess, "kb_list", map[string]any{}); len(got["entries"].([]any)) != 0 {
		t.Errorf("a call without an account: %v", got)
	}
	mustFail(t, m.sess, "kb_get_note", map[string]any{"path": "notes/Open/Note.md"}, "not_found")
}
