// Package config loads the server configuration from defaults, an optional
// YAML file and KB_* environment variables (highest precedence).
package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vaxann/knowledge-base-mcp/internal/secret"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
)

// Config is the fully resolved server configuration.
type Config struct {
	Vault  Vault  `yaml:"vault"`
	Git    Git    `yaml:"git"`
	Search Search `yaml:"search"`
	Server Server `yaml:"server"`
}

// Vault describes where the notes live.
type Vault struct {
	Path    string   `yaml:"path"`
	Remote  string   `yaml:"remote"`
	Exclude []string `yaml:"exclude"`
	// Alias names the configured vault when a multi-user server imports it
	// as its first repository.
	Alias string `yaml:"alias"`
}

// Git configures commits and synchronisation.
type Git struct {
	Branch        string        `yaml:"branch"`
	AuthorName    string        `yaml:"author_name"`
	AuthorEmail   string        `yaml:"author_email"`
	InstanceID    string        `yaml:"instance_id"`
	AutoPush      bool          `yaml:"auto_push"`
	PushDebounce  time.Duration `yaml:"push_debounce"`
	PullInterval  time.Duration `yaml:"pull_interval"`
	PullFreshness time.Duration `yaml:"pull_freshness"`
	// Token is an HTTPS token handed to Git through a credential helper.
	// It is never written to disk or logged.
	Token    string `yaml:"-"`
	Username string `yaml:"-"`
	// SSHKnownHosts holds known_hosts lines. When set, a multi-user server
	// trusts only these hosts for SSH repositories instead of pinning host
	// keys on first contact.
	SSHKnownHosts string `yaml:"ssh_known_hosts"`
}

// Search configures the full-text index and grep.
type Search struct {
	IndexDir        string   `yaml:"index_dir"`
	Languages       []string `yaml:"languages"`
	GrepMaxFileSize string   `yaml:"grep_max_file_size"`
}

// Server configures the process itself.
type Server struct {
	ReadOnly bool   `yaml:"read_only"`
	LogLevel string `yaml:"log_level"`
	HTTP     HTTP   `yaml:"http"`
	// MaxUpload caps file uploads (kb_upload_file and HTTP uploads).
	MaxUpload string `yaml:"max_upload"`
	// LinkTTL is the default lifetime of signed download/upload links.
	LinkTTL time.Duration `yaml:"link_ttl"`
	// DataDir holds the state file and the repositories added at runtime by
	// a multi-user server. Defaults to the index directory.
	DataDir  string   `yaml:"data_dir"`
	Accounts Accounts `yaml:"accounts"`
}

// Accounts switches the HTTP transport to multi-user mode and configures the
// email sign-in.
type Accounts struct {
	// AdminEmail is the administrator's address. Setting it (together with
	// an HTTP listen address) enables multi-user mode.
	AdminEmail string `yaml:"admin_email"`
	// MailFrom is the sender of sign-in emails, e.g. "KB <login@mail.example.com>".
	MailFrom string `yaml:"mail_from"`
	// ResendAPIKey authenticates to the mail provider. Environment only;
	// never logged.
	ResendAPIKey string `yaml:"-"`
	// SecretKey (base64, 32 bytes) encrypts repository credentials at rest.
	// Environment only; never logged or stored.
	SecretKey string `yaml:"-"`
}

// MultiUser reports whether the server runs with accounts, several
// repositories and access rules. It needs the HTTP transport: stdio always
// serves one local vault.
func (c Config) MultiUser() bool {
	return c.Server.HTTP.Listen != "" && c.Server.Accounts.AdminEmail != ""
}

// DataDirOrDefault returns the data directory.
func (c Config) DataDirOrDefault() string {
	if c.Server.DataDir != "" {
		return c.Server.DataDir
	}
	return c.Search.IndexDir
}

// SecretKeyBytes returns the decoded secret key; Validate has checked it.
func (c Config) SecretKeyBytes() []byte {
	key, _ := secret.ParseKey(c.Server.Accounts.SecretKey)
	return key
}

// MaxUploadBytes returns the parsed upload limit.
func (s Server) MaxUploadBytes() int64 {
	n, _ := ParseSize(s.MaxUpload)
	return n
}

