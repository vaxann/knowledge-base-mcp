package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

const wantTools = 31

func newSession(t *testing.T, readOnly bool) (*mcp.ClientSession, string) {
	t.Helper()
	vaultDir, _ := testutil.NewVaultWithRemote(t)
	cfg := config.Default()
	cfg.Vault.Path = vaultDir
	cfg.Search.IndexDir = filepath.Join(t.TempDir(), "idx")
	cfg.Git.PushDebounce = 0
	cfg.Git.PullInterval = 0
	cfg.Server.ReadOnly = readOnly
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := kb.Open(context.Background(), cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	srv := New(svc, "test", log)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() { _ = srv.MCP().Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, vaultDir
}

// call invokes a tool and decodes the structured result. Object results are
// returned as a map; array results are wrapped under the key "items".
func call(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any) (map[string]any, *mcp.CallToolResult) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	var raw []byte
	if res.StructuredContent != nil {
		raw, _ = json.Marshal(res.StructuredContent)
	} else {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				raw = []byte(tc.Text)
			}
		}
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	switch x := v.(type) {
	case map[string]any:
		return x, res
	case []any:
		return map[string]any{"items": x}, res
	}
	return map[string]any{}, res
}

func mustOK(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	out, res := call(t, sess, tool, args)
	if res.IsError {
		t.Fatalf("%s failed: %v", tool, out)
	}
	return out
}

func mustFail(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any, code string) map[string]any {
	t.Helper()
	out, res := call(t, sess, tool, args)
	if !res.IsError || out["code"] != code {
		t.Fatalf("%s: expected error %s, got isError=%v %v", tool, code, res.IsError, out)
	}
	return out
}

func TestToolsList(t *testing.T) {
	sess, _ := newSession(t, false)
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != wantTools {
		t.Errorf("tool count = %d, want %d", len(res.Tools), wantTools)
	}
	for _, tool := range res.Tools {
		if !strings.HasPrefix(tool.Name, "kb_") {
			t.Errorf("tool %s lacks kb_ prefix", tool.Name)
		}
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("tool %s lacks description or schema", tool.Name)
		}
	}
	ro, _ := newSession(t, true)
	res, _ = ro.ListTools(context.Background(), nil)
	if len(res.Tools) != wantTools-9 {
		t.Errorf("read-only tool count = %d", len(res.Tools))
	}
	for _, tool := range res.Tools {
		switch tool.Name {
		case "kb_create_note", "kb_replace_note", "kb_patch_note", "kb_move_note", "kb_delete_note", "kb_restore", "kb_resolve_conflict", "kb_upload_file", "kb_upload_link":
			t.Errorf("write tool %s listed in read-only mode", tool.Name)
		}
	}
}

func TestErrorShapeAndResources(t *testing.T) {
	sess, _ := newSession(t, false)
	out := mustFail(t, sess, "kb_get_note", map[string]any{"path": "Nope.md"}, "not_found")
	if out["message"] == "" {
		t.Error("error message missing")
	}
	mustFail(t, sess, "kb_get_note", map[string]any{"path": "../etc/passwd"}, "invalid_path")
	mustFail(t, sess, "kb_search", map[string]any{"query": "x", "filters": map[string]any{"modified_after": "yesterday"}}, "invalid_argument")

	rr, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "kb://note/Projects/Alpha.md"})
	if err != nil || len(rr.Contents) != 1 || !strings.Contains(rr.Contents[0].Text, "# Alpha") || rr.Contents[0].MIMEType != "text/markdown" {
		t.Errorf("note resource: %+v %v", rr, err)
	}
	rr, err = sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "kb://folder/Projects"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "Projects/Alpha.md") {
		t.Errorf("folder resource: %+v %v", rr, err)
	}
	if _, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "kb://note/Nope.md"}); err == nil {
		t.Error("missing resource must error")
	}
	tpl, err := sess.ListResourceTemplates(context.Background(), nil)
	if err != nil || len(tpl.ResourceTemplates) != 3 {
		t.Errorf("templates: %+v %v", tpl, err)
	}
}

