package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/config"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

// lastServer is the Server built by the most recent newHTTPServer call.
var lastServer *Server

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

func newHTTPServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	vaultDir := testutil.NewVaultRepo(t)
	cfg := config.Default()
	cfg.Vault.Path = vaultDir
	cfg.Search.IndexDir = filepath.Join(t.TempDir(), "idx")
	cfg.Git.PullInterval = 0
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := kb.Open(context.Background(), cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	srv := New(svc, "test", log)
	lastServer = srv
	ts := httptest.NewServer(srv.Handler(token))
	t.Cleanup(ts.Close)
	return ts, vaultDir
}

func connect(t *testing.T, ts *httptest.Server, token string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-client", Version: "1"}, nil)
	hc := &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}}
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: hc}, nil)
}

func TestHTTPBearerAuth(t *testing.T) {
	ts, vaultDir := newHTTPServer(t, "s3cret")
	// No token: 401, nothing executes.
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("expected 401 with WWW-Authenticate, got %d", resp.StatusCode)
	}
	if _, err := connect(t, ts, "wrong"); err == nil {
		t.Fatal("wrong token must fail")
	}
	// Health is unauthenticated.
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Errorf("healthz: %d %s", resp.StatusCode, body)
	}
	// Right token: full session, several concurrent clients share the service.
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := connect(t, ts, "s3cret")
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = sess.Close() }()
			for j := 0; j < 2; j++ {
				res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "kb_patch_note", Arguments: map[string]any{"path": "Projects/Alpha.md", "operations": []map[string]any{{"op": "append", "content": "via http"}}}})
				if err != nil || res.IsError {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := strings.TrimSpace(testutil.Git(t, vaultDir, "rev-list", "--count", "HEAD")); n != "9" {
		t.Errorf("expected 9 commits, got %s", n)
	}
	if msg := testutil.Git(t, vaultDir, "log", "-1", "--format=%B"); !strings.Contains(msg, "KB-Client: http-client") {
		t.Errorf("client trailer missing:\n%s", msg)
	}
}

func TestHTTPLoopbackWithoutToken(t *testing.T) {
	ts, _ := newHTTPServer(t, "")
	sess, err := connect(t, ts, "")
	if err != nil {
		t.Fatalf("loopback without token must work: %v", err)
	}
	_ = sess.Close()
	if !IsLoopback("127.0.0.1:1") || !IsLoopback("localhost:1") || IsLoopback("0.0.0.0:1") || IsLoopback("[::]:1") || IsLoopback("bad") {
		t.Error("IsLoopback wrong")
	}
	if _, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
}
