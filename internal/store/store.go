// Package store keeps the server-side state of a multi-user server in one
// JSON file outside any vault: accounts, access rules, registered
// repositories and OAuth data. Writes are atomic and transactional.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
)

// Version is the schema version written to the state file.
const Version = 1

// Account is a person who may sign in.
type Account struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"` // normalised (lower case)
	Admin      bool      `json:"admin,omitempty"`
	Disabled   bool      `json:"disabled,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSignIn time.Time `json:"last_sign_in,omitzero"`
}

// Credential kinds of a repository.
const (
	CredToken  = "token"   // HTTPS access token, sealed in Secret
	CredSSHKey = "ssh_key" // SSH private key, sealed in Secret
	CredHost   = "host"    // whatever the process environment provides
)

// Repo is a registered Git repository.
type Repo struct {
	ID         string `json:"id"`
	Alias      string `json:"alias"`
	URL        string `json:"url,omitempty"`
	Branch     string `json:"branch"`
	Credential string `json:"credential"`
	// Secret is the sealed token or private key; never the clear value.
	Secret []byte `json:"secret,omitempty"`
	// Hint is the last four characters of a token.
	Hint        string `json:"hint,omitempty"`
	PublicKey   string `json:"public_key,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// KeyPending is set while a generated SSH key waits to be registered
	// with the Git host; nothing is cloned until it is cleared.
	KeyPending bool `json:"key_pending,omitempty"`
	// VaultPath and IndexDir are set only for the vault imported from the
	// classic configuration; other repositories live under the data directory.
	VaultPath string    `json:"vault_path,omitempty"`
	IndexDir  string    `json:"index_dir,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Rule gives an account a level on a folder of a repository.
type Rule struct {
	AccountID string    `json:"account_id"`
	RepoID    string    `json:"repo_id"`
	Path      string    `json:"path"` // repository-relative, "" is the root
	Level     acl.Level `json:"level"`
}

// State is the whole persisted document. Values obtained from Snapshot are
// shared and must be treated as read-only.
type State struct {
	Version  int                 `json:"version"`
	Accounts map[string]*Account `json:"accounts"`
	Repos    map[string]*Repo    `json:"repos"`
	Rules    []Rule              `json:"rules"`
	// OAuth is owned by the authorization server (clients and token hashes).
	OAuth json.RawMessage `json:"oauth,omitempty"`
	// VaultImported records that the configured vault was registered once.
	VaultImported bool `json:"vault_imported,omitempty"`
	// OAuthMigrated records that classic OAuth clients were copied once.
	OAuthMigrated bool `json:"oauth_migrated,omitempty"`
}

func emptyState() *State {
	return &State{Version: Version, Accounts: map[string]*Account{}, Repos: map[string]*Repo{}, Rules: []Rule{}}
}

func (s *State) normalise() {
	if s.Accounts == nil {
		s.Accounts = map[string]*Account{}
	}
	if s.Repos == nil {
		s.Repos = map[string]*Repo{}
	}
	if s.Rules == nil {
		s.Rules = []Rule{}
	}
}

// Store is the state file with an in-memory copy.
type Store struct {
	path string
	mu   sync.Mutex // serialises updates
	cur  *State     // replaced, never mutated, on every update
	raw  []byte     // serialised form of cur, kept as the next backup
	rmu  sync.RWMutex
}

// Open loads the state file, creating an empty state when it does not exist.
// A file that exists but cannot be parsed is an error: starting empty would
// silently drop accounts and rules.
func Open(path string) (*Store, error) {
	s := &Store{path: path, cur: emptyState()}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	st := &State{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("state file %s is damaged (%w); restore it from %s.bak or remove it to start empty", path, err, path)
	}
	if st.Version > Version {
		return nil, fmt.Errorf("state file %s has version %d, newer than this server understands (%d)", path, st.Version, Version)
	}
	st.Version = Version
	st.normalise()
	s.cur, s.raw = st, data
	return s, nil
}

// Path returns the location of the state file.
func (s *Store) Path() string { return s.path }

// Snapshot returns the current state. It is immutable: updates replace it.
func (s *Store) Snapshot() *State {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	return s.cur
}

// Update applies fn to a private copy of the state and persists the result.
// When fn returns an error nothing changes, in memory or on disk.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, err := json.Marshal(s.Snapshot())
	if err != nil {
		return err
	}
	next := &State{}
	if err := json.Unmarshal(prev, next); err != nil {
		return err
	}
	next.normalise()
	if err := fn(next); err != nil {
		return err
	}
	next.Version = Version
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := s.write(data); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	s.rmu.Lock()
	s.cur, s.raw = next, data
	s.rmu.Unlock()
	return nil
}

// write replaces the state file atomically, keeping the previous version as
// a backup. The main file exists at every moment.
func (s *Store) write(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if s.raw != nil {
		if err := writeFileAtomic(s.path+".bak", s.raw); err != nil {
			return err
		}
	}
	return writeFileAtomic(s.path, data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
