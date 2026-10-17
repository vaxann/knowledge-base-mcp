// Package oauth is a minimal OAuth 2.1 authorization server embedded in the
// MCP server so that clients such as the Claude apps, which only support
// OAuth for remote connectors, can sign in with a password and obtain bearer
// tokens. It implements RFC 8414 / RFC 9728 metadata, RFC 7591 dynamic client
// registration, the authorization code grant with PKCE (S256) and refresh
// token rotation.
//
// The sign-in step comes in two forms. A classic server has one user:
// whoever knows the password. A multi-user server signs a person in with a
// one-time code sent to their email address, and every token it issues
// belongs to that person's account.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/vaxann/knowledge-base-mcp/internal/signin"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
)

// Config configures the authorization server.
type Config struct {
	// PublicURL is the externally visible base URL (https://kb.example.com).
	// When empty it is derived from each request (Host, X-Forwarded-Proto).
	PublicURL string
	// Password is what the user types on the sign-in page.
	Password string
	// StaticToken, when set, is accepted as a bearer token too (for scripts).
	StaticToken string
	// StatePath is the JSON file that keeps clients and tokens across restarts.
	// Storage, when set, is used instead.
	StatePath string
	Storage   Storage
	// SignIn switches the sign-in page from the password to an emailed
	// one-time code and makes every token belong to an account. Accounts
	// must be set with it.
	SignIn   *signin.Flow
	Accounts *store.Store
	// FailureDelay slows down wrong passwords and wrong codes (default 1s).
	FailureDelay time.Duration
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	Logger       *slog.Logger
}

type client struct {
	ID           string    `json:"id"`
	SecretHash   string    `json:"secret_hash,omitempty"`
	RedirectURIs []string  `json:"redirect_uris"`
	Name         string    `json:"name,omitempty"`
	AuthMethod   string    `json:"auth_method"`
	CreatedAt    time.Time `json:"created_at"`
}

