// Package mcpserver exposes the kb service as MCP tools and resources.
package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/oauth"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

// Server wraps an MCP server bound to a workspace: one vault for a classic
// server, a set of repositories for a multi-user one.
type Server struct {
	ws        *workspace.Workspace
	mcp       *mcp.Server
	log       *slog.Logger
	urlMu     sync.RWMutex
	pubURL    string
	maxUpload int64
	// tools records every registered tool name, in order.
	tools []string
	// testCaller, when set, is the account every call acts as. In-process
	// tests use it in place of an authenticated HTTP request.
	testCaller *workspace.Caller
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// New registers every tool and resource of a classic single-vault server.
func New(svc *kb.Service, version string, log *slog.Logger) *Server {
	return NewWorkspace(workspace.NewClassic(svc, log), version, log)
}

// NewWorkspace registers every tool and resource over a workspace.
func NewWorkspace(ws *workspace.Workspace, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{ws: ws, log: log, maxUpload: ws.MaxUploadBytes()}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "knowledge-base-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: instructions(ws.ReadOnly(), ws.Multi()),
	})
	s.registerReadTools()
	s.registerSearchTools()
	s.registerGitTools()
	s.registerOpsTools()
	s.registerFileTools()
	if !ws.ReadOnly() {
		s.registerWriteTools()
	}
	s.registerResources()
	return s
}

// ToolNames lists the registered tools.
func (s *Server) ToolNames() []string { return append([]string(nil), s.tools...) }

// multiNote is appended to the description of path-taking tools on a
// multi-user server.
const multiNote = " Paths start with a repository alias: <alias>/<path inside the repository>."

// desc adds the alias note to a tool description when it applies.
func (s *Server) desc(text string) string {
	if s.ws.Multi() {
		return text + multiNote
	}
	return text
}

func instructions(readOnly, multi bool) string {
	b := strings.Builder{}
	if multi {
		b.WriteString("This server exposes Git-backed Markdown knowledge bases, one per repository. Every path starts with a repository alias followed by the path inside that repository, with forward slashes (for example notes/Projects/plan.md); kb_list on the empty folder shows the repositories available to you. You see only the folders you were given access to: anything else answers not_found, and a write to a folder you may only read answers forbidden. Search tools cover all your repositories unless a folder narrows them; history tools (kb_log, kb_diff, kb_ls_tree) work inside one repository named by their folder or path; links between notes resolve inside one repository. ")
	} else {
		b.WriteString("This server exposes a Git-backed Markdown knowledge base. Paths are vault-relative with forward slashes. ")
	}
	b.WriteString("Use kb_search (ranked, stemmed), kb_grep (exact/regex), kb_query (frontmatter predicates) or kb_context (retrieval bundle) to find notes; kb_get_note returns an etag to pass to write tools. ")
	if readOnly {
		b.WriteString("The server is read-only.")
	} else {
		b.WriteString("Every write is one Git commit. If a write fails with code merge_conflict, the clone has a frozen merge: read the conflicts (kb_conflicts), decide the merged content and submit it with kb_resolve_conflict; writes stay blocked until then. ")
		b.WriteString("kb_move_note does not rewrite links: check referencing_notes in its result and update them with kb_grep + kb_patch_note if needed. Deleted notes are recoverable with kb_ls_tree/kb_show_revision/kb_restore.")
	}
	return b.String()
}

// MCP returns the underlying server (for tests and custom transports).
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Run serves on stdio until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// HTTPOptions configure the streamable HTTP transport.
type HTTPOptions struct {
	Listen  string // host:port
	Token   string // bearer token; required unless Listen is loopback
	TLSCert string // optional PEM certificate for direct TLS
	TLSKey  string // optional PEM key for direct TLS
	// OAuth, when non-nil, adds the embedded authorization server so apps
	// that only support OAuth (the Claude apps) can sign in with a password.
	OAuth *oauth.Server
	// PublicURL is the base for signed file links (https://kb.example.com).
	PublicURL string
	// Mount, when set, adds further routes (the admin panel) to the mux.
	Mount func(mux *http.ServeMux)
}

