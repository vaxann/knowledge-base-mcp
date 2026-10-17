package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/mcpserver"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

// browser is a client with a cookie jar that does not follow redirects.
type browser struct {
	t    *testing.T
	h    *harness
	http *http.Client
	csrf string
}

func (h *harness) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: h.t, h: h, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) do(req *http.Request) page {
	b.t.Helper()
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	p := page{status: resp.StatusCode, body: string(raw), location: resp.Header.Get("Location")}
	if m := csrfRE.FindStringSubmatch(p.body); m != nil {
		b.csrf = m[1]
	}
	return p
}

func (b *browser) get(path string) page {
	req, _ := http.NewRequest(http.MethodGet, b.h.ts.URL+path, nil)
	return b.do(req)
}

// post submits a form with the anti-forgery token of the last page seen.
func (b *browser) post(path string, form url.Values) page {
	if form.Get("csrf") == "" && b.csrf != "" {
		form.Set("csrf", b.csrf)
	}
	return b.raw(path, form, "")
}

func (b *browser) raw(path string, form url.Values, origin string) page {
	req, _ := http.NewRequest(http.MethodPost, b.h.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return b.do(req)
}

// act posts a form and follows the redirect back to a page.
func (b *browser) act(path string, form url.Values) page {
	b.t.Helper()
	p := b.post(path, form)
	if p.status != http.StatusSeeOther {
		b.t.Fatalf("POST %s: %d\n%s", path, p.status, p.body)
	}
	return b.get(p.location)
}

// signIn signs the browser in through the email code.
func (b *browser) signIn(email string) page {
	b.t.Helper()
	first := b.get("/admin/signin")
	m := reqIDRE.FindStringSubmatch(first.body)
	if first.status != 200 || m == nil {
		b.t.Fatalf("sign-in page: %d\n%s", first.status, first.body)
	}
	b.raw("/admin/signin", url.Values{"req_id": {m[1]}, "step": {"email"}, "email": {email}}, "")
	b.h.m.SignIn.Wait()
	return b.raw("/admin/signin", url.Values{"req_id": {m[1]}, "step": {"code"}, "code": {b.h.mail.lastCode(email)}}, "")
}

func adminBrowser(t *testing.T, h *harness) *browser {
	t.Helper()
	b := h.browser()
	if p := b.signIn("admin@example.com"); p.status != http.StatusSeeOther || p.location != "/admin/users" {
		t.Fatalf("administrator sign-in: %d %q\n%s", p.status, p.location, p.body)
	}
	if p := b.get("/admin/users"); p.status != 200 || b.csrf == "" {
		t.Fatalf("panel after sign-in: %d", p.status)
	}
	return b
}

func TestAdminPanelAccess(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.HTTP.Token = "static-recovery-token-value"
	now := time.Now()
	h := &harness{}
	func() {
		// start with an adjustable clock for session expiry
		hh := startWith(t, cfg, func(o *Options) { o.Now = func() time.Time { return now } })
		*h = *hh
	}()
	h.addAccount("ann@example.com")
	h.relaxLimits()

	// Without a session nothing is shown.
	visitor := h.browser()
	for _, path := range []string{"/admin", "/admin/", "/admin/users", "/admin/repos", "/admin/access/notes"} {
		p := visitor.get(path)
		if p.status != http.StatusSeeOther || p.location != "/admin/signin" || strings.Contains(p.body, "example.com") {
			t.Errorf("GET %s without a session: %d %q", path, p.status, p.location)
		}
	}
	if p := visitor.raw("/admin/users", url.Values{"email": {"x@example.com"}}, ""); p.status != http.StatusForbidden {
		t.Errorf("POST without a session: %d", p.status)
	}
	signin := visitor.get("/admin/signin")
	// Referrer-Policy must let a browser send the real Origin on same-origin
	// posts: under "no-referrer" it sends "Origin: null" and every form of
	// the panel would be refused.
	for hdr, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Referrer-Policy": "same-origin"} {
		resp, _ := http.Get(h.ts.URL + "/admin/signin")
		_ = resp.Body.Close()
		if got := resp.Header.Get(hdr); got != want {
			t.Errorf("%s = %q, want %q", hdr, got, want)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("Content-Security-Policy = %q", csp)
		}
	}
	if strings.Contains(signin.body, "<script>") || strings.Contains(signin.body, "style=") || strings.Contains(signin.body, "onclick") {
		t.Error("panel pages must carry no inline script or style")
	}

	// A regular account's valid code opens nothing and looks like a wrong code.
	normal := func(p page) string { return reqIDRE.ReplaceAllString(p.body, "") }
	regular := h.browser()
	began := time.Now()
	refused := regular.signIn("ann@example.com")
	if refused.status != http.StatusUnauthorized || refused.location != "" || time.Since(began) < 40*time.Millisecond {
		t.Errorf("a regular account on the panel: %d %q", refused.status, refused.location)
	}
	if p := regular.get("/admin/users"); p.status != http.StatusSeeOther {
		t.Errorf("no session may exist for a regular account: %d", p.status)
	}
	wrongCode := h.browser()
	first := wrongCode.get("/admin/signin")
	id := reqIDRE.FindStringSubmatch(first.body)[1]
	wrongCode.raw("/admin/signin", url.Values{"req_id": {id}, "step": {"email"}, "email": {"ann@example.com"}}, "")
	wrong := wrongCode.raw("/admin/signin", url.Values{"req_id": {id}, "step": {"code"}, "code": {"000000"}}, "")
	if wrong.status != refused.status || normal(wrong) != normal(refused) {
		t.Errorf("a regular account's valid code must be answered like a wrong code")
	}

	// The administrator signs in with the emailed code.
	b := adminBrowser(t, h)
	users := b.get("/admin/users")
	if !strings.Contains(users.body, "admin@example.com") || !strings.Contains(users.body, "ann@example.com") {
		t.Errorf("accounts page:\n%s", users.body)
	}
	var cookie *http.Cookie
	u, _ := url.Parse(h.ts.URL)
	for _, c := range b.http.Jar.Cookies(u) {
		if c.Name == "kb_admin" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}

	// Forged requests change nothing.
	accounts := func() int { return len(h.m.Store.Snapshot().Accounts) }
	before := accounts()
	if p := b.raw("/admin/users", url.Values{"email": {"evil@example.com"}}, ""); p.status != http.StatusForbidden {
		t.Errorf("a post without the anti-forgery token: %d", p.status)
	}
	if p := b.raw("/admin/users", url.Values{"email": {"evil@example.com"}, "csrf": {"guess"}}, ""); p.status != http.StatusForbidden {
		t.Errorf("a post with a wrong anti-forgery token: %d", p.status)
	}
	if p := b.raw("/admin/users", url.Values{"email": {"evil@example.com"}, "csrf": {b.csrf}}, "https://attacker.example"); p.status != http.StatusForbidden {
		t.Errorf("a post from another origin: %d", p.status)
	}
	if p := b.raw("/admin/users", url.Values{"email": {"evil@example.com"}, "csrf": {b.csrf}}, "null"); p.status != http.StatusForbidden {
		t.Errorf("a post from an opaque origin (a sandboxed frame): %d", p.status)
	}
	if accounts() != before {
		t.Fatal("a forged request changed the accounts")
	}
	if p := b.raw("/admin/users", url.Values{"email": {"own@example.com"}, "csrf": {b.csrf}}, h.ts.URL); p.status != http.StatusSeeOther || accounts() != before+1 {
		t.Errorf("a post from the panel's own origin: %d", p.status)
	}

	// Sessions end after 12 hours and on sign-out.
	now = now.Add(11 * time.Hour)
	if p := b.get("/admin/users"); p.status != 200 {
		t.Errorf("session after 11 hours: %d", p.status)
	}
	now = now.Add(2 * time.Hour)
	if p := b.get("/admin/users"); p.status != http.StatusSeeOther {
		t.Errorf("session after 13 hours: %d, want a redirect to the sign-in page", p.status)
	}
	now = time.Now()
	b = adminBrowser(t, h)
	if p := b.post("/admin/signout", url.Values{}); p.status != http.StatusSeeOther || p.location != "/admin/signin" {
		t.Errorf("sign-out: %d %q", p.status, p.location)
	}
	stale := h.browser()
	stale.http.Jar.SetCookies(u, b.http.Jar.Cookies(u))
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/admin/users", nil)
	req.AddCookie(cookie)
	if p := h.browser().do(req); p.status != http.StatusSeeOther {
		t.Errorf("an old session cookie after sign-out or expiry: %d", p.status)
	}

	// Recovery sign-in when mail cannot be delivered.
	h.mail.mu.Lock()
	h.mail.fail = context.DeadlineExceeded
	h.mail.mu.Unlock()
	rec := h.browser()
	first = rec.get("/admin/signin")
	id = reqIDRE.FindStringSubmatch(first.body)[1]
	began = time.Now()
	if p := rec.raw("/admin/signin", url.Values{"req_id": {id}, "step": {"token"}, "token": {"not-the-token"}}, ""); p.status != http.StatusUnauthorized || time.Since(began) < 40*time.Millisecond {
		t.Errorf("a wrong recovery token must be refused and delayed: %d", p.status)
	}
	if p := rec.raw("/admin/signin", url.Values{"req_id": {id}, "step": {"token"}, "token": {cfg.Server.HTTP.Token}}, ""); p.status != http.StatusSeeOther {
		t.Fatalf("recovery sign-in: %d\n%s", p.status, p.body)
	}
	if p := rec.get("/admin/users"); p.status != 200 {
		t.Errorf("panel after the recovery sign-in: %d", p.status)
	}
	if strings.Contains(h.logs.String(), cfg.Server.HTTP.Token) {
		t.Error("the recovery token must not be logged")
	}
}

