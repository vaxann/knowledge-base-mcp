package kb

import (
	"context"
	"path"
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

// checkFilePath validates a vault-relative path for any visible file (no
// .md requirement).
func (s *Service) checkFilePath(clean string) (string, error) {
	if clean == "" {
		return "", E(CodeInvalidPath, "path must name a file")
	}
	if _, _, err := s.v.Check(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// StatFile returns metadata for a file.
func (s *Service) StatFile(ctx context.Context, p string) (*FileInfo, error) {
	rel, err := s.in(p)
	if err != nil {
		return nil, s.wrap(err)
	}
	if err := s.guardRead(ctx, rel); err != nil {
		return nil, err
	}
	clean, err := s.checkFilePath(rel)
	if err != nil {
		return nil, s.wrap(err)
	}
	data, st, err := s.v.Read(clean)
	if err != nil {
		return nil, s.wrap(err)
	}
	return s.fileInfo(clean, data, st.ModTime()), nil
}

func (s *Service) fileInfo(clean string, data []byte, mod time.Time) *FileInfo {
	kind := "attachment"
	if vault.IsMarkdown(clean) {
		kind = "note"
	}
	return &FileInfo{Path: s.out(clean), Kind: kind, MediaType: vault.MediaType(clean), Size: int64(len(data)), Modified: mod, ETag: vault.ETagOf(data)}
}

// ReadFile returns a file's bytes and metadata. maxBytes > 0 limits the size
// that may be returned (larger files fail with too_large).
func (s *Service) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, *FileInfo, error) {
	rel, err := s.in(p)
	if err != nil {
		return nil, nil, s.wrap(err)
	}
	if err := s.guardRead(ctx, rel); err != nil {
		return nil, nil, err
	}
	clean, err := s.checkFilePath(rel)
	if err != nil {
		return nil, nil, s.wrap(err)
	}
	data, st, err := s.v.Read(clean)
	if err != nil {
		return nil, nil, s.wrap(err)
	}
	info := s.fileInfo(clean, data, st.ModTime())
	if maxBytes > 0 && info.Size > maxBytes {
		return nil, info, E(CodeTooLarge, "%s is %d bytes, above the %d byte limit; use kb_file_link to download it", info.Path, info.Size, maxBytes).With("file", info)
	}
	return data, info, nil
}

// PutFile writes bytes to any path inside the vault and commits (one commit).
func (s *Service) PutFile(ctx context.Context, p string, data []byte, overwrite bool, summary string) (*WriteResult, error) {
	rel, err := s.writable(ctx, p)
	if err != nil {
		return nil, err
	}
	if s.cfg.Server.MaxUploadBytes() > 0 && int64(len(data)) > s.cfg.Server.MaxUploadBytes() {
		return nil, E(CodeTooLarge, "upload of %d bytes exceeds the limit of %d bytes", len(data), s.cfg.Server.MaxUploadBytes())
	}
	return s.mutate(ctx, func() (*mutation, error) {
		clean, err := s.checkFilePath(rel)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(clean, "/") || path.Base(clean) == "" {
			return nil, E(CodeInvalidPath, "path must name a file, not a folder")
		}
		if s.v.Exists(clean) && !overwrite {
			return nil, E(CodeAlreadyExists, "%s already exists", s.out(clean))
		}
		if err := s.v.WriteAtomic(clean, data); err != nil {
			return nil, err
		}
		return &mutation{subject: "kb_upload_file: " + clean, summary: summary, paths: []string{clean},
			result: &WriteResult{Path: s.out(clean), ETag: vault.ETagOf(data)}}, nil
	})
}

// LinkTarget validates the target of a signed link for the caller: kind
// "get" needs an existing readable file, kind "put" a writable destination
// (a file, or a folder when p ends with "/"). It returns the target as the
// caller sees it; folder targets keep their trailing slash.
func (s *Service) LinkTarget(ctx context.Context, kind, p string) (string, error) {
	folder := strings.HasSuffix(p, "/")
	rel, err := s.in(p)
	if err != nil {
		return "", s.wrap(err)
	}
	if kind == "get" {
		if err := s.guardRead(ctx, rel); err != nil {
			return "", err
		}
		if _, _, err := s.v.Check(rel); err != nil || !s.v.Exists(rel) {
			return "", s.notFound(rel)
		}
		return s.out(rel), nil
	}
	if rel != "" && s.v.Excluded(rel) {
		return "", s.notFound(rel)
	}
	if err := s.guardWrite(ctx, rel); err != nil {
		return "", err
	}
	if s.ReadOnly() {
		return "", E(CodeReadOnly, "the server runs in read-only mode")
	}
	target := s.out(rel)
	if folder || rel == "" {
		target += "/"
	}
	return target, nil
}
