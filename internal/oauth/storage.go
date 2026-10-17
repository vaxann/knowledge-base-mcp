package oauth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/vaxann/knowledge-base-mcp/internal/store"
)

// Storage keeps the serialised state of the authorization server (registered
// clients and token hashes) across restarts.
type Storage interface {
	// Load returns the stored state, or nil when nothing was stored yet.
	Load() ([]byte, error)
	Save(data []byte) error
}

// FileStorage keeps the state in a file of its own, as a classic server does.
type FileStorage string

// Load reads the file; a missing file is an empty state.
func (f FileStorage) Load() ([]byte, error) {
	data, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// Save replaces the file atomically.
func (f FileStorage) Save(data []byte) error {
	tmp := string(f) + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, string(f))
}

// StoreStorage keeps the state inside the state file of a multi-user server.
type StoreStorage struct{ Store *store.Store }

// Load returns the OAuth section of the state file.
func (s StoreStorage) Load() ([]byte, error) { return s.Store.Snapshot().OAuth, nil }

// Save replaces the OAuth section of the state file.
func (s StoreStorage) Save(data []byte) error {
	return s.Store.Update(func(st *store.State) error {
		st.OAuth = json.RawMessage(data)
		return nil
	})
}

// ImportClients copies the clients registered with a classic server into the
// state of a multi-user server, once. Tokens are left behind: they belong to
// no account, so every client signs in again, without registering again. The
// classic file is not modified, which keeps a way back to classic mode.
func ImportClients(classicFile string, st *store.Store) error {
	if st.Snapshot().OAuthMigrated {
		return nil
	}
	var old state
	if data, err := os.ReadFile(classicFile); err == nil {
		if err := json.Unmarshal(data, &old); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return st.Update(func(s *store.State) error {
		s.OAuthMigrated = true
		if len(old.Clients) == 0 || len(s.OAuth) > 0 {
			return nil
		}
		fresh := state{Clients: old.Clients, Access: map[string]*tokenRec{}, Refresh: map[string]*tokenRec{}}
		data, err := json.Marshal(fresh)
		if err != nil {
			return err
		}
		s.OAuth = data
		return nil
	})
}