// startWith is start with a chance to adjust the options.
func startWith(t *testing.T, cfg config.Config, adjust func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, mail: &outbox{}, cfg: cfg, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	logs := &lockedBuffer{}
	h.logs = &logs.buf
	o := Options{Config: cfg, Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Version: "test", Sender: h.mail, FailureDelay: 40 * time.Millisecond, AllowLocalRemotes: true}
	adjust(&o)
	m, err := NewMulti(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	h.ts = httptest.NewServer(m.Handler())
	t.Cleanup(func() {
		h.ts.Close()
		_ = m.Close()
	})
	h.ready()
	return h
}

func TestAdminPanelIsAbsentInClassicMode(t *testing.T) {
	dir := testutil.NewVaultRepo(t)
	cfg := config.Default()
	cfg.Vault.Path = dir
	cfg.Search.IndexDir = filepath.Join(t.TempDir(), "idx")
	cfg.Git.PullInterval = 0
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := kb.Open(context.Background(), cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ts := httptest.NewServer(mcpserver.New(svc, "test", log).Handler("token"))
	defer ts.Close()
	for _, path := range []string{"/admin", "/admin/signin", "/admin/users"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on a classic server: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestAdminSessionCookieOverHTTPS(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.HTTP.PublicURL = "https://kb.example.com"
	h := start(t, cfg)
	b := h.browser()
	first := b.get("/admin/signin")
	id := reqIDRE.FindStringSubmatch(first.body)[1]
	b.raw("/admin/signin", url.Values{"req_id": {id}, "step": {"email"}, "email": {"admin@example.com"}}, "")
	h.m.SignIn.Wait()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/admin/signin", strings.NewReader(url.Values{"req_id": {id}, "step": {"code"}, "code": {h.mail.lastCode("admin@example.com")}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	set := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"__Host-kb_admin=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %q", set, want)
		}
	}
}

func TestAdminAccounts(t *testing.T) {
	h := start(t, testConfig(t))
	h.addRepo("notes")
	h.relaxLimits()
	b := adminBrowser(t, h)

	p := b.act("/admin/users", url.Values{"email": {"Ann@Example.com"}})
	if !strings.Contains(p.body, "ann@example.com") || !strings.Contains(p.body, "can now sign in") {
		t.Fatalf("after creating an account:\n%s", p.body)
	}
	ann := h.m.Store.Snapshot().AccountByEmail("ann@example.com")
	if ann == nil || ann.Disabled || len(h.m.Store.Snapshot().RulesFor(ann.ID, "")) != 0 {
		t.Fatalf("new account: %+v", ann)
	}
	for _, bad := range []string{"not-an-address", "", "Ann <ann2@example.com>", "ann@example.com"} {
		before := len(h.m.Store.Snapshot().Accounts)
		p = b.act("/admin/users", url.Values{"email": {bad}})
		if !strings.Contains(p.body, "flash bad") || len(h.m.Store.Snapshot().Accounts) != before {
			t.Errorf("address %q must be rejected with an explanation", bad)
		}
	}

	// The account can sign in, and the panel counts its sessions.
	tok, _ := h.signIn("ann@example.com")
	h.grant("ann@example.com", "notes", "Open", acl.Read)
	if p = b.get("/admin/users"); !regexp.MustCompile(`data-label="Sessions">1<`).MatchString(p.body) || !regexp.MustCompile(`data-label="Rules">1<`).MatchString(p.body) {
		t.Errorf("sessions and rules of the account are listed:\n%s", p.body)
	}
	if !h.works(tok.access) {
		t.Fatal("the new account's token must work")
	}

	// Disabling signs the account out; enabling lets it sign in again.
	p = b.act("/admin/users/"+ann.ID+"/disable", url.Values{})
	if !strings.Contains(p.body, "disabled") {
		t.Errorf("after disabling:\n%s", p.body)
	}
	if h.works(tok.access) {
		t.Error("a disabled account's token must stop working")
	}
	b.act("/admin/users/"+ann.ID+"/enable", url.Values{})
	if h.works(tok.access) {
		t.Error("re-enabling must not revive old tokens")
	}
	tok2, _ := h.signIn("ann@example.com")
	p = b.act("/admin/users/"+ann.ID+"/revoke", url.Values{})
	if !strings.Contains(p.body, "1 session(s)") {
		t.Errorf("after ending sessions:\n%s", p.body)
	}
	if h.works(tok2.access) {
		t.Error("ended sessions must stop working")
	}

	// Deleting removes rules and tokens.
	tok3, _ := h.signIn("ann@example.com")
	b.act("/admin/users/"+ann.ID+"/delete", url.Values{})
	st := h.m.Store.Snapshot()
	if st.Accounts[ann.ID] != nil || len(st.Rules) != 0 {
		t.Errorf("after deleting: account=%v rules=%d", st.Accounts[ann.ID], len(st.Rules))
	}
	if h.works(tok3.access) {
		t.Error("a deleted account's token must stop working")
	}

	// The configured administrator is protected.
	admin := st.AccountByEmail("admin@example.com")
	for _, action := range []string{"disable", "delete"} {
		p = b.act("/admin/users/"+admin.ID+"/"+action, url.Values{})
		if !strings.Contains(p.body, "flash bad") {
			t.Errorf("%s of the administrator must be rejected", action)
		}
	}
	if a := h.m.Store.Snapshot().Accounts[admin.ID]; a == nil || a.Disabled {
		t.Fatal("the administrator account must be untouched")
	}
	if p = b.get("/admin/users"); strings.Contains(p.body, "/admin/users/"+admin.ID+"/delete") {
		t.Error("the panel must not offer to delete the administrator")
	}
	if !strings.Contains(h.logs.String(), `msg="admin: account delete" by=admin@example.com account=ann@example.com`) {
		t.Error("account changes must be logged with the acting administrator")
	}
}

func TestAdminRepositories(t *testing.T) {
	h := start(t, testConfig(t))
	b := adminBrowser(t, h)
	_, remote := testutil.NewAccessRepo(t)

	p := b.act("/admin/repos", url.Values{"alias": {"notes"}, "url": {remote}, "branch": {"main"}, "credential": {"token"}, "token": {"a-secret-token-1234"}})
	if listed := strings.Contains(p.body, "s-cloning") || strings.Contains(p.body, "s-ok"); !strings.Contains(p.body, "notes") || !listed {
		t.Fatalf("after adding a repository:\n%s", p.body)
	}
	h.ready()
	p = b.get("/admin/repos")
	if !strings.Contains(p.body, "s-ok") || !strings.Contains(p.body, remote) {
		t.Errorf("the status follows the clone without a restart:\n%s", p.body)
	}
	detail := b.get("/admin/repos/notes")
	if !strings.Contains(detail.body, "ending in") || !strings.Contains(detail.body, "1234") {
		t.Errorf("the credential hint:\n%s", detail.body)
	}
	for _, page := range []string{p.body, detail.body, b.get("/admin/access/notes").body} {
		if strings.Contains(page, "a-secret-token-1234") || strings.Contains(page, "a-secret-token") {
			t.Error("no page may contain a full token")
		}
	}

	// Invalid requests are explained and store nothing.
	for name, form := range map[string]url.Values{
		"duplicate alias": {"alias": {"notes"}, "url": {remote}, "credential": {"token"}},
		"bad alias":       {"alias": {"My Notes"}, "url": {remote}, "credential": {"token"}},
		"bad url":         {"alias": {"x"}, "url": {"http://example.com/x.git"}, "credential": {"token"}, "token": {"t"}},
	} {
		p = b.act("/admin/repos", form)
		if !strings.Contains(p.body, "flash bad") || len(h.m.Store.Snapshot().Repos) != 1 {
			t.Errorf("%s must be rejected with an explanation", name)
		}
	}

	// A repository that cannot be cloned stays, with a reason, and takes a
	// new credential.
	missing := filepath.Join(t.TempDir(), "absent.git")
	b.act("/admin/repos", url.Values{"alias": {"gone"}, "url": {missing}, "credential": {"token"}})
	h.ready()
	if p = b.get("/admin/repos/gone"); !strings.Contains(p.body, "s-error") || !strings.Contains(p.body, "Try again") {
		t.Errorf("a failed clone:\n%s", p.body)
	}
	_, other := testutil.NewAccessRepo(t)
	if err := os.Rename(other, missing); err != nil {
		t.Fatal(err)
	}
	p = b.act("/admin/repos/gone/credential", url.Values{"credential": {"token"}, "token": {"another-token-9876"}})
	h.ready()
	if p = b.get("/admin/repos/gone"); !strings.Contains(p.body, "s-ok") || !strings.Contains(p.body, "9876") {
		t.Errorf("after replacing the credential:\n%s", p.body)
	}
	if p = b.act("/admin/repos/gone/sync", url.Values{}); !strings.Contains(p.body, "Synchronised") {
		t.Errorf("sync now:\n%s", p.body)
	}

	// Removal needs the alias typed exactly.
	p = b.act("/admin/repos/gone/remove", url.Values{"confirm": {"notes"}})
	if !strings.Contains(p.body, "nothing was removed") || h.m.Store.Snapshot().RepoByAlias("gone") == nil {
		t.Errorf("a removal with another alias typed:\n%s", p.body)
	}
	p = b.act("/admin/repos/gone/remove", url.Values{"confirm": {"gone"}})
	if !strings.Contains(p.body, "was removed") || h.m.Store.Snapshot().RepoByAlias("gone") != nil {
		t.Errorf("a confirmed removal:\n%s", p.body)
	}
	if p = b.get("/admin/repos/gone"); p.status != http.StatusNotFound {
		t.Errorf("page of a removed repository: %d", p.status)
	}

	// An SSH repository with a generated key shows the public key and waits.
	if _, err := exec.LookPath("ssh-keygen"); err == nil {
		p = b.act("/admin/repos", url.Values{"alias": {"viassh"}, "url": {"git@example.com:org/notes.git"}, "credential": {"generate"}})
		if !strings.Contains(p.body, "ssh-ed25519 ") || !strings.Contains(p.body, "s-key_pending") || !strings.Contains(p.body, "The key is registered, connect") || !strings.Contains(p.body, "SHA256:") {
			t.Errorf("an SSH repository with a generated key:\n%s", p.body)
		}
		if strings.Contains(p.body, "PRIVATE KEY") {
			t.Error("no page may contain a private key")
		}
		state, _ := os.ReadFile(StatePath(h.cfg))
		if strings.Contains(string(state), "PRIVATE KEY") {
			t.Error("the state file must not contain a private key in clear text")
		}
	}
	state, _ := os.ReadFile(StatePath(h.cfg))
	if strings.Contains(string(state), "a-secret-token-1234") || strings.Contains(h.logs.String(), "a-secret-token-1234") {
		t.Error("a token must appear neither in the state file nor in the logs")
	}
}

func TestAdminAccessRules(t *testing.T) {
	h := start(t, testConfig(t))
	h.addAccount("ann@example.com")
	remote := h.addRepo("notes")
	h.relaxLimits()
	ann := h.m.Store.Snapshot().AccountByEmail("ann@example.com")
	tok, _ := h.signIn("ann@example.com")
	sess := h.session(tok.access)
	b := adminBrowser(t, h)

	// The administrator holds no rule on this repository and still sees its
	// folder tree: names of folders, never files, titles or content.
	root := b.get("/admin/access/notes")
	for _, folder := range []string{"Open", "ReadOnly", "Shared", "hidn-private"} {
		if !strings.Contains(root.body, ">"+folder+"</a>") {
			t.Errorf("the folder %s must be listed:\n%s", folder, root.body)
		}
	}
	deep := b.get("/admin/access/notes?folder=Open")
	for _, page := range []string{root.body, deep.body, b.get("/admin/access/notes?folder=hidn-private").body} {
		for _, secret := range []string{"Note.md", "file.bin", "Open note", "hidn-secret", testutil.HiddenMarker, "zebra"} {
			if strings.Contains(page, secret) {
				t.Errorf("the panel shows a file name, a note title or content: %q", secret)
			}
		}
	}
	if !strings.Contains(deep.body, ">Sub</a>") || !strings.Contains(deep.body, ">hidn-closed</a>") {
		t.Errorf("sub-folders of Open:\n%s", deep.body)
	}

	// The links the panel renders lead where they say, at any depth: a
	// folder path is escaped once, not twice.
	hrefs := regexp.MustCompile(`href="(/admin/access/notes\?folder=[^"]*)"`)
	follow := func(body, name string) page {
		t.Helper()
		for _, m := range hrefs.FindAllStringSubmatch(body, -1) {
			link := strings.ReplaceAll(m[1], "&amp;", "&")
			if u, err := url.Parse(link); err == nil && strings.HasSuffix(u.Query().Get("folder"), name) {
				return b.get(link)
			}
		}
		t.Fatalf("no link to the folder %s in:\n%s", name, body)
		return page{}
	}
	open := follow(root.body, "Open")
	sub := follow(open.body, "Sub")
	if sub.status != 200 || !strings.Contains(sub.body, `name="folder" value="Open/Sub"`) || strings.Contains(sub.body, "not in the repository any more") {
		t.Errorf("following the links to a nested folder:\n%s", sub.body)
	}
	if back := follow(sub.body, "Open"); !strings.Contains(back.body, `name="folder" value="Open"`) {
		t.Errorf("the breadcrumb leads back to the parent folder")
	}

	// Granting on a nested folder takes effect at once.
	if _, c := call(t, sess, "kb_get_note", map[string]any{"path": "notes/Shared/Team/Plan.md"}); c != "not_found" {
		t.Fatalf("before the grant: %q", c)
	}
	p := b.act("/admin/access/notes", url.Values{"account": {ann.ID}, "folder": {"Shared/Team"}, "level": {"read"}})
	if !strings.Contains(p.body, "now has read on notes/Shared/Team") {
		t.Errorf("after granting:\n%s", p.body)
	}
	if _, c := call(t, sess, "kb_get_note", map[string]any{"path": "notes/Shared/Team/Plan.md"}); c != "" {
		t.Errorf("the grant must apply to the account's next call: %q", c)
	}
	row := func(body, email string) string {
		i := strings.Index(body, ">"+email+"</a>")
		if i < 0 {
			t.Fatalf("no row for %s:\n%s", email, body)
		}
		rest := body[i:]
		return rest[:strings.Index(rest, "</tr>")]
	}
	here := row(b.get("/admin/access/notes?folder=Shared/Team").body, "ann@example.com")
	if !strings.Contains(here, `class="lv read on"`) || !strings.Contains(here, "set here") {
		t.Errorf("the rule is explicit on its folder:\n%s", here)
	}
	parent := b.get("/admin/access/notes?folder=Shared").body
	if !strings.Contains(parent, "rules inside") {
		t.Errorf("a folder with rules below it is marked:\n%s", parent)
	}
	if r := row(parent, "ann@example.com"); !strings.Contains(r, `class="lv inherit on"`) || !strings.Contains(r, "no rule applies") {
		t.Errorf("the parent folder has no rule:\n%s", r)
	}

	// Inherited levels name the folder they come from.
	b.act("/admin/access/notes", url.Values{"account": {ann.ID}, "folder": {"Open"}, "level": {"write"}})
	b.act("/admin/access/notes", url.Values{"account": {ann.ID}, "folder": {"Open/hidn-closed"}, "level": {"none"}})
	inherited := row(b.get("/admin/access/notes?folder=Open/Sub").body, "ann@example.com")
	if !strings.Contains(inherited, `class="lv write ghost"`) || !strings.Contains(inherited, "notes/Open") || !strings.Contains(inherited, `class="level write"`) {
		t.Errorf("an inherited level:\n%s", inherited)
	}
	closed := row(b.get("/admin/access/notes?folder=Open/hidn-closed").body, "ann@example.com")
	if !strings.Contains(closed, `class="lv none on"`) {
		t.Errorf("a closed sub-folder:\n%s", closed)
	}
	if _, c := call(t, sess, "kb_list", map[string]any{"folder": "notes/Open/hidn-closed"}); c != "not_found" {
		t.Errorf("the closed sub-folder as the account: %q", c)
	}
	// Clearing the deeper rule lets the folder inherit again.
	p = b.act("/admin/access/notes", url.Values{"account": {ann.ID}, "folder": {"Open/hidn-closed"}, "level": {"inherit"}})
	if !strings.Contains(p.body, "inherits its level") {
		t.Errorf("after clearing:\n%s", p.body)
	}
	closed = row(b.get("/admin/access/notes?folder=Open/hidn-closed").body, "ann@example.com")
	if !strings.Contains(closed, `class="lv write ghost"`) || !strings.Contains(closed, "notes/Open") {
		t.Errorf("after clearing, write is inherited from notes/Open:\n%s", closed)
	}
	if _, c := call(t, sess, "kb_list", map[string]any{"folder": "notes/Open/hidn-closed"}); c != "" {
		t.Errorf("the re-opened sub-folder as the account: %q", c)
	}

	// Invalid changes are refused.
	for name, form := range map[string]url.Values{
		"unknown level":   {"account": {ann.ID}, "folder": {"Open"}, "level": {"owner"}},
		"unknown folder":  {"account": {ann.ID}, "folder": {"Nope"}, "level": {"read"}},
		"unknown account": {"account": {"acc_nobody"}, "folder": {"Open"}, "level": {"read"}},
	} {
		before := len(h.m.Store.Snapshot().Rules)
		if p = b.act("/admin/access/notes", form); !strings.Contains(p.body, "flash bad") || len(h.m.Store.Snapshot().Rules) != before {
			t.Errorf("%s must be refused", name)
		}
	}
	if p = b.post("/admin/access/notes", url.Values{"account": {ann.ID}, "folder": {"../x"}, "level": {"read"}}); p.status != http.StatusBadRequest {
		t.Errorf("a traversing folder: %d", p.status)
	}

	// The per-account overview lists rules across repositories.
	h.addRepo("other")
	h.grant("ann@example.com", "other", "", acl.Read)
	overview := b.get("/admin/users/" + ann.ID)
	for _, want := range []string{">notes/Open</a>", ">notes/Shared/Team</a>", ">other</a>", `class="level write"`, `class="level read"`} {
		if !strings.Contains(overview.body, want) {
			t.Errorf("account overview lacks %q:\n%s", want, overview.body)
		}
	}
	if strings.Contains(overview.body, "folder no longer exists") {
		t.Error("no rule is orphaned yet")
	}

	// A folder that disappears from the repository leaves an orphaned rule.
	clone := testutil.CloneRemote(t, remote)
	testutil.Git(t, clone, "rm", "-rq", "Shared/Team")
	testutil.Git(t, clone, "commit", "-q", "-m", "drop the team folder")
	testutil.Git(t, clone, "push", "-q", "origin", "main")
	if err := h.m.Workspace.SyncRepo(context.Background(), "notes"); err != nil {
		t.Fatal(err)
	}
	overview = b.get("/admin/users/" + ann.ID)
	if !strings.Contains(overview.body, "folder no longer exists") {
		t.Errorf("an orphaned rule must be marked:\n%s", overview.body)
	}
	gone := b.get("/admin/access/notes?folder=Shared/Team")
	if !strings.Contains(gone.body, "not in the repository any more") {
		t.Errorf("the page of a vanished folder:\n%s", gone.body)
	}
	p = b.act("/admin/access/notes", url.Values{"account": {ann.ID}, "folder": {"Shared/Team"}, "level": {"inherit"}, "return": {"/admin/users/" + ann.ID}})
	if strings.Contains(p.body, "folder no longer exists") || strings.Contains(p.body, "notes/Shared/Team</a>") {
		t.Errorf("an orphaned rule can be cleared:\n%s", p.body)
	}

	// Every rule change is on record with the administrator who made it.
	logs := h.logs.String()
	if !strings.Contains(logs, `msg="admin: access rule changed" by=admin@example.com account=ann@example.com repository=notes folder=Shared/Team level=read`) {
		t.Errorf("rule changes must be logged with the acting account")
	}
	// The administrator still has no access to content of its own.
	adminTok, _ := h.signIn("admin@example.com")
	out, _ := call(t, h.session(adminTok.access), "kb_list", map[string]any{})
	same(t, "root as the administrator without rules", entries(out))
	// ... until it grants itself some, like anyone else.
	admin := h.m.Store.Snapshot().AccountByEmail("admin@example.com")
	b.act("/admin/access/notes", url.Values{"account": {admin.ID}, "folder": {""}, "level": {"read"}})
	out, _ = call(t, h.session(adminTok.access), "kb_list", map[string]any{})
	same(t, "root as the administrator after granting itself read", entries(out), "notes")
	_ = store.Version
	_ = workspace.StatusOK
}