// HTTP configures the optional streamable HTTP transport.
type HTTP struct {
	Listen string `yaml:"listen"` // empty = stdio
	// Token is the bearer token. Prefer KB_HTTP_TOKEN over the file; it is
	// never logged.
	Token   string `yaml:"token"`
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// PublicURL is the externally visible base URL used in OAuth metadata
	// (https://kb.example.com). Derived from request headers when empty.
	PublicURL string `yaml:"public_url"`
	// OAuthPassword is what a person types on the sign-in page when an app
	// (for example the Claude apps) connects through OAuth. Falls back to
	// Token. Never logged.
	OAuthPassword string `yaml:"oauth_password"`
	// OAuthState is the file that keeps registered clients and tokens across
	// restarts. Defaults to <index_dir>/oauth-state.json.
	OAuthState string `yaml:"oauth_state"`
}

// Default returns the built-in defaults.
func Default() Config {
	cache, _ := os.UserCacheDir()
	return Config{
		Vault: Vault{Alias: "main"},
		Git: Git{
			Branch:        "main",
			AuthorName:    "Knowledge Base MCP",
			AuthorEmail:   "kb-mcp@localhost",
			AutoPush:      true,
			PushDebounce:  5 * time.Second,
			PullInterval:  60 * time.Second,
			PullFreshness: 5 * time.Second,
			Username:      "x-access-token",
		},
		Search: Search{
			IndexDir:        filepath.Join(cache, "knowledge-base-mcp", "index"),
			Languages:       []string{"ru", "en"},
			GrepMaxFileSize: "2MB",
		},
		Server: Server{LogLevel: "info", MaxUpload: "50MB", LinkTTL: 15 * time.Minute},
	}
}