// TestEndToEnd drives the documented scenario through MCP:
// create -> search -> grep -> patch -> move -> delete -> ls_tree -> restore.
func TestEndToEnd(t *testing.T) {
	sess, vaultDir := newSession(t, false)
	created := mustOK(t, sess, "kb_create_note", map[string]any{
		"path": "Ideas/Zebra.md", "content": "# Zebra\n\n## Plan\n\nStripes first.\n",
		"frontmatter": map[string]any{"type": "idea", "value": 7}, "summary": "new idea",
	})
	etag := created["etag"].(string)
	if msg := testutil.Git(t, vaultDir, "log", "-1", "--format=%B"); !strings.Contains(msg, "KB-Client: test-client") || !strings.Contains(msg, "new idea") {
		t.Errorf("commit trailer:\n%s", msg)
	}
	hits := mustOK(t, sess, "kb_search", map[string]any{"query": "stripes"})["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["path"] != "Ideas/Zebra.md" {
		t.Fatalf("search: %v", hits)
	}
	rows := mustOK(t, sess, "kb_query", map[string]any{"where": `type eq "idea" and value gte 7`, "select": []string{"path", "value"}})
	if n := rows["rows"]; n != nil {
		t.Fatalf("unexpected wrapper %v", n)
	}
	grep := mustOK(t, sess, "kb_grep", map[string]any{"pattern": "Stripes"})
	if grep["total"].(float64) != 1 {
		t.Fatalf("grep: %v", grep)
	}
	patched := mustOK(t, sess, "kb_patch_note", map[string]any{
		"path": "Ideas/Zebra.md", "etag": etag,
		"operations": []map[string]any{{"op": "replace_section", "heading": "Plan", "content": "Stripes then mane."}, {"op": "set_frontmatter", "fields": map[string]any{"status": "active"}}},
	})
	mustFail(t, sess, "kb_patch_note", map[string]any{"path": "Ideas/Zebra.md", "etag": etag, "operations": []map[string]any{{"op": "append", "content": "x"}}}, "conflict")
	note := mustOK(t, sess, "kb_get_note", map[string]any{"path": "Ideas/Zebra.md"})
	if !strings.Contains(note["body"].(string), "then mane") || note["etag"] != patched["etag"] {
		t.Fatalf("patched note: %v", note)
	}
	moved := mustOK(t, sess, "kb_move_note", map[string]any{"from": "Ideas/Zebra.md", "to": "Archive/Zebra.md"})
	if moved["path"] != "Archive/Zebra.md" {
		t.Fatalf("move: %v", moved)
	}
	ctxb := mustOK(t, sess, "kb_context", map[string]any{"query": "zebra mane", "max_chars": 2000})
	if !strings.Contains(ctxb["text"].(string), "Archive/Zebra.md") {
		t.Fatalf("context: %v", ctxb)
	}
	del := mustOK(t, sess, "kb_delete_note", map[string]any{"path": "Archive/Zebra.md"})
	mustFail(t, sess, "kb_get_note", map[string]any{"path": "Archive/Zebra.md"}, "not_found")
	tree := mustOK(t, sess, "kb_ls_tree", map[string]any{"revision": del["commit"].(string) + "~1", "folder": "Archive"})
	raw, _ := json.Marshal(tree)
	if !strings.Contains(string(raw), "Archive/Zebra.md") {
		t.Fatalf("ls_tree: %s", raw)
	}
	hist := mustOK(t, sess, "kb_history", map[string]any{"path": "Archive/Zebra.md"})
	if raw, _ := json.Marshal(hist); !strings.Contains(string(raw), "kb_create_note: Ideas/Zebra.md") {
		t.Fatalf("history must follow the rename: %s", raw)
	}
	shown := mustOK(t, sess, "kb_show_revision", map[string]any{"path": "Archive/Zebra.md", "revision": "HEAD~1"})
	if !strings.Contains(shown["content"].(string), "then mane") {
		t.Fatalf("show_revision: %v", shown)
	}
	restored := mustOK(t, sess, "kb_restore", map[string]any{"path": "Archive/Zebra.md", "revision": "HEAD~1"})
	if restored["etag"] != patched["etag"] {
		t.Errorf("restored etag differs: %v vs %v", restored["etag"], patched["etag"])
	}
	if d := mustOK(t, sess, "kb_diff", map[string]any{"from": "HEAD~1", "to": "HEAD"}); !strings.Contains(d["diff"].(string), "+++ b/Archive/Zebra.md") {
		t.Errorf("diff: %v", d)
	}
	st := mustOK(t, sess, "kb_sync_now", nil)
	if st["state"] != "ok" || st["ahead"].(float64) != 0 {
		t.Errorf("sync: %v", st)
	}
	info := mustOK(t, sess, "kb_info", nil)
	if info["note_count"].(float64) != 10 || info["sync_state"] != "ok" {
		t.Errorf("info: %v", info)
	}
	log := mustOK(t, sess, "kb_log", map[string]any{"limit": 2})
	if len(log["commits"].([]any)) != 2 || log["next_cursor"].(float64) != 2 {
		t.Errorf("log: %v", log)
	}
	tags := mustOK(t, sess, "kb_tags", map[string]any{"prefix": "pro"})
	if raw, _ := json.Marshal(tags); !strings.Contains(string(raw), "project") {
		t.Errorf("tags: %s", raw)
	}
	bl := mustOK(t, sess, "kb_backlinks", map[string]any{"path": "Projects/Alpha.md"})
	if raw, _ := json.Marshal(bl); !strings.Contains(string(raw), "README.md") {
		t.Errorf("backlinks: %s", raw)
	}
	qo := mustOK(t, sess, "kb_quick_open", map[string]any{"text": "zebr"})
	if raw, _ := json.Marshal(qo); !strings.Contains(string(raw), "Archive/Zebra.md") {
		t.Errorf("quick open: %s", raw)
	}
	sec := mustOK(t, sess, "kb_get_section", map[string]any{"path": "Archive/Zebra.md", "heading": "plan"})
	if !strings.Contains(sec["text"].(string), "mane") {
		t.Errorf("section: %v", sec)
	}
	conf := mustOK(t, sess, "kb_conflicts", nil)
	if len(conf["conflicts"].([]any)) != 0 {
		t.Errorf("conflicts: %v", conf)
	}
	mustFail(t, sess, "kb_resolve_conflict", map[string]any{"resolutions": []map[string]any{{"path": "x.md", "content": "y"}}}, "not_in_conflict")
}

