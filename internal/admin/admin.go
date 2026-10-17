// Package admin is the web panel of a multi-user server: accounts,
// repositories and access rules. Pages are rendered on the server; every
// change is a form post protected by a session cookie and an anti-forgery
// token. The panel shows folder names, never file names or note content, and
// never a credential.
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/oauth"
	"github.com/vaxann/knowledge-base-mcp/internal/signin"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

//go:embed web/*.html web/*.css web/*.js
var webFS embed.FS

// SessionTTL is how long an administrator session lasts.
const SessionTTL = 12 * time.Hour

// Config wires the panel to the server.
type Config struct {
	Store     *store.Store
	Workspace *workspace.Workspace
	OAuth     *oauth.Server
	SignIn    *signin.Flow
	// StaticToken, when set, is accepted on the sign-in page as a recovery
	// sign-in, for when mail cannot be delivered.
	StaticToken string
	// Secure marks the session cookie HTTPS-only. It must be true unless
	// the server is reached over plain HTTP on a loopback address.
	Secure bool
	// FailureDelay slows down wrong codes and tokens (default 1s).
	FailureDelay time.Duration
	Log          *slog.Logger
	Now          func() time.Time
}

type session struct {
	accountID string
	csrf      string
	expires   time.Time
	flash     string
	flashErr  bool
}

type pendingSignIn struct {
	email   string
	expires time.Time
}

// Panel is the admin panel.
type Panel struct {
	cfg      Config
	log      *slog.Logger
	tmpl     map[string]*template.Template
	mu       sync.Mutex
	sessions map[string]*session       // sha256(cookie) -> session
	pending  map[string]*pendingSignIn // sign-in request id -> state
}

// New builds the panel.
func New(cfg Config) *Panel {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.FailureDelay == 0 {
		cfg.FailureDelay = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p := &Panel{cfg: cfg, log: cfg.Log, sessions: map[string]*session{}, pending: map[string]*pendingSignIn{}, tmpl: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
	}
	for _, page := range []string{"signin", "users", "user", "repos", "repo", "access"} {
		p.tmpl[page] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+page+".html"))
	}
	return p
}

// Mount registers the panel under /admin.
func (p *Panel) Mount(mux *http.ServeMux) {
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /admin/static/", p.headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".css") && !strings.HasSuffix(r.URL.Path, ".js") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.StripPrefix("/admin/static/", files).ServeHTTP(w, r)
	})))
	open := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, p.headers(h)) }
	guarded := func(pattern string, h func(http.ResponseWriter, *http.Request, *session)) {
		mux.Handle(pattern, p.headers(p.guard(h)))
	}
	open("GET /admin/signin", p.signInPage)
	open("POST /admin/signin", p.signInPost)
	guarded("POST /admin/signout", p.signOut)
	guarded("GET /admin", func(w http.ResponseWriter, r *http.Request, _ *session) {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	})
	guarded("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request, _ *session) {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	})
	guarded("GET /admin/users", p.usersPage)
	guarded("POST /admin/users", p.userCreate)
	guarded("GET /admin/users/{id}", p.userPage)
	guarded("POST /admin/users/{id}/{action}", p.userAction)
	guarded("GET /admin/repos", p.reposPage)
	guarded("POST /admin/repos", p.repoCreate)
	guarded("GET /admin/repos/{alias}", p.repoPage)
	guarded("POST /admin/repos/{alias}/{action}", p.repoAction)
	guarded("GET /admin/access/{alias}", p.accessPage)
	guarded("POST /admin/access/{alias}", p.accessPost)
}

// ---- plumbing ----

func (p *Panel) cookieName() string {
	if p.cfg.Secure {
		return "__Host-kb_admin"
	}
	return "kb_admin"
}

func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// headers adds the security headers every panel response carries.
func (p *Panel) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// Not "no-referrer": with it a browser sends "Origin: null" on the
		// panel's own form posts, and the origin check below would refuse them.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// sessionOf returns the live administrator session of a request.
func (p *Panel) sessionOf(r *http.Request) *session {
	c, err := r.Cookie(p.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	for k, s := range p.sessions {
		if now.After(s.expires) {
			delete(p.sessions, k)
		}
	}
	s := p.sessions[hashOf(c.Value)]
	if s == nil {
		return nil
	}
	// The account must still be the active administrator.
	a := p.cfg.Store.Snapshot().Accounts[s.accountID]
	if a == nil || !a.Admin || a.Disabled {
		delete(p.sessions, hashOf(c.Value))
		return nil
	}
	return s
}

// sameOrigin rejects a post whose Origin names another site.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // the anti-forgery token still has to match
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host || (r.Header.Get("X-Forwarded-Host") != "" && u.Host == r.Header.Get("X-Forwarded-Host"))
}

