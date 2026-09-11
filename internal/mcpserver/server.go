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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// Server wraps an MCP server bound to one kb.Service.
type Server struct {
	svc *kb.Service
	mcp *mcp.Server
	log *slog.Logger
}

// New registers every tool and resource.
func New(svc *kb.Service, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{svc: svc, log: log}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "knowledge-base-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: instructions(svc.ReadOnly()),
	})
	s.registerReadTools()
	s.registerSearchTools()
	s.registerGitTools()
	s.registerOpsTools()
	if !svc.ReadOnly() {
		s.registerWriteTools()
	}
	s.registerResources()
	return s
}

func instructions(readOnly bool) string {
	b := strings.Builder{}
	b.WriteString("This server exposes a Git-backed Markdown knowledge base. Paths are vault-relative with forward slashes. ")
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

// Handler returns the HTTP handler: /mcp (bearer-protected streamable HTTP)
// and /healthz (unauthenticated liveness check).
func (s *Server) Handler(token string) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Logger: s.log, DisableLocalhostProtection: token != ""})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.Handle("/mcp", bearerAuth(token, mcpHandler))
	mux.Handle("/mcp/", bearerAuth(token, mcpHandler))
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
	if o.Token == "" && !IsLoopback(o.Listen) {
		return fmt.Errorf("refusing to listen on %s without a token: set KB_HTTP_TOKEN or bind to 127.0.0.1", o.Listen)
	}
	if o.Token == "" {
		s.log.Warn("HTTP transport without a token: only safe because it is bound to loopback", "listen", o.Listen)
	}
	srv := &http.Server{Addr: o.Listen, Handler: s.Handler(o.Token), ReadHeaderTimeout: 10 * time.Second}
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
	mcp.AddTool(s.mcp, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := h(clientCtx(ctx, req), in)
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
	s.mcp.AddResource(&mcp.Resource{Name: "root", URI: "kb://folder/", MIMEType: "application/json", Description: "Listing of the vault root"}, s.readResource)
}

func (s *Server) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	switch {
	case strings.HasPrefix(uri, "kb://note/"):
		path := strings.TrimPrefix(uri, "kb://note/")
		n, err := s.svc.GetNote(ctx, path)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: n.Content}}}, nil
	case strings.HasPrefix(uri, "kb://folder/"):
		folder := strings.TrimPrefix(uri, "kb://folder/")
		res, err := s.svc.List(ctx, kb.ListRequest{Folder: folder, Limit: 1000})
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
