package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("vault:\n  path: /from/file\ngit:\n  branch: file-branch\n  pull_interval: 30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath, envMap(map[string]string{
		"KB_GIT_BRANCH":    "env-branch",
		"KB_READ_ONLY":     "true",
		"KB_EXCLUDE":       "Private/**, *.log",
		"KB_PUSH_DEBOUNCE": "1s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vault.Path != "/from/file" {
		t.Errorf("file value not applied: %q", cfg.Vault.Path)
	}
	if cfg.Git.Branch != "env-branch" {
		t.Errorf("env must win over file: %q", cfg.Git.Branch)
	}
	if cfg.Git.PullInterval != 30*time.Second {
		t.Errorf("file duration not applied: %v", cfg.Git.PullInterval)
	}
	if cfg.Git.PushDebounce != time.Second || !cfg.Server.ReadOnly {
		t.Errorf("env bool/duration not applied: %+v", cfg)
	}
	if len(cfg.Vault.Exclude) != 2 || cfg.Vault.Exclude[1] != "*.log" {
		t.Errorf("exclude list: %v", cfg.Vault.Exclude)
	}
	if cfg.Git.AuthorName != "Knowledge Base MCP" {
		t.Errorf("default lost: %q", cfg.Git.AuthorName)
	}
}

func TestLoadMissingPath(t *testing.T) {
	_, err := Load("", envMap(nil))
	if err == nil || !strings.Contains(err.Error(), "KB_VAULT_PATH") {
		t.Fatalf("expected actionable error, got %v", err)
	}
}

func TestLoadRelativePathRejected(t *testing.T) {
	_, err := Load("", envMap(map[string]string{"KB_VAULT_PATH": "relative/dir"}))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-path error, got %v", err)
	}
}

func TestLoadBadBool(t *testing.T) {
	_, err := Load("", envMap(map[string]string{"KB_VAULT_PATH": "/v", "KB_READ_ONLY": "maybe"}))
	if err == nil || !strings.Contains(err.Error(), "KB_READ_ONLY") {
		t.Fatalf("expected KB_READ_ONLY error, got %v", err)
	}
}

func TestHTTPValidation(t *testing.T) {
	base := map[string]string{"KB_VAULT_PATH": "/v"}
	cases := []struct {
		listen, token string
		ok            bool
	}{
		{"127.0.0.1:8765", "", true},
		{"localhost:8765", "", true},
		{"0.0.0.0:8765", "", false},
		{"0.0.0.0:8765", "s3cret", true},
		{"[::]:8765", "s3cret", true},
		{"nonsense", "x", false},
	}
	for _, c := range cases {
		env := map[string]string{"KB_HTTP_LISTEN": c.listen, "KB_HTTP_TOKEN": c.token}
		for k, v := range base {
			env[k] = v
		}
		_, err := Load("", envMap(env))
		if (err == nil) != c.ok {
			t.Errorf("listen=%s token=%q: err=%v, want ok=%v", c.listen, c.token, err, c.ok)
		}
	}
}

// testSecretKey is base64 of 32 zero bytes: a placeholder, never a real key.
const testSecretKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func multiEnv(over map[string]string) map[string]string {
	env := map[string]string{
		"KB_HTTP_LISTEN":    "0.0.0.0:8765",
		"KB_INDEX_DIR":      "/data/index",
		"KB_ADMIN_EMAIL":    "admin@example.com",
		"KB_RESEND_API_KEY": "re_placeholder",
		"KB_MAIL_FROM":      "KB <login@mail.example.com>",
		"KB_SECRET_KEY":     testSecretKey,
	}
	for k, v := range over {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return env
}

func TestMultiUserMode(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		multi   bool
		wantErr string // substring; empty means the configuration is valid
	}{
		{"classic stdio", map[string]string{"KB_VAULT_PATH": "/v"}, false, ""},
		{"classic http", map[string]string{"KB_VAULT_PATH": "/v", "KB_HTTP_LISTEN": "127.0.0.1:8765"}, false, ""},
		{"stdio ignores the administrator address", map[string]string{"KB_VAULT_PATH": "/v", "KB_ADMIN_EMAIL": "admin@example.com"}, false, ""},
		{"stdio with an administrator still needs a vault", map[string]string{"KB_ADMIN_EMAIL": "admin@example.com"}, false, "KB_VAULT_PATH"},
		{"complete, no vault, no static token", multiEnv(nil), true, ""},
		{"complete with a vault to import", multiEnv(map[string]string{"KB_VAULT_PATH": "/data/vault"}), true, ""},
		{"missing mail key", multiEnv(map[string]string{"KB_RESEND_API_KEY": ""}), true, "KB_RESEND_API_KEY"},
		{"missing sender", multiEnv(map[string]string{"KB_MAIL_FROM": ""}), true, "KB_MAIL_FROM"},
		{"malformed sender", multiEnv(map[string]string{"KB_MAIL_FROM": "not an address"}), true, "KB_MAIL_FROM"},
		{"missing secret key", multiEnv(map[string]string{"KB_SECRET_KEY": ""}), true, "KB_SECRET_KEY"},
		{"short secret key", multiEnv(map[string]string{"KB_SECRET_KEY": "c2hvcnQ="}), true, "KB_SECRET_KEY"}, //nolint:gosec // base64 of "short", a deliberately invalid placeholder
		{"malformed administrator address", multiEnv(map[string]string{"KB_ADMIN_EMAIL": "Admin <admin@example.com>"}), true, "KB_ADMIN_EMAIL"},
		{"invalid alias", multiEnv(map[string]string{"KB_VAULT_ALIAS": "My Notes"}), true, "KB_VAULT_ALIAS"},
		{"relative vault", multiEnv(map[string]string{"KB_VAULT_PATH": "vault"}), true, "absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load("", envMap(c.env))
			if cfg.MultiUser() != c.multi {
				t.Errorf("MultiUser() = %v, want %v", cfg.MultiUser(), c.multi)
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("err = %v, want one naming %q", err, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "re_placeholder") {
				t.Error("an error message must not contain the mail key")
			}
		})
	}
}

func TestMultiUserDefaults(t *testing.T) {
	cfg, err := Load("", envMap(multiEnv(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vault.Alias != "main" {
		t.Errorf("default alias = %q", cfg.Vault.Alias)
	}
	if cfg.DataDirOrDefault() != "/data/index" {
		t.Errorf("data dir must default to the index dir, got %q", cfg.DataDirOrDefault())
	}
	if len(cfg.SecretKeyBytes()) != 32 {
		t.Errorf("secret key = %d bytes", len(cfg.SecretKeyBytes()))
	}
	cfg, err = Load("", envMap(multiEnv(map[string]string{"KB_DATA_DIR": "/srv/kb", "KB_VAULT_ALIAS": "notes"})))
	if err != nil || cfg.DataDirOrDefault() != "/srv/kb" || cfg.Vault.Alias != "notes" {
		t.Errorf("overrides not applied: %q %q %v", cfg.DataDirOrDefault(), cfg.Vault.Alias, err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"), envMap(nil))
	if err != nil {
		t.Fatalf("config.example.yaml must load: %v", err)
	}
	if cfg.MultiUser() {
		t.Error("the example must describe classic mode until an administrator address is set")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"2MB": 2 << 20, "512KB": 512 << 10, "100": 100, "1gb": 1 << 30}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("lots"); err == nil {
		t.Error("expected error for invalid size")
	}
}
