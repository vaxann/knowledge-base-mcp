// Package config loads the server configuration from defaults, an optional
// YAML file and KB_* environment variables (highest precedence).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
}

// Default returns the built-in defaults.
func Default() Config {
	cache, _ := os.UserCacheDir()
	return Config{
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
		Server: Server{LogLevel: "info"},
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
	str("KB_GREP_MAX_FILE_SIZE", &cfg.Search.GrepMaxFileSize)
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
	if c.Vault.Path == "" {
		return errors.New("vault path is required: set KB_VAULT_PATH or vault.path in the config file")
	}
	if !filepath.IsAbs(c.Vault.Path) {
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
	switch strings.ToLower(c.Server.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("server.log_level must be debug, info, warn or error, got %q", c.Server.LogLevel)
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
