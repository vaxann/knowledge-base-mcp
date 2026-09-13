package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// ---- tools ----

type getFileIn struct {
	Path     string `json:"path" jsonschema:"Vault-relative path of the file (PDF, image, note, ...)"`
	MaxBytes int64  `json:"max_bytes,omitempty" jsonschema:"Refuse to return content larger than this (default 10 MB); metadata is still returned"`
}

type uploadFileIn struct {
	Path          string `json:"path" jsonschema:"Destination path inside the vault, e.g. Documents/_attachments/scan.pdf"`
	ContentBase64 string `json:"content_base64" jsonschema:"File bytes encoded as standard base64"`
	Overwrite     bool   `json:"overwrite,omitempty"`
	Summary       string `json:"summary,omitempty" jsonschema:"Optional commit message paragraph"`
}

type fileLinkIn struct {
	Path       string `json:"path" jsonschema:"File to share"`
	TTLMinutes int    `json:"ttl_minutes,omitempty" jsonschema:"Link lifetime in minutes (default 15)"`
}

type uploadLinkIn struct {
	Path       string `json:"path" jsonschema:"Destination file path, or a folder ending with / to keep the uploaded file's own name (e.g. Inbox/)"`
	TTLMinutes int    `json:"ttl_minutes,omitempty" jsonschema:"Link lifetime in minutes (default 15)"`
}

const defaultInlineLimit = 10 << 20

func (s *Server) registerFileTools() {
	ro := toolOpts{readOnly: true, idempotent: true}
	addToolRaw(s, "kb_get_file", "Return any vault file (PDF, image, Office document, ...) as an embedded binary resource plus metadata. Large files are refused with too_large: use kb_file_link for those.", ro,
		func(ctx context.Context, in getFileIn) (*mcp.CallToolResult, any, error) {
			limit := in.MaxBytes
			if limit <= 0 {
				limit = defaultInlineLimit
			}
			data, info, err := s.svc.ReadFile(ctx, in.Path, limit)
			if err != nil {
				return nil, nil, err
			}
			res := &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("%s (%s, %d bytes)", info.Path, info.MediaType, info.Size)},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "kb://file/" + info.Path, MIMEType: info.MediaType, Blob: data}},
			}}
			return res, info, nil
		})
	addTool(s, "kb_file_link", "Create a temporary signed HTTPS link to download a vault file in a browser, without signing in. Give it to the user to open or save PDFs and other attachments.", ro,
		func(ctx context.Context, in fileLinkIn) (any, error) {
			link, exp, err := s.svc.SignedLink(s.publicURL(), "get", in.Path, time.Duration(in.TTLMinutes)*time.Minute)
			if err != nil {
				return nil, err
			}
			return map[string]any{"url": link, "expires_at": exp, "path": in.Path}, nil
		})
	if s.svc.ReadOnly() {
		return
	}
	addTool(s, "kb_upload_file", "Store a binary or text file in the vault from base64 content (one commit). For files the user has on their device, prefer kb_upload_link so they can upload in the browser.", toolOpts{},
		func(ctx context.Context, in uploadFileIn) (any, error) {
			data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.ContentBase64))
			if err != nil {
				return nil, kb.E(kb.CodeInvalidArgument, "content_base64 is not valid base64: %s", err)
			}
			return s.svc.PutFile(ctx, in.Path, data, in.Overwrite, in.Summary)
		})
	addTool(s, "kb_upload_link", "Create a temporary signed HTTPS upload page so the user can add a file (PDF, photo, document) to the vault from their browser or phone; the upload is committed automatically. Use a folder path ending with / to keep the file's own name.", toolOpts{},
		func(ctx context.Context, in uploadLinkIn) (any, error) {
			link, exp, err := s.svc.SignedLink(s.publicURL(), "put", in.Path, time.Duration(in.TTLMinutes)*time.Minute)
			if err != nil {
				return nil, err
			}
			return map[string]any{"url": link, "expires_at": exp, "path": in.Path}, nil
		})
}