type tokenRec struct {
	ClientID  string    `json:"client_id"`
	Scope     string    `json:"scope,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// AccountID is the account the token acts as (multi-user servers only).
	AccountID string `json:"account_id,omitempty"`
	// Pair links an access token to the refresh token that issued it.
	Pair string `json:"pair,omitempty"`
}

type state struct {
	Clients map[string]*client   `json:"clients"`
	Access  map[string]*tokenRec `json:"access"`  // sha256(token) -> record
	Refresh map[string]*tokenRec `json:"refresh"` // sha256(token) -> record
}

type pendingAuth struct {
	ClientID      string
	RedirectURI   string
	State         string
	Scope         string
	CodeChallenge string
	Resource      string
	Expires       time.Time
	// Email is the address typed on the sign-in page, kept only to show it
	// again on the code page.
	Email string
}

type authCode struct {
	pendingAuth
	Used      bool
	AccountID string
}

// Server is the authorization server.
type Server struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	st   state
	pend map[string]*pendingAuth // req_id -> request shown on the sign-in page
	code map[string]*authCode    // code -> grant
}

// New loads (or creates) the stored state.
func New(cfg Config) (*Server, error) {
	if cfg.SignIn == nil && cfg.Password == "" {
		return nil, errors.New("oauth: a sign-in password is required (KB_OAUTH_PASSWORD or KB_HTTP_TOKEN)")
	}
	if cfg.SignIn != nil && cfg.Accounts == nil {
		return nil, errors.New("oauth: email sign-in needs the account store")
	}
	if cfg.Storage == nil && cfg.StatePath != "" {
		cfg.Storage = FileStorage(cfg.StatePath)
	}
	if cfg.FailureDelay == 0 {
		cfg.FailureDelay = time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = 24 * time.Hour
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 90 * 24 * time.Hour
	}
	s := &Server{cfg: cfg, log: cfg.Logger, pend: map[string]*pendingAuth{}, code: map[string]*authCode{},
		st: state{Clients: map[string]*client{}, Access: map[string]*tokenRec{}, Refresh: map[string]*tokenRec{}}}
	if cfg.Storage != nil {
		data, err := cfg.Storage.Load()
		if err != nil {
			return nil, err
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &s.st); err != nil {
				return nil, fmt.Errorf("oauth: parse stored state: %w", err)
			}
			s.st.normalise()
			s.gc()
		}
	}
	return s, nil
}

func (st *state) normalise() {
	if st.Clients == nil {
		st.Clients = map[string]*client{}
	}
	if st.Access == nil {
		st.Access = map[string]*tokenRec{}
	}
	if st.Refresh == nil {
		st.Refresh = map[string]*tokenRec{}
	}
}

// multi reports whether tokens belong to accounts.
func (s *Server) multi() bool { return s.cfg.SignIn != nil }

// ---- helpers ----

func randomToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func constantEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// baseURL returns the issuer for a request.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

// save persists the state; caller holds s.mu.
func (s *Server) save() {
	if s.cfg.Storage == nil {
		return
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		s.log.Error("oauth: marshal state", "err", err)
		return
	}
	if err := s.cfg.Storage.Save(data); err != nil {
		s.log.Error("oauth: cannot persist state", "err", err)
	}
}

// gc drops expired tokens; caller holds s.mu.
func (s *Server) gc() {
	now := time.Now()
	for k, t := range s.st.Access {
		if now.After(t.ExpiresAt) {
			delete(s.st.Access, k)
		}
	}
	for k, t := range s.st.Refresh {
		if now.After(t.ExpiresAt) {
			delete(s.st.Refresh, k)
		}
	}
	for k, p := range s.pend {
		if now.After(p.Expires) {
			delete(s.pend, k)
		}
	}
	for k, c := range s.code {
		if now.After(c.Expires) {
			delete(s.code, k)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// ---- metadata ----

// Mount registers all endpoints on mux.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/", s.handleASMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handlePRMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/", s.handlePRMetadata)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorizeGet)
	mux.HandleFunc("POST /oauth/authorize", s.handleAuthorizePost)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
}

func (s *Server) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, 200, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"kb"},
		"service_documentation":                 "https://github.com/vaxann/knowledge-base-mcp",
	})
}

func (s *Server) handlePRMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, 200, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"kb"},
		"resource_name":            "knowledge-base-mcp",
	})
}

// ---- dynamic client registration (RFC 7591) ----

type registration struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

func validRedirect(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Fragment != "" || p.Host == "" {
		return false
	}
	switch p.Scheme {
	case "https":
		return true
	case "http":
		h := p.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var reg registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&reg); err != nil {
		oauthError(w, 400, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(reg.RedirectURIs) == 0 {
		oauthError(w, 400, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range reg.RedirectURIs {
		if !validRedirect(u) {
			oauthError(w, 400, "invalid_redirect_uri", "redirect URIs must be https (or http://localhost): "+u)
			return
		}
	}
	method := reg.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic"
	}
	switch method {
	case "none", "client_secret_basic", "client_secret_post":
	default:
		oauthError(w, 400, "invalid_client_metadata", "unsupported token_endpoint_auth_method "+method)
		return
	}
	c := &client{ID: randomToken("kbc_"), RedirectURIs: reg.RedirectURIs, Name: reg.ClientName, AuthMethod: method, CreatedAt: time.Now()}
	resp := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"client_name":                c.Name,
		"scope":                      "kb",
	}
	if method != "none" {
		secret := randomToken("kbs_")
		c.SecretHash = hashOf(secret)
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	s.mu.Lock()
	s.st.Clients[c.ID] = c
	s.save()
	s.mu.Unlock()
	s.log.Info("oauth: client registered", "client", c.Name, "id", c.ID[:12])
	writeJSON(w, 201, resp)
}

// ---- authorization endpoint ----

var signInTmpl = template.Must(template.New("signin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign in · knowledge-base-mcp</title>
<style>
body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
form{background:#1c1c1c;padding:2rem;border-radius:12px;width:min(360px,90vw);box-shadow:0 8px 30px #0008}
h1{font-size:1.1rem;margin:0 0 .5rem}p{color:#aaa;font-size:.9rem;margin:.25rem 0 1rem}
input[type=password],input[type=email],input[type=text]{width:100%;padding:.7rem;border-radius:8px;border:1px solid #444;background:#111;color:#eee;font-size:1rem;box-sizing:border-box}
input.code{font-family:ui-monospace,monospace;font-size:1.5rem;letter-spacing:.4rem;text-align:center}
button{margin-top:1rem;width:100%;padding:.7rem;border:0;border-radius:8px;background:#4f7cff;color:#fff;font-size:1rem;cursor:pointer}
button.link{background:none;color:#8aa4ff;font-size:.9rem;margin-top:.5rem;padding:.3rem}
.err{color:#ff6b6b;font-size:.9rem;margin-top:.5rem}
</style></head><body>
<form method="post" action="{{.Action}}">
<h1>knowledge-base-mcp</h1>
<p><b>{{.Client}}</b> asks for access to your knowledge base.<br>It will be redirected to <code>{{.RedirectHost}}</code>.</p>
<input type="hidden" name="req_id" value="{{.ReqID}}">
{{if eq .Step "password"}}<input type="password" name="password" placeholder="Password" autofocus autocomplete="current-password" required>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<button type="submit">Allow access</button>
{{else if eq .Step "email"}}<input type="hidden" name="step" value="email">
<input type="email" name="email" placeholder="Email address" autofocus autocomplete="email" required>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<button type="submit">Send me a code</button>
{{else}}<input type="hidden" name="step" value="code">
<p>If <b>{{.Email}}</b> has an account here, a 6-digit code is on its way. It is valid for 10 minutes.</p>
<input class="code" type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" placeholder="000000" autofocus autocomplete="one-time-code" required>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<button type="submit">Allow access</button>
<button class="link" type="submit" name="step" value="restart" formnovalidate>Use another address or send a new code</button>
{{end}}
</form></body></html>`))

