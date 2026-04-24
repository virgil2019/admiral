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
}

type Launch struct {
	Mode       string   `yaml:"mode"`
	PtyCommand []string `yaml:"pty_command"`
}

type Session struct {
	TGChatID   int64  `yaml:"tg_chat_id"`
	TeamName   string `yaml:"team_name"`
	CWD        string `yaml:"cwd"`
	OmxBinPath string `yaml:"omx_bin_path"`
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
	if strings.TrimSpace(c.Session.OmxBinPath) == "" {
		return fmt.Errorf("session.omx_bin_path is required")
	}

	c.Session.CWD = expandTilde(c.Session.CWD)
	c.Session.OmxBinPath = expandTilde(c.Session.OmxBinPath)
	c.Storage.SQLitePath = expandTilde(c.Storage.SQLitePath)
	c.Logging.File = expandTilde(c.Logging.File)

	if c.Storage.SQLitePath == "" {
		c.Storage.SQLitePath = expandTilde("~/.local/share/omx-bridge/bridge.db")
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
		if runtime.GOOS == "darwin" {
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

	if _, err := os.Stat(c.Session.OmxBinPath); err != nil {
		return fmt.Errorf("session.omx_bin_path not accessible: %w", err)
	}
	return nil
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
		return filepath.Join(x, "omx-bridge", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "omx-bridge", "config.yaml")
}