// addToolRaw is addTool for handlers that build their own CallToolResult
// (binary content) on success.
func addToolRaw[In any](s *Server, name, desc string, o toolOpts, h func(ctx context.Context, in In) (*mcp.CallToolResult, any, error)) {
	f := false
	t := &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: o.readOnly, IdempotentHint: o.idempotent, OpenWorldHint: &f}}
	mcp.AddTool(s.mcp, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, out, err := h(clientCtx(ctx, req), in)
		if err != nil {
			r, payload := errorResult(err)
			return r, payload, nil
		}
		return res, out, nil
	})
}

func (s *Server) publicURL() string {
	s.urlMu.RLock()
	defer s.urlMu.RUnlock()
	return s.pubURL
}

// SetPublicURL sets the base used for signed links.
func (s *Server) SetPublicURL(u string) {
	s.urlMu.Lock()
	defer s.urlMu.Unlock()
	s.pubURL = strings.TrimRight(u, "/")
}

// ---- HTTP: /files/<path> and /upload/<path> ----

var uploadTmpl = template.Must(template.New("upload").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Upload · knowledge-base-mcp</title>
<style>
body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
form{background:#1c1c1c;padding:2rem;border-radius:12px;width:min(420px,92vw);box-shadow:0 8px 30px #0008}
h1{font-size:1.1rem;margin:0 0 .5rem}p{color:#aaa;font-size:.9rem;margin:.25rem 0 1rem;word-break:break-all}
input[type=file]{width:100%;color:#ccc}button{margin-top:1rem;width:100%;padding:.7rem;border:0;border-radius:8px;background:#4f7cff;color:#fff;font-size:1rem;cursor:pointer}
.ok{color:#7bd88f}.err{color:#ff6b6b}
</style></head><body>
<form method="post" action="{{.Action}}" enctype="multipart/form-data">
<h1>Upload to the knowledge base</h1>
<p>Destination: <code>{{.Dest}}</code><br>Link valid until {{.Expires}}.</p>
<input type="file" name="file" required{{if .Multiple}} multiple{{end}}>
<button type="submit">Upload</button>
{{if .Message}}<p class="{{.Class}}">{{.Message}}</p>{{end}}
</form></body></html>`))

type uploadView struct {
	Action, Dest, Expires, Message, Class string
	Multiple                              bool
}

// fileRoutes mounts the file endpoints. authed reports whether the request
// carries a valid bearer/OAuth token; signed links work without one.
func (s *Server) fileRoutes(mux *http.ServeMux, authed func(*http.Request) bool) {
	allowed := func(r *http.Request, kind, signedPath string) bool {
		if authed(r) {
			return true
		}
		q := r.URL.Query()
		return s.svc.VerifyLink(kind, signedPath, q.Get("exp"), q.Get("sig"))
	}
	mux.HandleFunc("GET /files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		rel := r.PathValue("path")
		if !allowed(r, "get", rel) {
			http.Error(w, "forbidden: sign in or use a valid signed link", http.StatusForbidden)
			return
		}
		data, info, err := s.svc.ReadFile(r.Context(), rel, 0)
		if err != nil {
			httpErr(w, err)
			return
		}
		w.Header().Set("Content-Type", info.MediaType)
		w.Header().Set("Content-Length", fmt.Sprint(info.Size))
		w.Header().Set("ETag", `"`+info.ETag+`"`)
		w.Header().Set("Cache-Control", "private, no-store")
		disp := "inline"
		if r.URL.Query().Get("download") == "1" {
			disp = "attachment"
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename*=UTF-8''%s`, disp, url.PathEscape(path.Base(info.Path))))
		// Vault files are user content: never let a browser execute them.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		_, _ = w.Write(data) //nolint:gosec // raw file bytes served with nosniff and a sandboxing CSP
	})
	mux.HandleFunc("PUT /files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		rel := r.PathValue("path")
		if !allowed(r, "put", rel) && !allowed(r, "put", path.Dir(rel)+"/") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxUpload+1))
		if err != nil {
			http.Error(w, "upload too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		res, err := s.svc.PutFile(r.Context(), rel, data, r.URL.Query().Get("overwrite") != "0", r.Header.Get("X-KB-Summary"))
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSONResp(w, http.StatusCreated, res)
	})
	mux.HandleFunc("GET /upload/{path...}", func(w http.ResponseWriter, r *http.Request) {
		s.renderUpload(w, r, "", "")
	})
	mux.HandleFunc("POST /upload/{path...}", func(w http.ResponseWriter, r *http.Request) {
		signed := r.PathValue("path")
		if !allowed(r, "put", signed) {
			http.Error(w, "forbidden: this upload link is invalid or expired", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // body is capped by MaxBytesReader above
			s.renderUpload(w, r, "Upload too large or malformed.", "err")
			return
		}
		var stored []string
		for _, fh := range r.MultipartForm.File["file"] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(http.MaxBytesReader(w, f, s.maxUpload+1))
			_ = f.Close()
			if err != nil {
				s.renderUpload(w, r, "A file exceeded the upload limit.", "err")
				return
			}
			dest := signed
			if strings.HasSuffix(signed, "/") {
				dest = signed + path.Base(fh.Filename)
			}
			if _, err := s.svc.PutFile(r.Context(), dest, data, true, "uploaded via signed link"); err != nil {
				s.renderUpload(w, r, "Failed: "+err.Error(), "err")
				return
			}
			stored = append(stored, dest)
		}
		if len(stored) == 0 {
			s.renderUpload(w, r, "No file received.", "err")
			return
		}
		s.renderUpload(w, r, "Stored and committed: "+strings.Join(stored, ", "), "ok")
	})
}