type signInView struct {
	Action, Client, RedirectHost, ReqID, Error string
	// Step is password, email or code.
	Step, Email string
}

func (s *Server) handleAuthorizeGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	c := s.st.Clients[q.Get("client_id")]
	s.mu.Unlock()
	if c == nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	registered := false
	for _, u := range c.RedirectURIs {
		if u == redirect {
			registered = true
		}
	}
	if !registered {
		http.Error(w, "redirect_uri is not registered for this client", http.StatusBadRequest)
		return
	}
	fail := func(code, desc string) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		v.Set("error", code)
		v.Set("error_description", desc)
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // redirect only goes to a URI registered by the client
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	p := &pendingAuth{ClientID: c.ID, RedirectURI: redirect, State: q.Get("state"), Scope: q.Get("scope"),
		CodeChallenge: q.Get("code_challenge"), Resource: q.Get("resource"), Expires: time.Now().Add(10 * time.Minute)}
	id := randomToken("req_")
	s.mu.Lock()
	s.gc()
	s.pend[id] = p
	s.mu.Unlock()
	step := "password"
	if s.multi() {
		step = "email"
	}
	s.render(w, c, p, id, step, "")
}

func (s *Server) render(w http.ResponseWriter, c *client, p *pendingAuth, reqID, step, errMsg string) {
	host := p.RedirectURI
	if u, err := url.Parse(p.RedirectURI); err == nil {
		host = u.Host
	}
	name := c.Name
	if name == "" {
		name = "An application"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	_ = signInTmpl.Execute(w, signInView{Action: "/oauth/authorize", Client: name, RedirectHost: host, ReqID: reqID, Error: errMsg, Step: step, Email: p.Email})
}

func (s *Server) handleAuthorizePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id := r.PostForm.Get("req_id")
	s.mu.Lock()
	p := s.pend[id]
	var c *client
	if p != nil {
		c = s.st.Clients[p.ClientID]
	}
	s.mu.Unlock()
	if p == nil || c == nil || time.Now().After(p.Expires) {
		http.Error(w, "this sign-in request has expired; start again from the client", http.StatusBadRequest)
		return
	}
	accountID := ""
	if s.multi() {
		switch r.PostForm.Get("step") {
		case "email":
			email := strings.TrimSpace(r.PostForm.Get("email"))
			if len(email) > 254 {
				email = email[:254]
			}
			s.mu.Lock()
			p.Email = email
			s.mu.Unlock()
			// Nothing here depends on whether the address has an account:
			// the page that follows is the same in every case.
			s.cfg.SignIn.Request(id, email, signin.ClientAddr(r))
			s.render(w, c, p, id, "code", "")
			return
		case "code":
			acc, ok := s.cfg.SignIn.Verify(id, r.PostForm.Get("code"))
			if !ok {
				time.Sleep(s.cfg.FailureDelay) // slow down guessing
				s.log.Warn("oauth: sign-in code rejected", "client", c.Name)
				w.WriteHeader(http.StatusUnauthorized)
				s.render(w, c, p, id, "code", "That code is not valid. Check it, or request a new one.")
				return
			}
			accountID = acc.ID
		default:
			s.render(w, c, p, id, "email", "")
			return
		}
	} else if !constantEq(r.PostForm.Get("password"), s.cfg.Password) {
		time.Sleep(s.cfg.FailureDelay) // slow down guessing
		s.log.Warn("oauth: wrong password", "client", c.Name)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, c, p, id, "password", "Wrong password.")
		return
	}
	code := randomToken("kbcode_")
	s.mu.Lock()
	delete(s.pend, id)
	s.code[code] = &authCode{pendingAuth: *p, AccountID: accountID}
	s.code[code].Expires = time.Now().Add(5 * time.Minute)
	s.mu.Unlock()
	u, _ := url.Parse(p.RedirectURI)
	v := u.Query()
	v.Set("code", code)
	if p.State != "" {
		v.Set("state", p.State)
	}
	u.RawQuery = v.Encode()
	s.log.Info("oauth: sign-in granted", "client", c.Name)
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // redirect_uri was matched against the client's registration in handleAuthorizeGet
}

