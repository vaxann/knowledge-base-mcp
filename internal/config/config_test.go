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
