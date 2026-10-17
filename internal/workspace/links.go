package workspace

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/kb"
)

// linkSigner signs temporary file links with a key kept on disk next to the
// server's own data, so links survive restarts.
type linkSigner struct {
	path string
	mu   sync.Mutex
	key  []byte
}

func newLinkSigner(path string) *linkSigner { return &linkSigner{path: path} }

func (l *linkSigner) secret() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.key != nil {
		return l.key, nil
	}
	if data, err := os.ReadFile(l.path); err == nil && len(bytes.TrimSpace(data)) >= 32 {
		l.key = bytes.TrimSpace(data)
		return l.key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	enc := []byte(hex.EncodeToString(key))
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(l.path, enc, 0o600); err != nil {
		return nil, err
	}
	l.key = enc
	return enc, nil
}

// mac signs kind, path and expiry and, for a multi-user server, the account
// the link was issued to. An empty account keeps the classic format.
func (l *linkSigner) mac(kind, p string, exp int64, account string) (string, error) {
	key, err := l.secret()
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "%s\n%s\n%d", kind, p, exp)
	if account != "" {
		_, _ = fmt.Fprintf(m, "\n%s", account)
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

// SignedLink builds "<base>/<route>/<path>?exp=..&sig=.." for kind "get"
// (download) or "put" (upload). For uploads the path may be a folder ending
// with "/", in which case the uploaded file name is appended by the server.
// On a multi-user server the link is bound to the calling account, whose
// access is checked again whenever the link is used.
func (w *Workspace) SignedLink(ctx context.Context, base, kind, p string, ttl time.Duration) (string, time.Time, error) {
	if base == "" {
		return "", time.Time{}, kb.E(kb.CodeInvalidArgument, "signed links need the HTTP transport and a public URL (KB_PUBLIC_URL)")
	}
	t, err := w.route(ctx, p)
	if err != nil {
		return "", time.Time{}, err
	}
	target, err := t.svc.LinkTarget(t.ctx, kind, p)
	if err != nil {
		return "", time.Time{}, err
	}
	account := ""
	if w.Multi() {
		c, _ := CallerOf(ctx)
		account = c.AccountID
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	expAt := time.Now().Add(ttl).Truncate(time.Second)
	sig, err := w.links.mac(kind, target, expAt.Unix(), account)
	if err != nil {
		return "", time.Time{}, kb.E(kb.CodeInternal, "%s", err.Error())
	}
	route := "/files/"
	if kind == "put" {
		route = "/upload/"
	}
	var b strings.Builder
	for i, seg := range strings.Split(target, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		b.WriteString(url.PathEscape(seg))
	}
	q := url.Values{"exp": {strconv.FormatInt(expAt.Unix(), 10)}, "sig": {sig}}
	if account != "" {
		q.Set("u", account)
	}
	return strings.TrimRight(base, "/") + route + b.String() + "?" + q.Encode(), expAt, nil
}

// VerifyLink checks the signature of a link for kind and path (as signed:
// folder paths keep their trailing slash). On a multi-user server it also
// checks that the issuing account is still active and still holds the level
// the link needs, and returns a context acting as that account.
func (w *Workspace) VerifyLink(ctx context.Context, kind, signed string, q url.Values) (context.Context, bool) {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ctx, false
	}
	account := ""
	if w.Multi() {
		if account = q.Get("u"); account == "" {
			return ctx, false
		}
	}
	want, err := w.links.mac(kind, signed, exp, account)
	if err != nil || !hmac.Equal([]byte(want), []byte(q.Get("sig"))) {
		return ctx, false
	}
	if !w.Multi() {
		return ctx, true
	}
	c, ok := w.Caller(account)
	if !ok {
		return ctx, false
	}
	ctx = WithCaller(ctx, c)
	t, err := w.route(ctx, signed)
	if err != nil {
		return ctx, false
	}
	if _, err := t.svc.LinkTarget(t.ctx, kind, signed); err != nil {
		return ctx, false
	}
	return ctx, true
}