// IsLoopback reports whether a listen address binds only to localhost.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Handler returns the HTTP handler: /mcp (bearer-protected streamable HTTP),
// /healthz (unauthenticated liveness check) and, when o.OAuth is set, the
// OAuth metadata, registration, sign-in and token endpoints.
func (s *Server) Handler(token string) http.Handler {
	return s.HandlerWith(HTTPOptions{Token: token})
}

// HandlerWith builds the handler from full options.
func (s *Server) HandlerWith(o HTTPOptions) http.Handler {
	// A base64 upload inflates a file by a third and travels inside one
	// JSON-RPC request, so the transport body limit must follow KB_MAX_UPLOAD.
	bodyLimit := s.maxUpload/3*4 + (1 << 20)
	if bodyLimit < mcp.DefaultMaxRequestBodyBytes {
		bodyLimit = mcp.DefaultMaxRequestBodyBytes
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Logger: s.log, DisableLocalhostProtection: o.Token != "" || o.OAuth != nil, MaxRequestBodyBytes: bodyLimit})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	var protected http.Handler
	// authed reports whether a request carries a valid token and, on a
	// multi-user server, returns a context acting as its account.
	var authed func(*http.Request) (context.Context, bool)
	if o.OAuth != nil {
		o.OAuth.Mount(mux)
		protected = o.OAuth.Middleware(mcpHandler)
		authed = func(r *http.Request) (context.Context, bool) {
			id, ok := o.OAuth.AccountOf(r)
			if !ok {
				return r.Context(), false
			}
			if !s.ws.Multi() {
				return r.Context(), true
			}
			c, ok := s.ws.Caller(id)
			return workspace.WithCaller(r.Context(), c), ok
		}
	} else {
		protected = bearerAuth(o.Token, mcpHandler)
		authed = func(r *http.Request) (context.Context, bool) {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			return r.Context(), ok && o.Token != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(o.Token)) == 1
		}
	}
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/", protected)
	if o.PublicURL != "" {
		s.SetPublicURL(o.PublicURL)
	}
	s.fileRoutes(mux, authed)
	if o.Mount != nil {
		o.Mount(mux)
	}
	return mux
}

func bearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="knowledge-base-mcp"`)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RunHTTP serves the streamable HTTP transport until ctx is done. A bearer
// token is mandatory unless the listen address is loopback.
func (s *Server) RunHTTP(ctx context.Context, o HTTPOptions) error {
	if o.Token == "" && !s.ws.Multi() && !IsLoopback(o.Listen) {
		return fmt.Errorf("refusing to listen on %s without a token: set KB_HTTP_TOKEN or bind to 127.0.0.1", o.Listen)
	}
	if s.ws.Multi() && o.OAuth == nil {
		return errors.New("a multi-user server cannot run without its sign-in service")
	}
	if o.Token == "" && !s.ws.Multi() {
		s.log.Warn("HTTP transport without a token: only safe because it is bound to loopback", "listen", o.Listen)
	}
	srv := &http.Server{Addr: o.Listen, Handler: s.HandlerWith(o), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		if o.TLSCert != "" || o.TLSKey != "" {
			s.log.Info("serving MCP over HTTPS", "listen", o.Listen, "endpoint", "/mcp")
			errCh <- srv.ListenAndServeTLS(o.TLSCert, o.TLSKey)
			return
		}
		s.log.Info("serving MCP over HTTP", "listen", o.Listen, "endpoint", "/mcp")
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ---- tool plumbing ----

type toolOpts struct {
	readOnly    bool
	destructive bool
	idempotent  bool
}

func clientCtx(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil && p.ClientInfo.Name != "" {
			return kb.WithClient(ctx, p.ClientInfo.Name)
		}
	}
	return ctx
}

// callerCtx attaches the account a call acts as. On a multi-user server it
// comes from the bearer token of the HTTP request, looked up again on every
// call so that disabling an account takes effect at once. A call without an
// account gets no caller and therefore sees nothing.
func (s *Server) callerCtx(ctx context.Context, extra *mcp.RequestExtra) context.Context {
	if !s.ws.Multi() {
		return ctx
	}
	if s.testCaller != nil {
		return workspace.WithCaller(ctx, *s.testCaller)
	}
	if extra == nil || extra.TokenInfo == nil {
		return ctx
	}
	if c, ok := s.ws.Caller(extra.TokenInfo.UserID); ok {
		return workspace.WithCaller(ctx, c)
	}
	return ctx
}

func errorResult(err error) (*mcp.CallToolResult, any) {
	var ke *kb.Error
	if !errors.As(err, &ke) {
		ke = kb.E(kb.CodeInternal, "%s", err.Error())
	}
	payload := map[string]any{"code": ke.Code, "message": ke.Message}
	for k, v := range ke.Details {
		payload[k] = v
	}
	raw, _ := json.Marshal(payload)
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, payload
}

// addTool registers a typed tool whose handler returns a JSON-serialisable
// value or an error carrying a stable code.
func addTool[In any](s *Server, name, desc string, o toolOpts, h func(ctx context.Context, in In) (any, error)) {
	f := false
	t := &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: o.readOnly, IdempotentHint: o.idempotent, OpenWorldHint: &f}}
	if !o.readOnly {
		d := o.destructive
		t.Annotations.DestructiveHint = &d
	}
	s.tools = append(s.tools, name)
	mcp.AddTool(s.mcp, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := h(s.callerCtx(clientCtx(ctx, req), req.Extra), in)
		if err != nil {
			s.log.Debug("tool error", "tool", name, "err", err)
			res, payload := errorResult(err)
			return res, payload, nil
		}
		return nil, out, nil
	})
}

// ---- resources ----

func (s *Server) registerResources() {
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name: "note", URITemplate: "kb://note/{+path}", MIMEType: "text/markdown",
		Description: "Raw Markdown of a note, e.g. kb://note/Projects/Alpha.md",
	}, s.readResource)
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name: "folder", URITemplate: "kb://folder/{+path}", MIMEType: "application/json",
		Description: "JSON listing of a folder, e.g. kb://folder/Projects (kb://folder/ for the root)",
	}, s.readResource)
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name: "file", URITemplate: "kb://file/{+path}",
		Description: "Raw bytes of any vault file (PDF, image, ...) as a blob, e.g. kb://file/Files/scan.pdf",
	}, s.readResource)
	s.mcp.AddResource(&mcp.Resource{Name: "root", URI: "kb://folder/", MIMEType: "application/json", Description: "Listing of the vault root"}, s.readResource)
}

func (s *Server) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	ctx = s.callerCtx(ctx, req.Extra)
	switch {
	case strings.HasPrefix(uri, "kb://note/"):
		path := strings.TrimPrefix(uri, "kb://note/")
		n, err := s.ws.GetNote(ctx, path)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: n.Content}}}, nil
	case strings.HasPrefix(uri, "kb://file/"):
		data, info, err := s.ws.ReadFile(ctx, strings.TrimPrefix(uri, "kb://file/"), 0)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: info.MediaType, Blob: data}}}, nil
	case strings.HasPrefix(uri, "kb://folder/"):
		folder := strings.TrimPrefix(uri, "kb://folder/")
		res, err := s.ws.List(ctx, kb.ListRequest{Folder: folder, Limit: 1000})
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		raw, _ := json.Marshal(res)
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(raw)}}}, nil
	}
	return nil, mcp.ResourceNotFoundError(uri)
}

var _ = vault.MediaType

func requireString(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return kb.E(kb.CodeInvalidArgument, "%s is required", name)
	}
	return nil
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
