package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/mail"
	"github.com/vaxann/knowledge-base-mcp/internal/oauth"
	"github.com/vaxann/knowledge-base-mcp/internal/signin"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

const redirectURI = "https://client.example/callback"

// outbox is a mail sender that keeps what it is asked to deliver.
type outbox struct {
	mu   sync.Mutex
	msgs []mail.Message
	fail error
}

func (o *outbox) Send(_ context.Context, m mail.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.msgs = append(o.msgs, m)
	return nil
}

func (o *outbox) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.msgs)
}

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

// lastCode returns the code of the newest email to an address.
func (o *outbox) lastCode(to string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.msgs) - 1; i >= 0; i-- {
		if o.msgs[i].To == to {
			return sixDigits.FindString(o.msgs[i].Text)
		}
	}
	return ""
}

type harness struct {
	t    *testing.T
	m    *Multi
	ts   *httptest.Server
	mail *outbox
	logs *bytes.Buffer
	cfg  config.Config
	http *http.Client // does not follow redirects
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := config.Default()
	cfg.Server.HTTP.Listen = "127.0.0.1:0"
	cfg.Server.DataDir = dataDir
	cfg.Search.IndexDir = filepath.Join(dataDir, "index")
	cfg.Server.Accounts = config.Accounts{AdminEmail: "admin@example.com", MailFrom: "KB <kb@example.com>", ResendAPIKey: "re_test",
		SecretKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))}
	cfg.Git.PushDebounce, cfg.Git.PullInterval, cfg.Git.PullFreshness = 0, 0, 0
	return cfg
}