// guard requires an administrator session and, for state-changing requests,
// a valid anti-forgery token from the same origin.
func (p *Panel) guard(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := p.sessionOf(r)
		if s == nil {
			if r.Method != http.MethodGet {
				http.Error(w, "sign in first", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/admin/signin", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet {
			r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(s.csrf)) != 1 {
				http.Error(w, "this form has expired or did not come from the admin panel; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		h(w, r, s)
	})
}

// startSession signs the administrator in and redirects to the panel.
func (p *Panel) startSession(w http.ResponseWriter, r *http.Request, accountID string) {
	value := randomString()
	p.mu.Lock()
	p.sessions[hashOf(value)] = &session{accountID: accountID, csrf: randomString(), expires: p.cfg.Now().Add(SessionTTL)}
	p.mu.Unlock()
	//nolint:gosec // Secure is on unless the panel is reachable only over plain HTTP on a loopback address
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: value, Path: "/", HttpOnly: true, Secure: p.cfg.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(SessionTTL / time.Second)})
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (p *Panel) flash(s *session, isErr bool, msg string) {
	p.mu.Lock()
	s.flash, s.flashErr = msg, isErr
	p.mu.Unlock()
}

// back redirects after a post, carrying a message for the next page.
func (p *Panel) back(w http.ResponseWriter, r *http.Request, s *session, to string, err error, ok string) {
	if err != nil {
		p.flash(s, true, err.Error())
	} else if ok != "" {
		p.flash(s, false, ok)
	}
	http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // every caller passes a path under /admin/ on this server
}

// view is what every page template receives.
type view struct {
	Title    string
	Nav      string
	Admin    string
	CSRF     string
	Flash    string
	FlashErr bool
	Data     any
}

func (p *Panel) render(w http.ResponseWriter, s *session, page, title, nav string, data any) {
	v := view{Title: title, Nav: nav, Data: data}
	if s != nil {
		if a := p.cfg.Store.Snapshot().Accounts[s.accountID]; a != nil {
			v.Admin = a.Email
		}
		p.mu.Lock()
		v.CSRF, v.Flash, v.FlashErr = s.csrf, s.flash, s.flashErr
		s.flash = ""
		p.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := p.tmpl[page].ExecuteTemplate(w, "layout.html", v); err != nil {
		p.log.Error("admin: render", "page", page, "err", err)
	}
}

// actor names the administrator for the audit log.
func (p *Panel) actor(s *session) string {
	if a := p.cfg.Store.Snapshot().Accounts[s.accountID]; a != nil {
		return a.Email
	}
	return s.accountID
}

// ---- sign-in ----

type signInData struct {
	Step     string // email or code
	ReqID    string
	Email    string
	Error    string
	Recovery bool
}

func (p *Panel) newPending() string {
	id := randomString()
	p.mu.Lock()
	now := p.cfg.Now()
	for k, pend := range p.pending {
		if now.After(pend.expires) {
			delete(p.pending, k)
		}
	}
	p.pending[id] = &pendingSignIn{expires: now.Add(15 * time.Minute)}
	p.mu.Unlock()
	return id
}

func (p *Panel) signInPage(w http.ResponseWriter, r *http.Request) {
	if p.sessionOf(r) != nil {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	p.render(w, nil, "signin", "Sign in", "", signInData{Step: "email", ReqID: p.newPending(), Recovery: p.cfg.StaticToken != ""})
}

func (p *Panel) signInPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || !sameOrigin(r) {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id := r.PostForm.Get("req_id")
	p.mu.Lock()
	pend := p.pending[id]
	if pend != nil && p.cfg.Now().After(pend.expires) {
		delete(p.pending, id)
		pend = nil
	}
	p.mu.Unlock()
	if pend == nil {
		http.Redirect(w, r, "/admin/signin", http.StatusSeeOther)
		return
	}
	data := signInData{ReqID: id, Email: pend.email, Recovery: p.cfg.StaticToken != ""}
	refuse := func(step, msg string) {
		time.Sleep(p.cfg.FailureDelay) // slow down guessing
		data.Step, data.Error = step, msg
		w.WriteHeader(http.StatusUnauthorized)
		p.render(w, nil, "signin", "Sign in", "", data)
	}
	switch r.PostForm.Get("step") {
	case "email":
		email := strings.TrimSpace(r.PostForm.Get("email"))
		if len(email) > 254 {
			email = email[:254]
		}
		p.mu.Lock()
		pend.email = email
		p.mu.Unlock()
		// The same page follows for every address, administrator or not.
		p.cfg.SignIn.Request(id, email, signin.ClientAddr(r))
		data.Step, data.Email = "code", email
		p.render(w, nil, "signin", "Sign in", "", data)
	case "code":
		acc, ok := p.cfg.SignIn.Verify(id, r.PostForm.Get("code"))
		if !ok || !acc.Admin {
			// A valid code of a regular account opens nothing here and is
			// answered exactly like a wrong one.
			p.log.Warn("admin: sign-in refused")
			refuse("code", "That code is not valid. Check it, or request a new one.")
			return
		}
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		p.log.Info("admin: signed in", "by", acc.Email, "method", "email code")
		p.startSession(w, r, acc.ID)
	case "token":
		token := r.PostForm.Get("token")
		var admin *store.Account
		for _, a := range p.cfg.Store.Snapshot().Accounts {
			if a.Admin && !a.Disabled {
				admin = a
			}
		}
		if p.cfg.StaticToken == "" || admin == nil || subtle.ConstantTimeCompare([]byte(token), []byte(p.cfg.StaticToken)) != 1 {
			p.log.Warn("admin: recovery sign-in refused")
			refuse("email", "That recovery token is not valid.")
			return
		}
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		p.log.Info("admin: signed in", "by", admin.Email, "method", "recovery token")
		p.startSession(w, r, admin.ID)
	default:
		data.Step = "email"
		p.render(w, nil, "signin", "Sign in", "", data)
	}
}

func (p *Panel) signOut(w http.ResponseWriter, r *http.Request, _ *session) {
	if c, err := r.Cookie(p.cookieName()); err == nil {
		p.mu.Lock()
		delete(p.sessions, hashOf(c.Value))
		p.mu.Unlock()
	}
	//nolint:gosec // same attributes as the cookie it clears
	http.SetCookie(w, &http.Cookie{Name: p.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: p.cfg.Secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.Redirect(w, r, "/admin/signin", http.StatusSeeOther)
}
