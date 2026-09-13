package kb

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
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/vault"
)

// FileInfo describes any vault file (note or attachment).
type FileInfo struct {
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	ETag      string    `json:"etag"`
}

// checkFilePath validates a path for any visible file (no .md requirement).
func (s *Service) checkFilePath(p string) (string, error) {
	clean, err := vault.CleanRel(p)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return "", E(CodeInvalidPath, "path must name a file")
	}
	if _, _, err := s.v.Check(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// StatFile returns metadata for a file.
func (s *Service) StatFile(_ context.Context, p string) (*FileInfo, error) {
	clean, err := s.checkFilePath(p)
	if err != nil {
		return nil, wrap(err)
	}
	data, st, err := s.v.Read(clean)
	if err != nil {
		return nil, wrap(err)
	}
	return s.fileInfo(clean, data, st.ModTime()), nil
}

func (s *Service) fileInfo(clean string, data []byte, mod time.Time) *FileInfo {
	kind := "attachment"
	if vault.IsMarkdown(clean) {
		kind = "note"
	}
	return &FileInfo{Path: clean, Kind: kind, MediaType: vault.MediaType(clean), Size: int64(len(data)), Modified: mod, ETag: vault.ETagOf(data)}
}

// ReadFile returns a file's bytes and metadata. maxBytes > 0 limits the size
// that may be returned (larger files fail with too_large).
func (s *Service) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, *FileInfo, error) {
	clean, err := s.checkFilePath(p)
	if err != nil {
		return nil, nil, wrap(err)
	}
	data, st, err := s.v.Read(clean)
	if err != nil {
		return nil, nil, wrap(err)
	}
	info := s.fileInfo(clean, data, st.ModTime())
	if maxBytes > 0 && info.Size > maxBytes {
		return nil, info, E(CodeTooLarge, "%s is %d bytes, above the %d byte limit; use kb_file_link to download it", clean, info.Size, maxBytes).With("file", info)
	}
	return data, info, nil
}

// PutFile writes bytes to any path inside the vault and commits (one commit).
func (s *Service) PutFile(ctx context.Context, p string, data []byte, overwrite bool, summary string) (*WriteResult, error) {
	if s.cfg.Server.MaxUploadBytes() > 0 && int64(len(data)) > s.cfg.Server.MaxUploadBytes() {
		return nil, E(CodeTooLarge, "upload of %d bytes exceeds the limit of %d bytes", len(data), s.cfg.Server.MaxUploadBytes())
	}
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkFilePath(p)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(clean, "/") || path.Base(clean) == "" {
			return nil, E(CodeInvalidPath, "path must name a file, not a folder")
		}
		if s.v.Exists(clean) && !overwrite {
			return nil, E(CodeAlreadyExists, "%s already exists", clean)
		}
		if err := s.v.WriteAtomic(clean, data); err != nil {
			return nil, err
		}
		return &mutation{subject: "kb_upload_file: " + clean, summary: summary, paths: []string{clean},
			result: &WriteResult{Path: clean, ETag: vault.ETagOf(data)}}, nil
	})
}

// ---- signed links ----

// linkSecret loads or creates the HMAC key used for signed file links. It
// lives next to the index so links survive restarts.
func (s *Service) linkSecret() ([]byte, error) {
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	if s.linkKey != nil {
		return s.linkKey, nil
	}
	p := filepath.Join(s.cfg.Search.IndexDir, "link-secret")
	if data, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(data)) >= 32 {
		s.linkKey = bytes.TrimSpace(data)
		return s.linkKey, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	enc := []byte(hex.EncodeToString(key))
	if err := os.WriteFile(p, enc, 0o600); err != nil {
		return nil, err
	}
	s.linkKey = enc
	return enc, nil
}

func (s *Service) linkMAC(kind, clean string, exp int64) (string, error) {
	key, err := s.linkSecret()
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "%s\n%s\n%d", kind, clean, exp)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// SignedLink builds "<base>/<route>/<path>?exp=..&sig=.." for kind "get"
// (download) or "put" (upload). For uploads the path may be a folder ending
// with "/", in which case the uploaded file name is appended by the server.
func (s *Service) SignedLink(base, kind, p string, ttl time.Duration) (string, time.Time, error) {
	if base == "" {
		return "", time.Time{}, E(CodeInvalidArgument, "signed links need the HTTP transport and a public URL (KB_PUBLIC_URL)")
	}
	folder := strings.HasSuffix(p, "/")
	clean, err := vault.CleanRel(p)
	if err != nil {
		return "", time.Time{}, wrap(err)
	}
	if kind == "get" {
		if _, _, err := s.v.Check(clean); err != nil || !s.v.Exists(clean) {
			return "", time.Time{}, E(CodeNotFound, "%s", clean)
		}
	} else if clean != "" && s.v.Excluded(clean) {
		return "", time.Time{}, E(CodeNotFound, "%s", clean)
	}
	if folder || clean == "" {
		clean += "/"
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	expAt := time.Now().Add(ttl).Truncate(time.Second)
	sig, err := s.linkMAC(kind, clean, expAt.Unix())
	if err != nil {
		return "", time.Time{}, wrap(err)
	}
	route := "/files/"
	if kind == "put" {
		route = "/upload/"
	}
	var b strings.Builder
	for i, seg := range strings.Split(clean, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		b.WriteString(url.PathEscape(seg))
	}
	q := url.Values{"exp": {strconv.FormatInt(expAt.Unix(), 10)}, "sig": {sig}}
	return strings.TrimRight(base, "/") + route + b.String() + "?" + q.Encode(), expAt, nil
}

// VerifyLink checks a signature for kind and path (as signed, folder paths
// keep their trailing slash).
func (s *Service) VerifyLink(kind, clean, expStr, sig string) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	want, err := s.linkMAC(kind, clean, exp)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(want), []byte(sig))
}
