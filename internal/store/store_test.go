package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data", "state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestRoundTrip(t *testing.T) {
	s, p := open(t)
	var accID, repoID string
	err := s.Update(func(st *State) error {
		a, err := st.AddAccount("Ann@Example.com", false, t0)
		if err != nil {
			return err
		}
		accID = a.ID
		r := &Repo{Alias: "notes", URL: "https://example.com/org/notes.git", Branch: "main", Credential: CredToken, Secret: []byte{1, 2, 3}, Hint: "abcd", CreatedAt: t0}
		if err := st.AddRepo(r); err != nil {
			return err
		}
		repoID = r.ID
		st.OAuth = []byte(`{"clients":{}}`)
		return st.SetRule(accID, repoID, "Projects", acl.Read)
	})
	if err != nil {
		t.Fatal(err)
	}
	re, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st := re.Snapshot()
	a := st.AccountByEmail("ANN@example.com")
	if a == nil || a.ID != accID || a.Email != "ann@example.com" || !a.CreatedAt.Equal(t0) {
		t.Fatalf("account not restored: %+v", a)
	}
	r := st.RepoByAlias("notes")
	if r == nil || r.ID != repoID || string(r.Secret) != "\x01\x02\x03" || r.Hint != "abcd" {
		t.Fatalf("repo not restored: %+v", r)
	}
	if got := st.RulesFor(accID, repoID); len(got) != 1 || got[0].Path != "Projects" || got[0].Level != acl.Read {
		t.Fatalf("rules not restored: %+v", got)
	}
	if string(st.OAuth) == "" || st.Version != Version {
		t.Errorf("oauth blob or version lost: %q %d", st.OAuth, st.Version)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestUpdateIsTransactional(t *testing.T) {
	s, p := open(t)
	if err := s.Update(func(st *State) error { _, err := st.AddAccount("a@example.com", false, t0); return err }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	boom := errors.New("boom")
	err := s.Update(func(st *State) error {
		if _, err := st.AddAccount("b@example.com", false, t0); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if s.Snapshot().AccountByEmail("b@example.com") != nil {
		t.Error("a failed update leaked into memory")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("a failed update changed the file")
	}
}

func TestSnapshotIsStable(t *testing.T) {
	s, _ := open(t)
	old := s.Snapshot()
	if err := s.Update(func(st *State) error { _, err := st.AddAccount("a@example.com", false, t0); return err }); err != nil {
		t.Fatal(err)
	}
	if len(old.Accounts) != 0 {
		t.Error("an earlier snapshot must not see later updates")
	}
	if len(s.Snapshot().Accounts) != 1 {
		t.Error("a new snapshot must see the update")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	s, p := open(t)
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Update(func(st *State) error {
				_, err := st.AddAccount(fmt.Sprintf("user%d@example.com", i), false, t0)
				return err
			})
			if err != nil {
				t.Error(err)
			}
			_ = s.Snapshot().AccountList()
		}(i)
	}
	wg.Wait()
	re, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(re.Snapshot().Accounts); got != n {
		t.Errorf("accounts on disk = %d, want %d", got, n)
	}
}

func TestBackupAndInterruptedWrite(t *testing.T) {
	s, p := open(t)
	add := func(email string) {
		t.Helper()
		if err := s.Update(func(st *State) error { _, err := st.AddAccount(email, false, t0); return err }); err != nil {
			t.Fatal(err)
		}
	}
	add("a@example.com")
	if _, err := os.Stat(p + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no backup expected after the first write, got %v", err)
	}
	first, _ := os.ReadFile(p)
	add("b@example.com")
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || string(bak) != string(first) {
		t.Fatalf("backup must hold the previous version: %v", err)
	}
	// A crash in the middle of a write leaves a partial temporary file behind;
	// the state file itself is untouched and loads normally.
	if err := os.WriteFile(p+".tmp", []byte(`{"version":1,"accounts":{"x":`), 0o600); err != nil {
		t.Fatal(err)
	}
	re, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(re.Snapshot().Accounts) != 2 {
		t.Errorf("accounts = %d, want 2", len(re.Snapshot().Accounts))
	}
	if err := re.Update(func(st *State) error { _, err := st.AddAccount("c@example.com", false, t0); return err }); err != nil {
		t.Fatalf("update after a leftover temporary file: %v", err)
	}
}

func TestOpenRejectsDamagedFile(t *testing.T) {
	_, p := open(t)
	for name, content := range map[string]string{
		"truncated": `{"version":1,"accounts":{`,
		"not json":  "garbage",
		"newer":     `{"version":99}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(p); err == nil {
				t.Fatal("Open must fail instead of starting empty")
			} else if !strings.Contains(err.Error(), p) {
				t.Errorf("error should name the file: %v", err)
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"ann@example.com", "ann@example.com", true},
		{"  Ann@Example.COM ", "ann@example.com", true},
		{"not-an-address", "", false},
		{"Ann <ann@example.com>", "", false},
		{"ann@localhost", "", false},
		{"ann@", "", false},
		{"@example.com", "", false},
		{"a@example.com, b@example.com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := NormalizeEmail(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestValidAlias(t *testing.T) {
	cases := map[string]bool{
		"notes": true, "a": true, "work-2": true, "0day": true, strings.Repeat("a", 32): true,
		"": false, "My Notes": false, "Notes": false, "-x": false, "a_b": false, "a/b": false, "a.b": false, strings.Repeat("a", 33): false,
	}
	for in, want := range cases {
		if got := ValidAlias(in); got != want {
			t.Errorf("ValidAlias(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAccountsAndAdmin(t *testing.T) {
	st := emptyState()
	admin, err := st.EnsureAdmin("Root@Example.com", t0)
	if err != nil || !admin.Admin || admin.Disabled || len(st.Accounts) != 1 {
		t.Fatalf("first start: %+v, %v", admin, err)
	}
	if _, err := st.AddAccount("root@example.com", false, t0); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate address (other case) must be rejected: %v", err)
	}
	if _, err := st.AddAccount("nope", false, t0); !errors.Is(err, ErrInvalid) {
		t.Errorf("malformed address must be rejected: %v", err)
	}
	admin.Disabled = true
	again, err := st.EnsureAdmin("root@example.com", t0)
	if err != nil || again.ID != admin.ID || again.Disabled {
		t.Errorf("restart must re-enable the same administrator: %+v, %v", again, err)
	}
	// Changing the configured address moves the role.
	other, err := st.EnsureAdmin("new@example.com", t0)
	if err != nil || !other.Admin || st.Accounts[admin.ID].Admin {
		t.Errorf("administrator role must follow the configured address")
	}
}

func TestRulesAndCascades(t *testing.T) {
	st := emptyState()
	a, _ := st.AddAccount("a@example.com", false, t0)
	b, _ := st.AddAccount("b@example.com", false, t0)
	r1 := &Repo{Alias: "one", Branch: "main"}
	r2 := &Repo{Alias: "two", Branch: "main"}
	for _, r := range []*Repo{r1, r2} {
		if err := st.AddRepo(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddRepo(&Repo{Alias: "one"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate alias: %v", err)
	}
	if err := st.AddRepo(&Repo{Alias: "Bad Alias"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid alias: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.SetRule(a.ID, r1.ID, "", acl.Read))
	must(st.SetRule(a.ID, r1.ID, "", acl.Write)) // replaces
	must(st.SetRule(a.ID, r2.ID, "X", acl.Read))
	must(st.SetRule(b.ID, r1.ID, "Y", acl.None))
	if len(st.Rules) != 3 || st.RulesFor(a.ID, r1.ID).Level("x.md") != acl.Write {
		t.Fatalf("rules = %+v", st.Rules)
	}
	if err := st.SetRule(a.ID, r1.ID, "", "owner"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid level: %v", err)
	}
	if err := st.SetRule("nobody", r1.ID, "", acl.Read); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	if !st.ClearRule(a.ID, r2.ID, "X") || st.ClearRule(a.ID, r2.ID, "X") {
		t.Error("ClearRule must report whether a rule was removed")
	}
	must(st.DeleteRepo(r1.ID))
	if len(st.Rules) != 0 {
		t.Errorf("rules of a removed repository must go: %+v", st.Rules)
	}
	must(st.SetRule(b.ID, r2.ID, "", acl.Read))
	must(st.DeleteAccount(b.ID))
	if len(st.Rules) != 0 || st.AccountByEmail("b@example.com") != nil {
		t.Errorf("rules of a deleted account must go: %+v", st.Rules)
	}
}
