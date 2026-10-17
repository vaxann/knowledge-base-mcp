package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vaxann/knowledge-base-mcp/internal/config"
)

// docEnv returns the first dotenv block of a documentation file as a map.
func docEnv(t *testing.T, file string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", file))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```dotenv\n(.*?)```").FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s has no dotenv block", file)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(m[1]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: not a KEY=VALUE line: %q", file, line)
		}
		env[k] = strings.Trim(v, `"`)
	}
	return env
}

// TestDocumentedEnvironmentStartsAServer takes the configuration block of
// docs/multi-user.md as written, fills in only the secret the reader is told
// to generate and the directories of this test, and starts a server with it.
func TestDocumentedEnvironmentStartsAServer(t *testing.T) {
	env := docEnv(t, "docs/multi-user.md")
	for _, key := range []string{"KB_HTTP_LISTEN", "KB_ADMIN_EMAIL", "KB_RESEND_API_KEY", "KB_MAIL_FROM", "KB_SECRET_KEY"} {
		if env[key] == "" {
			t.Fatalf("the documented block must set %s", key)
		}
	}
	if !strings.Contains(env["KB_SECRET_KEY"], "openssl rand -base64 32") {
		t.Fatalf("the block must tell how to generate KB_SECRET_KEY, not carry a key: %q", env["KB_SECRET_KEY"])
	}
	// Without the generated key the server must refuse to start and say why.
	env["KB_DATA_DIR"] = filepath.Join(t.TempDir(), "data")
	env["KB_INDEX_DIR"] = filepath.Join(env["KB_DATA_DIR"], "index")
	lookup := func(k string) string { return env[k] }
	if _, err := config.Load("", lookup); err == nil || !strings.Contains(err.Error(), "KB_SECRET_KEY") {
		t.Fatalf("a placeholder secret key must be rejected by name: %v", err)
	}
	env["KB_SECRET_KEY"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) // what openssl rand -base64 32 prints
	env["KB_HTTP_LISTEN"] = "127.0.0.1:0"
	cfg, err := config.Load("", lookup)
	if err != nil {
		t.Fatalf("the documented configuration must load: %v", err)
	}
	if !cfg.MultiUser() || cfg.Vault.Alias != "main" || cfg.Server.HTTP.PublicURL != "https://kb.example.com" {
		t.Fatalf("configuration: %+v", cfg.Server)
	}
	m, err := NewMulti(context.Background(), Options{Config: cfg, Version: "test", Sender: &outbox{}})
	if err != nil {
		t.Fatalf("the documented configuration must start a server: %v", err)
	}
	defer func() { _ = m.Close() }()
	if a := m.Store.Snapshot().AccountByEmail(env["KB_ADMIN_EMAIL"]); a == nil || !a.Admin {
		t.Error("the administrator from the documented block must exist")
	}
	if m.Handler() == nil {
		t.Error("no handler")
	}
}

// TestReadmeEnvironmentIsValid checks the shorter block in the README.
func TestReadmeEnvironmentIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```dotenv\n(.*?)```").FindAllSubmatch(raw, -1)
	var multi string
	for _, b := range blocks {
		if bytes.Contains(b[1], []byte("KB_ADMIN_EMAIL")) {
			multi = string(b[1])
		}
	}
	if multi == "" {
		t.Fatal("the README must show the multi-user settings")
	}
	for _, key := range []string{"KB_ADMIN_EMAIL=", "KB_RESEND_API_KEY=", "KB_MAIL_FROM=", "KB_SECRET_KEY="} {
		if !strings.Contains(multi, key) {
			t.Errorf("the README block lacks %s", key)
		}
	}
	// No real address, key or host may ever be committed to the docs.
	for _, file := range []string{"README.md", "docs/multi-user.md", "docs/clients.md", "docs/configuration.md", ".env.example", "config.example.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}`).FindAll(data, -1) {
			addr := string(m)
			if !strings.HasSuffix(addr, "example.com") && !strings.HasPrefix(addr, "git@") && addr != "kb-mcp@localhost" {
				t.Errorf("%s contains an address that is not a placeholder: %s", file, addr)
			}
		}
	}
}