// Load builds the configuration: defaults, then the YAML file at path (if
// non-empty or if KB_CONFIG is set), then environment variables.
func Load(path string, env func(string) string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = env("KB_CONFIG")
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, env); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config, env func(string) string) error {
	str := func(key string, dst *string) {
		if v := env(key); v != "" {
			*dst = v
		}
	}
	str("KB_VAULT_PATH", &cfg.Vault.Path)
	str("KB_GIT_REMOTE", &cfg.Vault.Remote)
	str("KB_GIT_BRANCH", &cfg.Git.Branch)
	str("KB_GIT_AUTHOR_NAME", &cfg.Git.AuthorName)
	str("KB_GIT_AUTHOR_EMAIL", &cfg.Git.AuthorEmail)
	str("KB_INSTANCE_ID", &cfg.Git.InstanceID)
	str("KB_GIT_TOKEN", &cfg.Git.Token)
	str("KB_GIT_USERNAME", &cfg.Git.Username)
	str("KB_INDEX_DIR", &cfg.Search.IndexDir)
	str("KB_LOG_LEVEL", &cfg.Server.LogLevel)
	str("KB_HTTP_LISTEN", &cfg.Server.HTTP.Listen)
	str("KB_HTTP_TOKEN", &cfg.Server.HTTP.Token)
	str("KB_HTTP_TLS_CERT", &cfg.Server.HTTP.TLSCert)
	str("KB_HTTP_TLS_KEY", &cfg.Server.HTTP.TLSKey)
	str("KB_PUBLIC_URL", &cfg.Server.HTTP.PublicURL)
	str("KB_OAUTH_PASSWORD", &cfg.Server.HTTP.OAuthPassword)
	str("KB_OAUTH_STATE", &cfg.Server.HTTP.OAuthState)
	str("KB_MAX_UPLOAD", &cfg.Server.MaxUpload)
	str("KB_GREP_MAX_FILE_SIZE", &cfg.Search.GrepMaxFileSize)
	str("KB_VAULT_ALIAS", &cfg.Vault.Alias)
	str("KB_SSH_KNOWN_HOSTS", &cfg.Git.SSHKnownHosts)
	str("KB_DATA_DIR", &cfg.Server.DataDir)
	str("KB_ADMIN_EMAIL", &cfg.Server.Accounts.AdminEmail)
	str("KB_MAIL_FROM", &cfg.Server.Accounts.MailFrom)
	str("KB_RESEND_API_KEY", &cfg.Server.Accounts.ResendAPIKey)
	str("KB_SECRET_KEY", &cfg.Server.Accounts.SecretKey)
	if v := env("KB_EXCLUDE"); v != "" {
		cfg.Vault.Exclude = splitList(v)
	}
	if v := env("KB_SEARCH_LANGUAGES"); v != "" {
		cfg.Search.Languages = splitList(v)
	}
	var err error
	boolEnv := func(key string, dst *bool) {
		if v := env(key); v != "" && err == nil {
			b, perr := strconv.ParseBool(v)
			if perr != nil {
				err = fmt.Errorf("%s: %w", key, perr)
				return
			}
			*dst = b
		}
	}
	durEnv := func(key string, dst *time.Duration) {
		if v := env(key); v != "" && err == nil {
			d, perr := time.ParseDuration(v)
			if perr != nil {
				err = fmt.Errorf("%s: %w", key, perr)
				return
			}
			*dst = d
		}
	}
	boolEnv("KB_READ_ONLY", &cfg.Server.ReadOnly)
	boolEnv("KB_GIT_AUTO_PUSH", &cfg.Git.AutoPush)
	durEnv("KB_PUSH_DEBOUNCE", &cfg.Git.PushDebounce)
	durEnv("KB_PULL_INTERVAL", &cfg.Git.PullInterval)
	durEnv("KB_PULL_FRESHNESS", &cfg.Git.PullFreshness)
	durEnv("KB_LINK_TTL", &cfg.Server.LinkTTL)
	return err
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks the configuration for problems that must stop startup.
func (c Config) Validate() error {
	multi := c.MultiUser()
	// A multi-user server may start with no repository at all.
	if c.Vault.Path == "" && !multi {
		return errors.New("vault path is required: set KB_VAULT_PATH or vault.path in the config file")
	}
	if c.Vault.Path != "" && !filepath.IsAbs(c.Vault.Path) {
		return fmt.Errorf("vault path must be absolute: %q", c.Vault.Path)
	}
	if c.Git.Branch == "" {
		return errors.New("git branch must not be empty")
	}
	if c.Search.IndexDir == "" {
		return errors.New("search index_dir must not be empty (set KB_INDEX_DIR)")
	}
	if _, err := ParseSize(c.Search.GrepMaxFileSize); err != nil {
		return fmt.Errorf("search.grep_max_file_size: %w", err)
	}
	if _, err := ParseSize(c.Server.MaxUpload); err != nil {
		return fmt.Errorf("server.max_upload: %w", err)
	}
	if h := c.Server.HTTP; h.Listen != "" {
		host, _, err := net.SplitHostPort(h.Listen)
		if err != nil {
			return fmt.Errorf("server.http.listen must be host:port, got %q", h.Listen)
		}
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if !loopback && h.Token == "" && !multi {
			return fmt.Errorf("server.http.listen=%s is not loopback: set KB_HTTP_TOKEN (bearer token) before exposing the server", h.Listen)
		}
		if (h.TLSCert == "") != (h.TLSKey == "") {
			return errors.New("server.http.tls_cert and tls_key must be set together")
		}
	}
	if multi {
		if err := c.validateMultiUser(); err != nil {
			return err
		}
	}
	switch strings.ToLower(c.Server.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("server.log_level must be debug, info, warn or error, got %q", c.Server.LogLevel)
	}
	return nil
}

// validateMultiUser checks the settings multi-user mode cannot run without,
// naming the missing one.
func (c Config) validateMultiUser() error {
	a := c.Server.Accounts
	if _, err := store.NormalizeEmail(a.AdminEmail); err != nil {
		return fmt.Errorf("KB_ADMIN_EMAIL must be a bare email address, got %q", a.AdminEmail)
	}
	if a.ResendAPIKey == "" {
		return errors.New("multi-user mode needs a mail API key: set KB_RESEND_API_KEY")
	}
	if a.MailFrom == "" {
		return errors.New("multi-user mode needs a sender address: set KB_MAIL_FROM, e.g. \"KB <login@mail.example.com>\"")
	}
	if _, err := mail.ParseAddress(a.MailFrom); err != nil {
		return fmt.Errorf("KB_MAIL_FROM is not a valid sender address: %q", a.MailFrom)
	}
	if a.SecretKey == "" {
		return errors.New("multi-user mode needs a key to encrypt repository credentials: set KB_SECRET_KEY (openssl rand -base64 32)")
	}
	if _, err := secret.ParseKey(a.SecretKey); err != nil {
		return fmt.Errorf("KB_SECRET_KEY: %w", err)
	}
	if !store.ValidAlias(c.Vault.Alias) {
		return fmt.Errorf("KB_VAULT_ALIAS %q must be 1-32 lowercase letters, digits or hyphens and start with a letter or digit", c.Vault.Alias)
	}
	if dir := c.DataDirOrDefault(); !filepath.IsAbs(dir) {
		return fmt.Errorf("data directory must be absolute: %q (set KB_DATA_DIR)", dir)
	}
	return nil
}

// ParseSize parses "2MB", "512KB", "100" (bytes).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// GrepMaxBytes returns the parsed grep file size limit.
func (c Config) GrepMaxBytes() int64 {
	n, _ := ParseSize(c.Search.GrepMaxFileSize)
	return n
}
