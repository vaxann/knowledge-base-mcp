package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Vault is a sandboxed view of a directory tree.
type Vault struct {
	Root string
	excl *Excluder
}

// Open validates root and compiles exclusion patterns.
func Open(root string, exclude []string) (*Vault, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	ex, err := NewExcluder(exclude)
	if err != nil {
		return nil, err
	}
	return &Vault{Root: root, excl: ex}, nil
}

// Entry describes a file or folder in a listing.
type Entry struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"` // note | attachment | folder
	Title   string    `json:"title,omitempty"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"modified,omitempty"`
}

// Check validates and resolves a user path; it must not be excluded.
func (v *Vault) Check(rel string) (clean, abs string, err error) {
	clean, err = CleanRel(rel)
	if err != nil {
		return "", "", err
	}
	if v.excl.Excluded(clean) {
		return clean, "", fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	abs, err = Resolve(v.Root, clean)
	if err != nil {
		return clean, "", err
	}
	return clean, abs, nil
}

// Excluded reports whether a canonical path is hidden.
func (v *Vault) Excluded(rel string) bool { return v.excl.Excluded(rel) }

// Read returns the raw bytes of a file.
func (v *Vault) Read(rel string) ([]byte, os.FileInfo, error) {
	_, abs, err := v.Check(rel)
	if err != nil {
		return nil, nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return nil, nil, err
	}
	if st.IsDir() {
		return nil, nil, fmt.Errorf("%w: %s is a folder", ErrNotFound, rel)
	}
	data, err := os.ReadFile(abs)
	return data, st, err
}

// ReadNote reads and parses a Markdown note.
func (v *Vault) ReadNote(rel string) (*Note, error) {
	clean, err := CleanRel(rel)
	if err != nil {
		return nil, err
	}
	if !IsMarkdown(clean) {
		return nil, fmt.Errorf("%w: %s is not a Markdown note", ErrNotFound, clean)
	}
	data, st, err := v.Read(clean)
	if err != nil {
		return nil, err
	}
	n := Parse(clean, data)
	n.ModTime = st.ModTime()
	return n, nil
}

// Exists reports whether a (non-excluded) file exists.
func (v *Vault) Exists(rel string) bool {
	_, abs, err := v.Check(rel)
	if err != nil {
		return false
	}
	st, err := os.Stat(abs)
	return err == nil && !st.IsDir()
}

// WriteAtomic writes data via a temp file and rename, creating parent dirs.
func (v *Vault) WriteAtomic(rel string, data []byte) error {
	_, abs, err := v.Check(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".kb-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, abs)
}

// Remove deletes a file.
func (v *Vault) Remove(rel string) error {
	_, abs, err := v.Check(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return err
	}
	return nil
}

// Rename moves a file inside the vault.
func (v *Vault) Rename(from, to string) error {
	_, absFrom, err := v.Check(from)
	if err != nil {
		return err
	}
	_, absTo, err := v.Check(to)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return err
	}
	return os.Rename(absFrom, absTo)
}

// Walk visits every visible file (not folders) under rel, in path order.
func (v *Vault) Walk(rel string, fn func(rel string, info fs.FileInfo) error) error {
	clean, abs, err := v.Check(rel)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relp, _ := filepath.Rel(v.Root, p)
		relp = filepath.ToSlash(relp)
		if relp == "." {
			relp = ""
		}
		if relp != clean && v.excl.Excluded(relp) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow symlinks
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(relp, info)
	})
}

// List returns direct children (or all descendants when recursive) of folder.
func (v *Vault) List(folder string, recursive bool) ([]Entry, error) {
	clean, abs, err := v.Check(folder)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, folder)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a folder", ErrNotFound, folder)
	}
	var out []Entry
	if recursive {
		err = v.Walk(clean, func(rel string, info fs.FileInfo) error {
			out = append(out, v.entry(rel, info))
			return nil
		})
		return out, err
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	for _, d := range des {
		rel := path.Join(clean, d.Name())
		if v.excl.Excluded(rel) || d.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if d.IsDir() {
			out = append(out, Entry{Path: rel, Kind: "folder"})
			continue
		}
		info, err := d.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, v.entry(rel, info))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (v *Vault) entry(rel string, info fs.FileInfo) Entry {
	e := Entry{Path: rel, Kind: "attachment", Size: info.Size(), ModTime: info.ModTime()}
	if IsMarkdown(rel) {
		e.Kind = "note"
		e.Title = Stem(rel)
		if len(rel) > 0 {
			if data, err := os.ReadFile(filepath.Join(v.Root, filepath.FromSlash(rel))); err == nil {
				e.Title = Parse(rel, data).Title
			}
		}
	}
	return e
}

// MediaType guesses a media type from the extension.
func MediaType(rel string) string {
	switch strings.ToLower(path.Ext(rel)) {
	case ".md":
		return "text/markdown"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	default:
		return "application/octet-stream"
	}
}