func start(t *testing.T, cfg config.Config) *harness {
	t.Helper()
	h := &harness{t: t, mail: &outbox{}, logs: &bytes.Buffer{}, cfg: cfg,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: h.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, err := NewMulti(context.Background(), Options{Config: cfg, Log: log, Version: "test", Sender: h.mail, FailureDelay: 40 * time.Millisecond, AllowLocalRemotes: true})
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

// lockedBuffer is a log sink safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (h *harness) ready() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.m.Workspace.WaitReady(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) addAccount(email string) {
	h.t.Helper()
	if err := h.m.Store.Update(func(s *store.State) error { _, err := s.AddAccount(email, false, time.Now()); return err }); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) addRepo(alias string) string {
	h.t.Helper()
	_, remote := testutil.NewAccessRepo(h.t)
	if _, err := h.m.Workspace.AddRepo(context.Background(), workspace.AddRepoRequest{Alias: alias, URL: remote}); err != nil {
		h.t.Fatal(err)
	}
	h.ready()
	return remote
}

func (h *harness) grant(email, alias, path string, level acl.Level) {
	h.t.Helper()
	err := h.m.Store.Update(func(s *store.State) error {
		return s.SetRule(s.AccountByEmail(email).ID, s.RepoByAlias(alias).ID, path, level)
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) register() string {
	h.t.Helper()
	body := `{"redirect_uris":["` + redirectURI + `"],"client_name":"Test client","token_endpoint_auth_method":"none"}`
	resp, err := h.http.Post(h.ts.URL+"/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var reg map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	if resp.StatusCode != 201 {
		h.t.Fatalf("register: %d %v", resp.StatusCode, reg)
	}
	return reg["client_id"].(string)
}

const verifier = "vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"

var reqIDRE = regexp.MustCompile(`name="req_id" value="([^"]+)"`)

// page is a response of the sign-in flow.
type page struct {
	status   int
	body     string
	location string
}

func (h *harness) do(req *http.Request) page {
	h.t.Helper()
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return page{status: resp.StatusCode, body: string(raw), location: resp.Header.Get("Location")}
}

func (h *harness) get(path string) page {
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	return h.do(req)
}

func (h *harness) post(path string, form url.Values) page {
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return h.do(req)
}

// begin opens the sign-in page and returns its request id.
func (h *harness) begin(clientID string) (string, page) {
	h.t.Helper()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {"st"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	p := h.get("/oauth/authorize?" + q.Encode())
	m := reqIDRE.FindStringSubmatch(p.body)
	if p.status != 200 || m == nil {
		h.t.Fatalf("sign-in page: %d\n%s", p.status, p.body)
	}
	return m[1], p
}

func (h *harness) sendEmail(reqID, email string) page {
	p := h.post("/oauth/authorize", url.Values{"req_id": {reqID}, "step": {"email"}, "email": {email}})
	h.m.SignIn.Wait()
	return p
}

func (h *harness) sendCode(reqID, code string) page {
	return h.post("/oauth/authorize", url.Values{"req_id": {reqID}, "step": {"code"}, "code": {code}})
}

type tokens struct{ access, refresh string }

func (h *harness) exchange(form url.Values) (tokens, int) {
	h.t.Helper()
	resp, err := h.http.PostForm(h.ts.URL+"/oauth/token", form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		return tokens{}, resp.StatusCode
	}
	return tokens{out["access_token"].(string), out["refresh_token"].(string)}, 200
}

// signIn runs the whole flow for an address and returns its tokens.
func (h *harness) signIn(email string) (tokens, string) {
	h.t.Helper()
	clientID := h.register()
	reqID, _ := h.begin(clientID)
	h.sendEmail(reqID, email)
	code := h.mail.lastCode(email)
	if code == "" {
		h.t.Fatalf("no sign-in email for %s", email)
	}
	p := h.sendCode(reqID, code)
	u, err := url.Parse(p.location)
	if p.status != 302 || err != nil || u.Query().Get("code") == "" || u.Query().Get("state") != "st" {
		h.t.Fatalf("code accepted: %d %q\n%s", p.status, p.location, p.body)
	}
	tok, status := h.exchange(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}, "redirect_uri": {redirectURI}})
	if status != 200 {
		h.t.Fatalf("token exchange: %d", status)
	}
	return tok, clientID
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (h *harness) connect(token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "1"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
}

// works reports whether a token opens a session.
func (h *harness) works(token string) bool {
	sess, err := h.connect(token)
	if err != nil {
		return false
	}
	_ = sess.Close()
	return true
}

// relaxLimits lets a test sign the same address in several times in a row.
func (h *harness) relaxLimits() {
	h.m.SignIn.Limiter = signin.NewLimiter(signin.Limits{AddrInterval: 0, AddrPerHour: 1000, ClientPerHour: 1000, TotalPerHour: 1000}, nil)
}

func (h *harness) session(token string) *mcp.ClientSession {
	h.t.Helper()
	sess, err := h.connect(token)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	h.t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// call invokes a tool. It returns the decoded result and, for a tool error,
// its code.
func call(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	out := map[string]any{}
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		if json.Unmarshal(raw, &out) != nil {
			var items []any
			_ = json.Unmarshal(raw, &items)
			out["items"] = items
		}
	} else if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	if res.IsError {
		c, _ := out["code"].(string)
		return out, c
	}
	return out, ""
}

func entries(out map[string]any) []string {
	var paths []string
	list, _ := out["entries"].([]any)
	for _, e := range list {
		paths = append(paths, e.(map[string]any)["path"].(string))
	}
	return paths
}

func same(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestFirstStartCreatesTheAdministrator(t *testing.T) {
	h := start(t, testConfig(t))
	st := h.m.Store.Snapshot()
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %d, want exactly the administrator", len(st.Accounts))
	}
	a := st.AccountByEmail("ADMIN@example.com")
	if a == nil || !a.Admin || a.Disabled {
		t.Fatalf("administrator: %+v", a)
	}
	if info, err := os.Stat(StatePath(h.cfg)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file: %v", err)
	}
	if _, err := NewMulti(context.Background(), Options{Config: config.Default()}); err == nil {
		t.Error("NewMulti must refuse a classic configuration")
	}
}

func TestEmailSignInEndToEnd(t *testing.T) {
	h := start(t, testConfig(t))
	h.addAccount("ann@example.com")
	h.addRepo("notes")
	h.grant("ann@example.com", "notes", "Open", acl.Write)

	// Discovery still starts from a 401 on /mcp.
	resp, err := h.http.Get(h.ts.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+h.ts.URL+`/.well-known/oauth-protected-resource"`) {
		t.Fatalf("discovery: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	clientID := h.register()
	reqID, first := h.begin(clientID)
	if strings.Contains(first.body, `type="password"`) || !strings.Contains(first.body, `type="email"`) {
		t.Fatalf("a multi-user sign-in page asks for an address, never a password:\n%s", first.body)
	}
	if p := h.post("/oauth/authorize", url.Values{"req_id": {reqID}, "password": {"anything"}}); p.status != 200 || p.location != "" || !strings.Contains(p.body, `type="email"`) {
		t.Errorf("a password grants nothing: %d %q", p.status, p.location)
	}
	codePage := h.sendEmail(reqID, "Ann@Example.com")
	if codePage.status != 200 || !strings.Contains(codePage.body, `name="code"`) {
		t.Fatalf("code page: %d\n%s", codePage.status, codePage.body)
	}
	code := h.mail.lastCode("ann@example.com")
	if code == "" || h.mail.count() != 1 {
		t.Fatalf("emails sent: %d", h.mail.count())
	}

	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	began := time.Now()
	bad := h.sendCode(reqID, wrong)
	if bad.status != 401 || bad.location != "" || time.Since(began) < 40*time.Millisecond {
		t.Errorf("a wrong code must be refused and delayed: %d after %v", bad.status, time.Since(began))
	}
	ok := h.sendCode(reqID, code)
	u, _ := url.Parse(ok.location)
	if ok.status != 302 || !strings.HasPrefix(ok.location, redirectURI) || u.Query().Get("code") == "" {
		t.Fatalf("the right code redirects to the client: %d %q", ok.status, ok.location)
	}
	if again := h.sendCode(reqID, code); again.status == 302 {
		t.Error("a code and its sign-in request are single use")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}}
	tok, status := h.exchange(form)
	if status != 200 || tok.access == "" || tok.refresh == "" {
		t.Fatalf("token exchange: %d", status)
	}
	if _, status := h.exchange(form); status == 200 {
		t.Error("an authorization code is single use")
	}

	sess := h.session(tok.access)
	out, code2 := call(t, sess, "kb_list", map[string]any{})
	if code2 != "" {
		t.Fatalf("kb_list: %v", out)
	}
	same(t, "root as ann", entries(out), "notes")
	info, _ := call(t, sess, "kb_info", map[string]any{})
	if info["account"] != "ann@example.com" {
		t.Errorf("the session acts as the account that signed in: %v", info)
	}
	if _, c := call(t, sess, "kb_get_note", map[string]any{"path": "notes/hidn-private/hidn-secret.md"}); c != "not_found" {
		t.Errorf("outside the account's rules: %q", c)
	}

	// Refresh keeps the account and retires the old pair.
	next, status := h.exchange(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {tok.refresh}})
	if status != 200 {
		t.Fatalf("refresh: %d", status)
	}
	if _, err := h.connect(tok.access); err == nil {
		t.Error("the previous access token must stop working after a refresh")
	}
	info, _ = call(t, h.session(next.access), "kb_info", map[string]any{})
	if info["account"] != "ann@example.com" {
		t.Errorf("a refreshed token keeps its account: %v", info)
	}
	if got := h.m.OAuth.Sessions()[h.m.Store.Snapshot().AccountByEmail("ann@example.com").ID]; got != 1 {
		t.Errorf("live sessions of the account = %d, want 1", got)
	}
	if a := h.m.Store.Snapshot().AccountByEmail("ann@example.com"); a.LastSignIn.IsZero() {
		t.Error("the sign-in time must be recorded")
	}
}

func TestSignInRevealsNothing(t *testing.T) {
	h := start(t, testConfig(t))
	h.addAccount("ann@example.com")
	h.addAccount("off@example.com")
	if err := h.m.Store.Update(func(s *store.State) error { s.AccountByEmail("off@example.com").Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	clientID := h.register()
	// Every address kind must produce the same page, apart from the address
	// echoed back and the request id.
	normal := func(p page, reqID, email string) string {
		return strings.ReplaceAll(strings.ReplaceAll(p.body, reqID, "<req>"), email, "<email>")
	}
	type result struct {
		status          int
		afterEmail      string
		wrongCodeStatus int
		afterWrongCode  string
	}
	run := func(email string) result {
		reqID, _ := h.begin(clientID)
		p := h.sendEmail(reqID, email)
		bad := h.sendCode(reqID, "000000")
		return result{p.status, normal(p, reqID, email), bad.status, normal(bad, reqID, email)}
	}
	known := run("ann@example.com")
	if h.mail.count() != 1 {
		t.Fatalf("the known address must get its email: %d", h.mail.count())
	}
	for _, email := range []string{"off@example.com", "nobody@example.com", "not-an-address"} {
		before := h.mail.count()
		if got := run(email); got != known {
			t.Errorf("%s is told apart from a known address:\n%+v\nvs\n%+v", email, got, known)
		}
		if h.mail.count() != before {
			t.Errorf("%s: no mail may be sent", email)
		}
	}
	if n := len(h.m.Store.Snapshot().Accounts); n != 3 {
		t.Errorf("a sign-in attempt must not create an account: %d accounts", n)
	}

	// A provider failure shows the same page too.
	h.mail.mu.Lock()
	h.mail.fail = errors.New("provider down")
	h.mail.mu.Unlock()
	h.addAccount("bob@example.com")
	if got := run("bob@example.com"); got != known {
		t.Errorf("a delivery failure is visible on the page:\n%+v", got)
	}
	if !strings.Contains(h.logs.String(), "not delivered") {
		t.Error("the delivery failure must be logged")
	}
	h.mail.mu.Lock()
	h.mail.fail = nil
	h.mail.mu.Unlock()

	// A second request for the same address within a minute sends nothing
	// and looks the same.
	before := h.mail.count()
	if got := run("ann@example.com"); got != known {
		t.Errorf("a rate-limited request is visible on the page:\n%+v", got)
	}
	if h.mail.count() != before {
		t.Error("a request over the limit must send no mail")
	}
}

func TestSecretsStayOutOfLogsAndState(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.HTTP.Token = "static-recovery-token-value"
	h := start(t, cfg)
	h.addAccount("ann@example.com")
	tok, _ := h.signIn("ann@example.com")
	code := h.mail.lastCode("ann@example.com")
	_ = h.session(tok.access)
	_ = h.session(cfg.Server.HTTP.Token)
	state, err := os.ReadFile(StatePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"access token": tok.access, "refresh token": tok.refresh, "static token": cfg.Server.HTTP.Token,
		"mail key": cfg.Server.Accounts.ResendAPIKey, "secret key": cfg.Server.Accounts.SecretKey} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("the %s appears in the logs", name)
		}
		if strings.Contains(string(state), secret) {
			t.Errorf("the %s appears in the state file", name)
		}
	}
	// The code is six digits: look for it as a whole token.
	if regexp.MustCompile(`\b`+code+`\b`).MatchString(h.logs.String()) || regexp.MustCompile(`"`+code+`"`).Match(state) {
		t.Error("the one-time code appears in the logs or the state file")
	}
	if !strings.Contains(h.logs.String(), "ann@example.com") {
		t.Error("logs may, and should, name the account and the outcome")
	}
}

func TestDisabledAccountAndSessionPinning(t *testing.T) {
	h := start(t, testConfig(t))
	h.addAccount("ann@example.com")
	h.addAccount("bob@example.com")
	ann, _ := h.signIn("ann@example.com")
	bob, _ := h.signIn("bob@example.com")

	// A session opened by ann cannot be continued with bob's token.
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	raw := func(token, session, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}
	resp := raw(ann.access, "", initBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d session=%q", resp.StatusCode, sid)
	}
	listTools := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	if got := raw(ann.access, sid, listTools).StatusCode; got != 200 {
		t.Fatalf("the session's own account: %d", got)
	}
	if got := raw(bob.access, sid, listTools).StatusCode; got != 403 {
		t.Errorf("another account on the same session: %d, want 403", got)
	}

	sess := h.session(ann.access)
	if _, c := call(t, sess, "kb_info", map[string]any{}); c != "" {
		t.Fatalf("before disabling: %q", c)
	}
	id := h.m.Store.Snapshot().AccountByEmail("ann@example.com").ID
	if err := h.m.Store.Update(func(s *store.State) error { s.Accounts[id].Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := raw(ann.access, sid, listTools).StatusCode; got != 401 {
		t.Errorf("the next request of a disabled account: %d, want 401", got)
	}
	if _, err := h.connect(ann.access); err == nil {
		t.Error("a disabled account cannot open a session")
	}
	if _, status := h.exchange(url.Values{"grant_type": {"refresh_token"}, "client_id": {h.register()}, "refresh_token": {ann.refresh}}); status == 200 {
		t.Error("a disabled account cannot refresh")
	}
	if _, c := call(t, h.session(bob.access), "kb_info", map[string]any{}); c != "" {
		t.Errorf("other accounts are unaffected: %q", c)
	}
	// Revoking sessions ends them at once, for an active account too.
	bobID := h.m.Store.Snapshot().AccountByEmail("bob@example.com").ID
	if n := h.m.OAuth.RevokeAccount(bobID); n != 1 {
		t.Errorf("revoked sessions = %d, want 1", n)
	}
	if _, err := h.connect(bob.access); err == nil {
		t.Error("a revoked token must stop working")
	}
}

func TestStaticTokenActsAsTheAdministratorAccount(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.HTTP.Token = "static-recovery-token-value"
	h := start(t, cfg)
	h.addRepo("notes")
	sess := h.session(cfg.Server.HTTP.Token)
	out, _ := call(t, sess, "kb_list", map[string]any{})
	same(t, "root with the static token and no rules", entries(out))
	if _, c := call(t, sess, "kb_get_note", map[string]any{"path": "notes/Open/Note.md"}); c != "not_found" {
		t.Errorf("the static token has no access of its own: %q", c)
	}
	info, _ := call(t, sess, "kb_info", map[string]any{})
	if info["account"] != "admin@example.com" {
		t.Errorf("the static token acts as the administrator account: %v", info)
	}
	h.grant("admin@example.com", "notes", "Shared/Team", acl.Read)
	out, _ = call(t, sess, "kb_list", map[string]any{"folder": "notes"})
	same(t, "the static token follows the administrator's rules", entries(out), "notes/Shared")
	if _, err := h.connect("some-other-token"); err == nil {
		t.Error("an unknown token must be refused")
	}
}

func TestSwitchFromClassicAndBack(t *testing.T) {
	cfg := testConfig(t)
	vaultDir, _ := testutil.NewAccessRepo(t)
	cfg.Vault.Path = vaultDir
	classicFile := filepath.Join(cfg.Search.IndexDir, "oauth-state.json")

	// The deployment ran in classic mode: one client is registered and holds
	// a token obtained with the password.
	classic := func() (*oauth.Server, *httptest.Server) {
		s, err := oauth.New(oauth.Config{Password: "hunter2", StatePath: classicFile, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		s.Mount(mux)
		return s, httptest.NewServer(mux)
	}
	cs, cts := classic()
	hc := &harness{t: t, ts: cts, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	clientID := hc.register()
	reqID, _ := hc.begin(clientID)
	granted := hc.post("/oauth/authorize", url.Values{"req_id": {reqID}, "password": {"hunter2"}})
	u, _ := url.Parse(granted.location)
	old, status := hc.exchange(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}})
	if status != 200 || !cs.Valid(old.access) {
		t.Fatalf("classic sign-in: %d", status)
	}
	cts.Close()
	before, _ := os.ReadFile(classicFile)

	h := start(t, cfg)
	if _, err := h.connect(old.access); err == nil {
		t.Error("a token issued in classic mode belongs to no account and must be refused")
	}
	if _, status := h.exchange(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {old.refresh}}); status == 200 {
		t.Error("a classic refresh token must be refused")
	}
	// The client is still known: it signs in again without registering again.
	newReq, p := h.begin(clientID)
	if !strings.Contains(p.body, "Test client") {
		t.Errorf("the registered client must be kept:\n%s", p.body)
	}
	h.sendEmail(newReq, "admin@example.com")
	ok := h.sendCode(newReq, h.mail.lastCode("admin@example.com"))
	u, _ = url.Parse(ok.location)
	tok, status := h.exchange(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}})
	if status != 200 {
		t.Fatalf("sign-in of the migrated client: %d", status)
	}
	// The vault is served under its alias, for the administrator only.
	sess, err := h.connect(tok.access)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, sess, "kb_list", map[string]any{})
	same(t, "root after the switch", entries(out), "main")
	_ = sess.Close()

	// Rollback: the classic file was never touched, so classic sessions work.
	h.ts.Close()
	_ = h.m.Close()
	if after, _ := os.ReadFile(classicFile); !bytes.Equal(before, after) {
		t.Fatal("the classic OAuth file must stay untouched")
	}
	cs, cts = classic()
	defer cts.Close()
	if !cs.Valid(old.access) {
		t.Error("removing the administrator address must restore classic sessions")
	}
}

func TestFileRoutesFollowAccessRules(t *testing.T) {
	cfg := testConfig(t)
	h := start(t, cfg)
	h.addAccount("ann@example.com")
	h.addRepo("notes")
	h.grant("ann@example.com", "notes", "Open", acl.Write)
	h.grant("ann@example.com", "notes", "ReadOnly", acl.Read)
	tok, _ := h.signIn("ann@example.com")

	do := func(method, target, token string, body io.Reader) (int, string) {
		req, _ := http.NewRequest(method, target, body)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := h.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	base := h.ts.URL
	if status, body := do("GET", base+"/files/notes/Open/file.bin", tok.access, nil); status != 200 || body != "open-bytes" {
		t.Errorf("download with a token: %d %q", status, body)
	}
	if status, _ := do("GET", base+"/files/notes/Open/file.bin", "", nil); status != 403 {
		t.Errorf("download without a token or link: %d", status)
	}
	hiddenStatus, hiddenBody := do("GET", base+"/files/notes/hidn-private/hidn-blob.bin", tok.access, nil)
	missingStatus, missingBody := do("GET", base+"/files/notes/hidn-private/absent.bin", tok.access, nil)
	if hiddenStatus != 404 || missingStatus != 404 || strings.Contains(hiddenBody, testutil.HiddenMarker) ||
		strings.ReplaceAll(hiddenBody, "hidn-blob", "x") != strings.ReplaceAll(missingBody, "absent", "x") {
		t.Errorf("a hidden file and a missing one must answer alike: %d %q / %d %q", hiddenStatus, hiddenBody, missingStatus, missingBody)
	}
	if status, _ := do("GET", base+"/files/nope/a.bin", tok.access, nil); status != 404 {
		t.Errorf("unknown alias: %d", status)
	}
	if status, _ := do("PUT", base+"/files/notes/ReadOnly/x.bin", tok.access, strings.NewReader("x")); status != 403 {
		t.Errorf("upload into a read-only folder: %d", status)
	}
	if status, _ := do("PUT", base+"/files/notes/hidn-private/x.bin", tok.access, strings.NewReader("x")); status != 404 {
		t.Errorf("upload into a hidden folder: %d", status)
	}
	if status, body := do("PUT", base+"/files/notes/Open/up.bin", tok.access, strings.NewReader("uploaded")); status != 201 || !strings.Contains(body, `"path":"notes/Open/up.bin"`) {
		t.Errorf("upload with a token: %d %s", status, body)
	}

	// Signed links act as the account that created them.
	h.m.Server.SetPublicURL(base)
	sess := h.session(tok.access)
	out, c := call(t, sess, "kb_file_link", map[string]any{"path": "notes/Open/file.bin"})
	link, _ := out["url"].(string)
	if c != "" || !strings.HasPrefix(link, base+"/files/notes/Open/file.bin?") {
		t.Fatalf("download link: %v", out)
	}
	if status, body := do("GET", link, "", nil); status != 200 || body != "open-bytes" {
		t.Errorf("signed download: %d %q", status, body)
	}
	if status, _ := do("GET", strings.Replace(link, "file.bin", "Note.md", 1), "", nil); status != 403 {
		t.Errorf("a link is bound to its file: %d", status)
	}
	out, c = call(t, sess, "kb_upload_link", map[string]any{"path": "notes/Open/"})
	up, _ := out["url"].(string)
	if c != "" || up == "" {
		t.Fatalf("upload link: %v", out)
	}
	if status, body := do("GET", up, "", nil); status != 200 || !strings.Contains(body, "notes/Open/") {
		t.Errorf("upload page: %d", status)
	}
	if _, c := call(t, sess, "kb_upload_link", map[string]any{"path": "notes/ReadOnly/"}); c != "forbidden" {
		t.Errorf("upload link into a read-only folder: %q", c)
	}

	// Access is checked again when a link is used.
	h.grant("ann@example.com", "notes", "Open", acl.Read)
	if status, _ := do("GET", up, "", nil); status != 403 {
		t.Errorf("an upload link after write access was removed: %d", status)
	}
	if status, _ := do("GET", link, "", nil); status != 200 {
		t.Errorf("a download link still has the level it needs: %d", status)
	}
	h.grant("ann@example.com", "notes", "Open", acl.None)
	if status, _ := do("GET", link, "", nil); status != 403 {
		t.Errorf("a download link after access was removed: %d, want 403", status)
	}
}
