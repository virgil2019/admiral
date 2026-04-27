package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	BotToken         string      `yaml:"bot_token"`
	AllowedTGUserIDs []int64     `yaml:"allowed_tg_user_ids"`
	Session          Session     `yaml:"session"`
	Launch           Launch      `yaml:"launch"`
	Storage          Storage     `yaml:"storage"`
	Telegram         Telegram    `yaml:"telegram"`
	EventStream      EventStream `yaml:"event_stream"`
	Logging          Logging     `yaml:"logging"`

	// Warnings collects non-fatal config notices (e.g. deprecated key
	// usage). Populated during Load; main.go logs them after open.
	Warnings []string `yaml:"-"`
}

type Launch struct {
	Mode          string   `yaml:"mode"`
	PtyCommand    []string `yaml:"pty_command"`
	WorkerCount   int      `yaml:"worker_count"`
	AgentType     string   `yaml:"agent_type"`
	BootstrapTask string   `yaml:"bootstrap_task"`
}

type Session struct {
	TGChatID int64  `yaml:"tg_chat_id"`
	TeamName string `yaml:"team_name"`
	CWD      string `yaml:"cwd"`
	// Provider selects the team-cli backend: "omx" or "omc". Defaults to "omx".
	Provider string `yaml:"provider"`
	// CLIBinPath is the absolute path to the team-cli binary (omx or omc).
	CLIBinPath string `yaml:"cli_bin_path"`
	// OmxBinPath is the deprecated alias for CLIBinPath. If set and
	// CLIBinPath is empty, the value is promoted and a deprecation warning
	// is recorded.
	OmxBinPath string `yaml:"omx_bin_path"`
	// LeaderWorker is the worker name TG plain-text gets routed to and
	// shutdown requests target. Defaults are provider-specific:
	// omx → "leader-fixed" (a virtual worker omx routes to its dispatcher);
	// omc → "worker-1" (omc has no leader-agent in its leader pane, so we
	// route to the first concrete worker by convention).
	LeaderWorker string `yaml:"leader_worker"`
}

type Storage struct {
	SQLitePath string `yaml:"sqlite_path"`
}

type Telegram struct {
	LongPollTimeoutS int    `yaml:"long_poll_timeout_s"`
	APIBase          string `yaml:"api_base"`
}

type EventStream struct {
	AwaitTimeoutMs int `yaml:"await_timeout_ms"`
	IdleBackoffMs  int `yaml:"idle_backoff_ms"`
}

type Logging struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validateAndExpand(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validateAndExpand() error {
	if strings.TrimSpace(c.BotToken) == "" {
		return fmt.Errorf("bot_token is required")
	}
	if len(c.AllowedTGUserIDs) == 0 {
		return fmt.Errorf("allowed_tg_user_ids must contain at least one id; refusing to run an open bot")
	}
	if strings.TrimSpace(c.Session.TeamName) == "" {
		return fmt.Errorf("session.team_name is required")
	}
	if strings.TrimSpace(c.Session.CWD) == "" {
		return fmt.Errorf("session.cwd is required")
	}

	switch c.Session.Provider {
	case "":
		c.Session.Provider = "omx"
	case "omx", "omc":
	default:
		return fmt.Errorf("session.provider must be 'omx' or 'omc' (got %q)", c.Session.Provider)
	}

	if strings.TrimSpace(c.Session.LeaderWorker) == "" {
		if c.Session.Provider == "omc" {
			c.Session.LeaderWorker = "worker-1"
		} else {
			c.Session.LeaderWorker = "leader-fixed"
		}
	}

	// Promote deprecated omx_bin_path → cli_bin_path. If both are set, the
	// new key wins and the old is ignored with a warning.
	switch {
	case strings.TrimSpace(c.Session.CLIBinPath) != "" && strings.TrimSpace(c.Session.OmxBinPath) != "":
		c.Warnings = append(c.Warnings,
			"session.omx_bin_path is deprecated and ignored because session.cli_bin_path is set")
	case strings.TrimSpace(c.Session.CLIBinPath) == "" && strings.TrimSpace(c.Session.OmxBinPath) != "":
		c.Session.CLIBinPath = c.Session.OmxBinPath
		c.Warnings = append(c.Warnings,
			"session.omx_bin_path is deprecated; rename to session.cli_bin_path")
	case strings.TrimSpace(c.Session.CLIBinPath) == "":
		return fmt.Errorf("session.cli_bin_path is required")
	}

	c.Session.CWD = expandTilde(c.Session.CWD)
	c.Session.CLIBinPath = expandTilde(c.Session.CLIBinPath)
	c.Storage.SQLitePath = expandTilde(c.Storage.SQLitePath)
	c.Logging.File = expandTilde(c.Logging.File)

	if c.Storage.SQLitePath == "" {
		c.Storage.SQLitePath = expandTilde("~/.local/share/admiral/bridge.db")
	}
	if c.Telegram.LongPollTimeoutS <= 0 {
		c.Telegram.LongPollTimeoutS = 50
	}
	if c.Telegram.APIBase == "" {
		c.Telegram.APIBase = "https://api.telegram.org"
	}
	if c.EventStream.AwaitTimeoutMs <= 0 {
		c.EventStream.AwaitTimeoutMs = 30000
	}
	if c.EventStream.IdleBackoffMs <= 0 {
		c.EventStream.IdleBackoffMs = 2000
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}

	switch c.Launch.Mode {
	case "":
		// omc does not require a TTY (sanity-tested), so default to direct
		// regardless of OS. omx still defaults to pty on darwin where
		// `omx team` refuses to run without a real terminal.
		if c.Session.Provider == "omx" && runtime.GOOS == "darwin" {
			c.Launch.Mode = "pty"
		} else {
			c.Launch.Mode = "direct"
		}
	case "pty", "direct":
	default:
		return fmt.Errorf("launch.mode must be 'pty' or 'direct' (got %q)", c.Launch.Mode)
	}
	if c.Launch.Mode == "pty" && len(c.Launch.PtyCommand) == 0 {
		c.Launch.PtyCommand = []string{"/usr/bin/script", "-q", "/dev/null"}
	}

	if c.Launch.WorkerCount <= 0 {
		return fmt.Errorf("launch.worker_count must be > 0")
	}
	switch c.Launch.AgentType {
	case "codex", "claude", "gemini":
	case "":
		return fmt.Errorf("launch.agent_type is required (codex|claude|gemini)")
	default:
		return fmt.Errorf("launch.agent_type must be one of codex|claude|gemini (got %q)", c.Launch.AgentType)
	}
	if strings.TrimSpace(c.Launch.BootstrapTask) == "" {
		return fmt.Errorf("launch.bootstrap_task is required")
	}

	// session.team_name must match the sanitized form of launch.bootstrap_task
	// so the bridge and omx agree on the team identifier (omx derives it from
	// the task string; we don't auto-derive in v0.1.1 — user declares it).
	expectedTeamName := SanitizeTeamName(c.Launch.BootstrapTask)
	if c.Session.TeamName != expectedTeamName {
		return fmt.Errorf(
			"session.team_name (%q) must equal sanitized launch.bootstrap_task (%q)",
			c.Session.TeamName, expectedTeamName,
		)
	}

	if _, err := os.Stat(c.Session.CLIBinPath); err != nil {
		return fmt.Errorf("session.cli_bin_path not accessible: %w", err)
	}
	return nil
}

// SanitizeTeamName mirrors omx's own sanitizeTeamName: lowercase, collapse
// non-alphanumeric runs to single hyphens, trim leading/trailing hyphens,
// max 30 chars. Must start with [a-z0-9]. Verified against omx's regex
// /^[a-z0-9][a-z0-9-]{0,29}$/.
func SanitizeTeamName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := true // so leading non-alphanum is dropped
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 30 {
		out = strings.TrimRight(out[:30], "-")
	}
	return out
}

func (c *Config) IsAllowed(userID int64) bool {
	for _, id := range c.AllowedTGUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func expandTilde(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func DefaultConfigPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "admiral", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "admiral", "config.yaml")
}