// ---- token endpoint ----

func (s *Server) authenticateClient(r *http.Request) (*client, bool) {
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		id, secret = u, p
	}
	s.mu.Lock()
	c := s.st.Clients[id]
	s.mu.Unlock()
	if c == nil {
		return nil, false
	}
	if c.AuthMethod == "none" {
		return c, true
	}
	if secret == "" || !constantEq(hashOf(secret), c.SecretHash) {
		return nil, false
	}
	return c, true
}

func pkceOK(verifier, challenge string) bool {
	if verifier == "" || len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return constantEq(base64.RawURLEncoding.EncodeToString(sum[:]), challenge)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "form body required")
		return
	}
	c, ok := s.authenticateClient(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="knowledge-base-mcp"`)
		oauthError(w, 401, "invalid_client", "client authentication failed")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.mu.Lock()
		s.gc()
		ac := s.code[r.PostForm.Get("code")]
		if ac != nil {
			delete(s.code, r.PostForm.Get("code")) // single use
		}
		s.mu.Unlock()
		if ac == nil || ac.ClientID != c.ID {
			oauthError(w, 400, "invalid_grant", "unknown, expired or already used code")
			return
		}
		if ru := r.PostForm.Get("redirect_uri"); ru != "" && ru != ac.RedirectURI {
			oauthError(w, 400, "invalid_grant", "redirect_uri mismatch")
			return
		}
		if !pkceOK(r.PostForm.Get("code_verifier"), ac.CodeChallenge) {
			oauthError(w, 400, "invalid_grant", "PKCE verification failed")
			return
		}
		s.issue(w, c, ac.Scope, ac.AccountID)
	case "refresh_token":
		rt := r.PostForm.Get("refresh_token")
		h := hashOf(rt)
		s.mu.Lock()
		rec := s.st.Refresh[h]
		if rec != nil && (rec.ClientID != c.ID || time.Now().After(rec.ExpiresAt)) {
			rec = nil
		}
		if rec != nil && s.multi() && !s.accountActive(rec.AccountID) {
			rec = nil
		}
		if rec != nil {
			delete(s.st.Refresh, h) // rotation
			for k, a := range s.st.Access {
				if a.Pair == h {
					delete(s.st.Access, k)
				}
			}
		}
		s.mu.Unlock()
		if rec == nil {
			oauthError(w, 400, "invalid_grant", "unknown or expired refresh token")
			return
		}
		s.issue(w, c, rec.Scope, rec.AccountID)
	default:
		oauthError(w, 400, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) issue(w http.ResponseWriter, c *client, scope, accountID string) {
	if scope == "" {
		scope = "kb"
	}
	access, refresh := randomToken("kb_at_"), randomToken("kb_rt_")
	now := time.Now()
	rh := hashOf(refresh)
	s.mu.Lock()
	s.st.Refresh[rh] = &tokenRec{ClientID: c.ID, Scope: scope, ExpiresAt: now.Add(s.cfg.RefreshTTL), AccountID: accountID}
	s.st.Access[hashOf(access)] = &tokenRec{ClientID: c.ID, Scope: scope, ExpiresAt: now.Add(s.cfg.AccessTTL), Pair: rh, AccountID: accountID}
	s.save()
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(s.cfg.AccessTTL.Seconds()),
		"refresh_token": refresh, "scope": scope,
	})
}

// ---- resource protection ----

// accountActive reports whether an account exists and is not disabled.
func (s *Server) accountActive(id string) bool {
	if id == "" {
		return false
	}
	a := s.cfg.Accounts.Snapshot().Accounts[id]
	return a != nil && !a.Disabled
}

// adminAccount returns the id of the administrator account.
func (s *Server) adminAccount() string {
	for _, a := range s.cfg.Accounts.Snapshot().Accounts {
		if a.Admin && !a.Disabled {
			return a.ID
		}
	}
	return ""
}

// Verify checks a bearer token. On a multi-user server it also returns the
// account the token acts as: the static token acts as the administrator, an
// OAuth token as the account that signed in, provided it is still active.
func (s *Server) Verify(token string) (accountID string, expires time.Time, ok bool) {
	if token == "" {
		return "", time.Time{}, false
	}
	if s.cfg.StaticToken != "" && constantEq(token, s.cfg.StaticToken) {
		exp := time.Now().Add(24 * time.Hour)
		if !s.multi() {
			return "", exp, true
		}
		id := s.adminAccount()
		return id, exp, id != ""
	}
	s.mu.Lock()
	rec := s.st.Access[hashOf(token)]
	s.mu.Unlock()
	if rec == nil || !time.Now().Before(rec.ExpiresAt) {
		return "", time.Time{}, false
	}
	if s.multi() && !s.accountActive(rec.AccountID) {
		// Also rejects tokens issued before the switch to multi-user mode,
		// which belong to nobody.
		return "", time.Time{}, false
	}
	return rec.AccountID, rec.ExpiresAt, true
}

// Valid reports whether a bearer token grants access.
func (s *Server) Valid(token string) bool {
	_, _, ok := s.Verify(token)
	return ok
}

// RevokeAccount deletes every token of an account and returns how many
// sessions (refresh tokens) that ended.
func (s *Server) RevokeAccount(accountID string) int {
	if accountID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, t := range s.st.Refresh {
		if t.AccountID == accountID {
			delete(s.st.Refresh, k)
			n++
		}
	}
	for k, t := range s.st.Access {
		if t.AccountID == accountID {
			delete(s.st.Access, k)
		}
	}
	s.save()
	return n
}

// Sessions returns the number of live sessions (refresh tokens) per account.
func (s *Server) Sessions() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := map[string]int{}
	for _, t := range s.st.Refresh {
		if t.AccountID != "" && now.Before(t.ExpiresAt) {
			out[t.AccountID]++
		}
	}
	return out
}

// Middleware protects a handler with bearer tokens and advertises the
// protected resource metadata on 401 (RFC 9728), which is how MCP clients
// discover the sign-in flow.
func (s *Server) Middleware(next http.Handler) http.Handler {
	if s.multi() {
		return s.accountMiddleware(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.Valid(strings.TrimSpace(tok)) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="knowledge-base-mcp", resource_metadata="%s/.well-known/oauth-protected-resource"`, s.baseURL(r)))
			oauthError(w, 401, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accountMiddleware is Middleware for a multi-user server. It records the
// account in the request through the MCP SDK, which then passes it to tool
// handlers and pins the MCP session to that account, so a session cannot be
// continued with someone else's token.
func (s *Server) accountMiddleware(next http.Handler) http.Handler {
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		id, exp, ok := s.Verify(token)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: id, Expiration: exp, Scopes: []string{"kb"}}, nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opts := &auth.RequireBearerTokenOptions{ResourceMetadataURL: s.baseURL(r) + "/.well-known/oauth-protected-resource"}
		auth.RequireBearerToken(verifier, opts)(next).ServeHTTP(w, r)
	})
}

// AccountOf returns the account a request's bearer token acts as.
func (s *Server) AccountOf(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	id, _, ok := s.Verify(strings.TrimSpace(tok))
	return id, ok
}