func (s *Server) renderUpload(w http.ResponseWriter, r *http.Request, msg, class string) {
	signed := r.PathValue("path")
	q := r.URL.Query()
	if !s.svc.VerifyLink("put", signed, q.Get("exp"), q.Get("sig")) {
		http.Error(w, "this upload link is invalid or expired", http.StatusForbidden)
		return
	}
	exp := q.Get("exp")
	var expires string
	if n, err := parseUnix(exp); err == nil {
		expires = n.UTC().Format(time.RFC1123)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = uploadTmpl.Execute(w, uploadView{Action: r.URL.RequestURI(), Dest: signed, Expires: expires, Message: msg, Class: class, Multiple: strings.HasSuffix(signed, "/")}) //nolint:gosec // html/template escapes every field
}

func parseUnix(s string) (time.Time, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

func statusOf(err error) int {
	var ke *kb.Error
	if errors.As(err, &ke) {
		switch ke.Code {
		case kb.CodeNotFound, kb.CodeInvalidPath:
			return http.StatusNotFound
		case kb.CodeAlreadyExists, kb.CodeConflict, kb.CodeMergeConflict:
			return http.StatusConflict
		case kb.CodeTooLarge:
			return http.StatusRequestEntityTooLarge
		case kb.CodeReadOnly:
			return http.StatusForbidden
		case kb.CodeInvalidArgument:
			return http.StatusBadRequest
		}
	}
	return http.StatusInternalServerError
}

// httpErr writes a tool error as JSON (never HTML, so no markup is reflected).
func httpErr(w http.ResponseWriter, err error) {
	var ke *kb.Error
	if !errors.As(err, &ke) {
		ke = kb.E(kb.CodeInternal, "%s", err.Error())
	}
	writeJSONResp(w, statusOf(err), map[string]string{"code": ke.Code, "message": ke.Message})
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, _ := jsonMarshal(v)
	_, _ = w.Write(raw) //nolint:gosec // JSON-encoded, served as application/json
}

var _ = vault.MediaType