// TestStdioSubprocess builds the binary, launches it as an MCP command and
// checks that it answers initialize, writes nothing but protocol to stdout and
// opens no listening sockets.
func TestStdioSubprocess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on /proc")
	}
	bin := filepath.Join(t.TempDir(), "kb-mcp")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/knowledge-base-mcp")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	vaultDir := testutil.NewVaultRepo(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "KB_VAULT_PATH="+vaultDir, "KB_INDEX_DIR="+filepath.Join(t.TempDir(), "idx"), "KB_LOG_LEVEL=debug")
	cmd.Stderr = io.Discard
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "kb_info", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("kb_info over stdio: %v %+v", err, res)
	}
	pid := cmd.Process.Pid
	if listening := listeningSockets(t, pid); len(listening) > 0 {
		t.Errorf("server has listening sockets: %v", listening)
	}
}

// listeningSockets returns inodes of TCP sockets in LISTEN state owned by pid.
func listeningSockets(t *testing.T, pid int) []string {
	t.Helper()
	fds, err := os.ReadDir("/proc/" + itoa(pid) + "/fd")
	if err != nil {
		t.Skip("cannot read /proc")
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink("/proc/" + itoa(pid) + "/fd/" + fd.Name())
		if err == nil && strings.HasPrefix(link, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	var out []string
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 9 && f[3] == "0A" && inodes[f[9]] {
				out = append(out, f[1])
			}
		}
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }
