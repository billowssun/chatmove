package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PathMap maps a project path as it appears on the remote (the machine that
// pushed it) to the same project's path on this machine.
type PathMap struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

type Config struct {
	WebDAVURL     string    `json:"webdav_url"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	RemoteDir     string    `json:"remote_dir"`
	Machine       string    `json:"machine"`
	ClaudeDir     string    `json:"claude_dir"`
	CodexDir      string    `json:"codex_dir"`
	SyncClaude    bool      `json:"sync_claude"`
	SyncCodex     bool      `json:"sync_codex"`
	ActiveMinutes int       `json:"active_minutes"`
	PathMap       []PathMap `json:"path_map"`
	CodexReindex  bool      `json:"codex_reindex"`
	CodexCommand  string    `json:"codex_command"`

	path string
}

func DefaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "chatsync.json"
	}
	return filepath.Join(filepath.Dir(exe), "chatsync.json")
}

func templateConfig() *Config {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	return &Config{
		WebDAVURL:     "https://dav.jianguoyun.com/dav/",
		Username:      "you@example.com",
		Password:      "",
		RemoteDir:     "chatsync",
		Machine:       host,
		ClaudeDir:     filepath.Join(home, ".claude"),
		CodexDir:      filepath.Join(home, ".codex"),
		SyncClaude:    true,
		SyncCodex:     true,
		ActiveMinutes: 10,
		PathMap:       []PathMap{},
		CodexReindex:  true,
	}
}

func WriteTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	b, _ := json.MarshalIndent(templateConfig(), "", "  ")
	return os.WriteFile(path, b, 0600)
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config %s not found, run `chatsync init` first", path)
	}
	if err != nil {
		return nil, err
	}
	cfg := templateConfig()
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if p := os.Getenv("CHATSYNC_PASSWORD"); p != "" {
		cfg.Password = p
	}
	cfg.RemoteDir = strings.Trim(cfg.RemoteDir, "/")
	cfg.path, _ = filepath.Abs(path)
	return cfg, nil
}

func (c *Config) Dir() string  { return filepath.Dir(c.path) }
func (c *Config) Path() string { return c.path }
func (c *Config) StatePath() string {
	return strings.TrimSuffix(c.path, filepath.Ext(c.path)) + ".state.json"
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeProjectName mirrors how Claude Code names project folders under
// ~/.claude/projects: every non-alphanumeric character becomes '-'.
func claudeProjectName(p string) string {
	return nonAlnum.ReplaceAllString(p, "-")
}

// projectMaps returns remote->local and local->remote folder-name maps for
// Claude project directories.
func (c *Config) projectMaps() (toLocal, toRemote map[string]string) {
	toLocal, toRemote = map[string]string{}, map[string]string{}
	for _, m := range c.PathMap {
		r, l := claudeProjectName(m.Remote), claudeProjectName(m.Local)
		toLocal[r] = l
		toRemote[l] = r
	}
	return
}
