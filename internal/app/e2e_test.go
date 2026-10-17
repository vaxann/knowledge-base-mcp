package app

import (
	"net/url"
	"strings"
	"testing"

	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

// TestEndToEnd walks the whole product through its public surfaces only:
// the admin panel in a browser and the MCP endpoint behind OAuth.
func TestEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	vaultDir, _ := testutil.NewAccessRepo(t)
	cfg.Vault.Path = vaultDir // becomes the first repository, "main"
	h := start(t, cfg)
	h.relaxLimits()

	// The administrator signs in to the panel and adds a second repository.
	b := adminBrowser(t, h)
	_, remote := testutil.NewAccessRepo(t)
	b.act("/admin/repos", url.Values{"alias": {"team"}, "url": {remote}, "credential": {"token"}, "token": {"team-repo-token-0001"}})
	h.ready()
	if p := b.get("/admin/repos"); strings.Count(p.body, "s-ok") != 2 {
		t.Fatalf("both repositories must be ready:\n%s", p.body)
	}

	// The new repository is invisible to the administrator itself until it
	// grants itself access; the imported vault is its own from the start.
	adminTok, _ := h.signIn("admin@example.com")
	adminSess := h.session(adminTok.access)
	out, _ := call(t, adminSess, "kb_list", map[string]any{})
	same(t, "administrator root before granting itself access", entries(out), "main")
	admin := h.m.Store.Snapshot().AccountByEmail("admin@example.com")
	b.act("/admin/access/team", url.Values{"account": {admin.ID}, "folder": {""}, "level": {"write"}})
	out, _ = call(t, adminSess, "kb_list", map[string]any{})
	same(t, "administrator root after granting itself access", entries(out), "main", "team")

	// A new account gets one nested folder to read and one to write.
	b.act("/admin/users", url.Values{"email": {"ann@example.com"}})
	ann := h.m.Store.Snapshot().AccountByEmail("ann@example.com")
	b.act("/admin/access/team", url.Values{"account": {ann.ID}, "folder": {"Shared/Team"}, "level": {"read"}})
	b.act("/admin/access/team", url.Values{"account": {ann.ID}, "folder": {"Open/Sub"}, "level": {"write"}})

	// The account signs in through OAuth with the emailed code.
	tok, _ := h.signIn("ann@example.com")
	sess := h.session(tok.access)
	out, _ = call(t, sess, "kb_list", map[string]any{})
	same(t, "account root", entries(out), "team")
	out, _ = call(t, sess, "kb_list", map[string]any{"folder": "team", "recursive": true})
	same(t, "everything the account can see", entries(out), "team/Open/Sub/Deep.md", "team/Shared/Team/Plan.md")

	// Inside its rules: read, search, write.
	note, c := call(t, sess, "kb_get_note", map[string]any{"path": "team/Shared/Team/Plan.md"})
	if c != "" || !strings.Contains(note["content"].(string), "Team plan zebra") {
		t.Fatalf("read inside the rules: %q %v", c, note)
	}
	found, _ := call(t, sess, "kb_search", map[string]any{"query": "zebra"})
	if found["total"].(float64) != 2 {
		t.Errorf("search covers exactly the readable notes: %v", found)
	}
	written, c := call(t, sess, "kb_patch_note", map[string]any{"path": "team/Open/Sub/Deep.md", "operations": []map[string]any{{"op": "append", "content": "added by ann"}}})
	if c != "" || written["commit"] == "" {
		t.Fatalf("write inside the rules: %q %v", c, written)
	}
	created, c := call(t, sess, "kb_create_note", map[string]any{"path": "team/Open/Sub/New.md", "content": "# New\n\nwombatless content\n"})
	if c != "" || created["path"] != "team/Open/Sub/New.md" {
		t.Fatalf("create inside the rules: %q %v", c, created)
	}
	if found, _ = call(t, sess, "kb_search", map[string]any{"query": "wombatless"}); found["total"].(float64) != 1 {
		t.Errorf("a new note is searchable at once: %v", found)
	}
	log, _ := call(t, sess, "kb_log", map[string]any{"limit": 1})
	last := log["commits"].([]any)[0].(map[string]any)
	if !strings.Contains(last["message"].(string), "KB-User: ann@example.com") || !strings.HasPrefix(last["message"].(string), "kb_create_note: Open/Sub/New.md") {
		t.Errorf("the commit names the acting account: %v", last["message"])
	}

	// Outside its rules: refused, and indistinguishable from absent.
	for tool, args := range map[string]map[string]any{
		"kb_get_note":    {"path": "team/Open/Note.md"},
		"kb_get_file":    {"path": "team/hidn-private/hidn-blob.bin"},
		"kb_create_note": {"path": "team/Open/New.md", "content": "x"},
		"kb_list":        {"folder": "main"},
		"kb_history":     {"path": "main/Open/Note.md"},
	} {
		if _, c := call(t, sess, tool, args); c != "not_found" {
			t.Errorf("%s outside the rules: %q, want not_found", tool, c)
		}
	}
	if _, c := call(t, sess, "kb_patch_note", map[string]any{"path": "team/Shared/Team/Plan.md", "operations": []map[string]any{{"op": "append", "content": "x"}}}); c != "forbidden" {
		t.Errorf("write to a read-only folder: %q, want forbidden", c)
	}
	if _, c := call(t, sess, "kb_reindex", map[string]any{}); c != "forbidden" {
		t.Errorf("reindex by a regular account: %q", c)
	}
	if found, _ = call(t, sess, "kb_search", map[string]any{"query": "QUOKKA*"}); found["total"].(float64) != 0 {
		t.Errorf("hidden content must not be found: %v", found)
	}

	// The administrator sees the account's work in the repository it shares.
	seen, c := call(t, adminSess, "kb_get_note", map[string]any{"path": "team/Open/Sub/New.md"})
	if c != "" || !strings.Contains(seen["content"].(string), "wombatless") {
		t.Errorf("the administrator reads what the account wrote: %q", c)
	}
	// Taking the rule away takes the folder away, on the same session.
	b.act("/admin/access/team", url.Values{"account": {ann.ID}, "folder": {"Open/Sub"}, "level": {"inherit"}})
	if _, c := call(t, sess, "kb_get_note", map[string]any{"path": "team/Open/Sub/New.md"}); c != "not_found" {
		t.Errorf("after the rule is cleared: %q", c)
	}
}
