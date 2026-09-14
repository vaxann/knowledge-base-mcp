package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

func TestFileToolsAndResources(t *testing.T) {
	sess, vaultDir := newSession(t, false)
	pdf := []byte("%PDF-1.4\n%% uploaded via tool\n")
	up := mustOK(t, sess, "kb_upload_file", map[string]any{"path": "Files/tool.pdf", "content_base64": base64.StdEncoding.EncodeToString(pdf), "summary": "attach scan"})
	if up["commit"] == nil || up["path"] != "Files/tool.pdf" {
		t.Fatalf("upload: %v", up)
	}
	if msg := strings.TrimSpace(testutil.Git(t, vaultDir, "log", "-1", "--format=%s")); msg != "kb_upload_file: Files/tool.pdf" {
		t.Errorf("commit subject %q", msg)
	}
	mustFail(t, sess, "kb_upload_file", map[string]any{"path": "Files/tool.pdf", "content_base64": "AAAA"}, "already_exists")
	mustFail(t, sess, "kb_upload_file", map[string]any{"path": "Files/x.bin", "content_base64": "not base64!"}, "invalid_argument")
	mustFail(t, sess, "kb_upload_file", map[string]any{"path": ".obsidian/x.bin", "content_base64": "AAAA"}, "not_found")

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "kb_get_file", Arguments: map[string]any{"path": "Files/tool.pdf"}})
	if err != nil || res.IsError {
		t.Fatalf("get_file: %v %+v", err, res)
	}
	var blob []byte
	for _, c := range res.Content {
		if er, ok := c.(*mcp.EmbeddedResource); ok {
			blob = er.Resource.Blob
			if er.Resource.MIMEType != "application/pdf" {
				t.Errorf("mime %q", er.Resource.MIMEType)
			}
		}
	}
	if !bytes.Equal(blob, pdf) {
		t.Errorf("blob mismatch: %q", blob)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"media_type":"application/pdf"`) {
		t.Errorf("metadata: %s", raw)
	}
	mustFail(t, sess, "kb_get_file", map[string]any{"path": "Files/tool.pdf", "max_bytes": 5}, "too_large")

	rr, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "kb://file/Files/tool.pdf"})
	if err != nil || len(rr.Contents) != 1 || !bytes.Equal(rr.Contents[0].Blob, pdf) {
		t.Errorf("file resource: %v %+v", err, rr)
	}
	// Attachments can be moved and deleted with the generic tools; notes cannot become attachments.
	mustOK(t, sess, "kb_move_note", map[string]any{"from": "Files/tool.pdf", "to": "Archive/tool.pdf"})
	mustFail(t, sess, "kb_move_note", map[string]any{"from": "Archive/tool.pdf", "to": "Archive/tool.md"}, "invalid_path")
	mustOK(t, sess, "kb_delete_note", map[string]any{"path": "Archive/tool.pdf"})
	if _, err := os.Stat(filepath.Join(vaultDir, "Archive", "tool.pdf")); err == nil {
		t.Error("attachment not deleted")
	}
	// Links need a public URL: absent in the in-memory session.
	mustFail(t, sess, "kb_file_link", map[string]any{"path": "Files/scan.pdf"}, "invalid_argument")
}

func TestHTTPFileEndpoints(t *testing.T) {
	ts, vaultDir := newHTTPServer(t, "s3cret")
	srv := lastServer
	srv.SetPublicURL(ts.URL)
	ctx := context.Background()

	// Signed download link works without a token; wrong sig / expiry do not.
	link, exp, err := srv.svc.SignedLink(ts.URL, "get", "Files/scan.pdf", time.Minute)
	if err != nil || !strings.HasPrefix(link, ts.URL+"/files/Files/scan.pdf?") || time.Until(exp) < 30*time.Second {
		t.Fatalf("link: %s %v %v", link, exp, err)
	}
	resp, _ := http.Get(link)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/pdf" || !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatalf("signed download: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body[:10])
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "scan.pdf") {
		t.Errorf("disposition %q", resp.Header.Get("Content-Disposition"))
	}
	resp, _ = http.Get(strings.Replace(link, "sig=", "sig=00", 1))
	if resp.StatusCode != 403 {
		t.Errorf("tampered sig: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/files/Files/scan.pdf")
	if resp.StatusCode != 403 {
		t.Errorf("no auth: %d", resp.StatusCode)
	}
	// A link for one file must not open another.
	u, _ := url.Parse(link)
	other := ts.URL + "/files/Projects/Alpha.md?" + u.RawQuery
	if resp, _ = http.Get(other); resp.StatusCode != 403 {
		t.Errorf("link reuse for another path: %d", resp.StatusCode)
	}
	// Bearer token works too.
	req, _ := http.NewRequest("GET", ts.URL+"/files/Projects/Alpha.md", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/markdown" {
		t.Errorf("bearer download: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if _, _, err := srv.svc.SignedLink(ts.URL, "get", "Nope.pdf", time.Minute); err == nil {
		t.Error("link for missing file must fail")
	}

	// PUT with bearer commits.
	req, _ = http.NewRequest("PUT", ts.URL+"/files/Files/put.bin", bytes.NewReader([]byte{1, 2, 3}))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("X-KB-Summary", "raw put")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 201 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	if msg := testutil.Git(t, vaultDir, "log", "-1", "--format=%B"); !strings.HasPrefix(msg, "kb_upload_file: Files/put.bin\n\nraw put") {
		t.Errorf("put commit:\n%s", msg)
	}
	req, _ = http.NewRequest("PUT", ts.URL+"/files/Files/put.bin", bytes.NewReader([]byte{9}))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Errorf("unauthenticated put: %d", resp.StatusCode)
	}

	// Upload page for a folder: form renders, multipart POST stores the file under its own name.
	upLink, _, err := srv.svc.SignedLink(ts.URL, "put", "Inbox/", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = http.Get(upLink)
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(page), `enctype="multipart/form-data"`) || !strings.Contains(string(page), "Inbox/") {
		t.Fatalf("upload page: %d\n%s", resp.StatusCode, page)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "photo.jpg")
	_, _ = fw.Write([]byte("\xff\xd8\xffjpeg"))
	_ = mw.Close()
	resp, _ = http.Post(upLink, mw.FormDataContentType(), &buf)
	page, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(page), "Stored and committed: Inbox/photo.jpg") {
		t.Fatalf("multipart upload: %d\n%s", resp.StatusCode, page)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "Inbox", "photo.jpg")); err != nil {
		t.Error("uploaded file missing")
	}
	if files := testutil.Git(t, vaultDir, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(files) != "Inbox/photo.jpg" {
		t.Errorf("upload commit files: %q", files)
	}
	// The folder link must not allow escaping the folder.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	fw, _ = mw.CreateFormFile("file", "../../escape.txt")
	_, _ = fw.Write([]byte("x"))
	_ = mw.Close()
	resp, _ = http.Post(upLink, mw.FormDataContentType(), &buf)
	page, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(page), "Stored and committed: Inbox/escape.txt") {
		t.Errorf("filename must be reduced to its base name:\n%s", page)
	}
	// Expired link.
	old, _, _ := srv.svc.SignedLink(ts.URL, "get", "Files/scan.pdf", time.Nanosecond)
	time.Sleep(1100 * time.Millisecond)
	if resp, _ = http.Get(old); resp.StatusCode != 403 {
		t.Errorf("expired link: %d", resp.StatusCode)
	}
	// Tool-produced link matches the HTTP route.
	sess := connectOK(t, ts, "s3cret")
	res, _ := sess.CallTool(ctx, &mcp.CallToolParams{Name: "kb_file_link", Arguments: map[string]any{"path": "Files/scan.pdf", "ttl_minutes": 1}})
	raw, _ := json.Marshal(res.StructuredContent)
	var lk map[string]any
	_ = json.Unmarshal(raw, &lk)
	if resp, _ = http.Get(lk["url"].(string)); resp.StatusCode != 200 {
		t.Errorf("tool link: %d %v", resp.StatusCode, lk)
	}
	res, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "kb_upload_link", Arguments: map[string]any{"path": "Inbox/"}})
	raw, _ = json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(raw, &lk)
	if resp, _ = http.Get(lk["url"].(string)); resp.StatusCode != 200 {
		t.Errorf("tool upload link: %d %v", resp.StatusCode, lk)
	}
}

func TestLargeBase64UploadOverHTTP(t *testing.T) {
	ts, vaultDir := newHTTPServer(t, "s3cret")
	sess := connectOK(t, ts, "s3cret")
	data := make([]byte, 6<<20) // above the SDK's 4 MiB default body limit once base64-encoded
	copy(data, "%PDF-1.4\n")
	for i := 16; i < len(data); i++ {
		data[i] = byte(i * 7)
	}
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "kb_upload_file", Arguments: map[string]any{"path": "Files/big.pdf", "content_base64": base64.StdEncoding.EncodeToString(data)}})
	if err != nil || res.IsError {
		t.Fatalf("6 MB base64 upload must succeed: %v %+v", err, res)
	}
	got, err := os.ReadFile(filepath.Join(vaultDir, "Files", "big.pdf"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored bytes differ: %v", err)
	}
	// Above KB_MAX_UPLOAD (50 MB default is too big for a unit test): the tool reports too_large cleanly.
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "kb_get_file", Arguments: map[string]any{"path": "Files/big.pdf", "max_bytes": 1 << 20}})
	if err != nil || !res.IsError {
		t.Fatalf("expected too_large tool error, got %v %+v", err, res)
	}
}

func connectOK(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	sess, err := connect(t, ts, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}
