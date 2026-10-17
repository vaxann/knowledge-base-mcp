package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
)

// Errors returned by the model helpers.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrInvalid  = errors.New("invalid")
)

// NewID returns a random identifier with a prefix.
func NewID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// NormalizeEmail validates a bare email address and returns its canonical
// (trimmed, lower-case) form.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s || len(s) > 254 {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, s)
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || !strings.Contains(s[at+1:], ".") {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, s)
	}
	return strings.ToLower(s), nil
}

var aliasRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidAlias reports whether s can name a repository: 1 to 32 lowercase
// letters, digits and hyphens, starting with a letter or digit.
func ValidAlias(s string) bool { return aliasRE.MatchString(s) }

// AccountByEmail finds an account by address (any case).
func (s *State) AccountByEmail(email string) *Account {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, a := range s.Accounts {
		if a.Email == email {
			return a
		}
	}
	return nil
}

// AccountList returns accounts sorted by address.
func (s *State) AccountList() []*Account {
	out := make([]*Account, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// AddAccount creates an active account for a normalised address.
func (s *State) AddAccount(email string, admin bool, now time.Time) (*Account, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if s.AccountByEmail(email) != nil {
		return nil, fmt.Errorf("%w: an account for %s", ErrExists, email)
	}
	a := &Account{ID: NewID("acc_"), Email: email, Admin: admin, CreatedAt: now}
	s.Accounts[a.ID] = a
	return a, nil
}

// EnsureAdmin makes sure the configured administrator address exists as an
// active administrator account and that it is the only administrator.
func (s *State) EnsureAdmin(email string, now time.Time) (*Account, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	admin := s.AccountByEmail(email)
	if admin == nil {
		if admin, err = s.AddAccount(email, true, now); err != nil {
			return nil, err
		}
	}
	admin.Admin, admin.Disabled = true, false
	for _, a := range s.Accounts {
		if a.ID != admin.ID {
			a.Admin = false
		}
	}
	return admin, nil
}

// DeleteAccount removes an account and its rules.
func (s *State) DeleteAccount(id string) error {
	if _, ok := s.Accounts[id]; !ok {
		return fmt.Errorf("%w: account", ErrNotFound)
	}
	delete(s.Accounts, id)
	kept := s.Rules[:0]
	for _, r := range s.Rules {
		if r.AccountID != id {
			kept = append(kept, r)
		}
	}
	s.Rules = kept
	return nil
}

// RepoByAlias finds a repository by alias.
func (s *State) RepoByAlias(alias string) *Repo {
	for _, r := range s.Repos {
		if r.Alias == alias {
			return r
		}
	}
	return nil
}

// RepoList returns repositories sorted by alias.
func (s *State) RepoList() []*Repo {
	out := make([]*Repo, 0, len(s.Repos))
	for _, r := range s.Repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// AddRepo registers a repository; the alias must be valid and unused.
func (s *State) AddRepo(r *Repo) error {
	if !ValidAlias(r.Alias) {
		return fmt.Errorf("%w: alias %q must be 1-32 lowercase letters, digits or hyphens and start with a letter or digit", ErrInvalid, r.Alias)
	}
	if s.RepoByAlias(r.Alias) != nil {
		return fmt.Errorf("%w: a repository with the alias %q", ErrExists, r.Alias)
	}
	if r.ID == "" {
		r.ID = NewID("repo_")
	}
	s.Repos[r.ID] = r
	return nil
}

// DeleteRepo removes a repository and every rule that refers to it.
func (s *State) DeleteRepo(id string) error {
	if _, ok := s.Repos[id]; !ok {
		return fmt.Errorf("%w: repository", ErrNotFound)
	}
	delete(s.Repos, id)
	kept := s.Rules[:0]
	for _, r := range s.Rules {
		if r.RepoID != id {
			kept = append(kept, r)
		}
	}
	s.Rules = kept
	return nil
}

// RulesFor returns the rules of one account in one repository.
func (s *State) RulesFor(accountID, repoID string) acl.Set {
	var out acl.Set
	for _, r := range s.Rules {
		if r.AccountID == accountID && r.RepoID == repoID {
			out = append(out, acl.Rule{Path: r.Path, Level: r.Level})
		}
	}
	return out
}

// SetRule creates or replaces the rule of an account on a folder.
func (s *State) SetRule(accountID, repoID, path string, level acl.Level) error {
	if !level.Valid() {
		return fmt.Errorf("%w: level %q", ErrInvalid, level)
	}
	if _, ok := s.Accounts[accountID]; !ok {
		return fmt.Errorf("%w: account", ErrNotFound)
	}
	if _, ok := s.Repos[repoID]; !ok {
		return fmt.Errorf("%w: repository", ErrNotFound)
	}
	for i := range s.Rules {
		if s.Rules[i].AccountID == accountID && s.Rules[i].RepoID == repoID && s.Rules[i].Path == path {
			s.Rules[i].Level = level
			return nil
		}
	}
	s.Rules = append(s.Rules, Rule{AccountID: accountID, RepoID: repoID, Path: path, Level: level})
	return nil
}

// ClearRule removes the rule of an account on a folder, if any.
func (s *State) ClearRule(accountID, repoID, path string) bool {
	for i := range s.Rules {
		if s.Rules[i].AccountID == accountID && s.Rules[i].RepoID == repoID && s.Rules[i].Path == path {
			s.Rules = append(s.Rules[:i], s.Rules[i+1:]...)
			return true
		}
	}
	return false
}
