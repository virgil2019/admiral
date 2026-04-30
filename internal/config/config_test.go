package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeTeamName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"vibe bridge session", "vibe-bridge-session"},
		{"  vibe bridge session  ", "vibe-bridge-session"},
		{"UPPER Case Task", "upper-case-task"},
		{"multi---dashes   task", "multi-dashes-task"},
		{"task!@#$%^&*with symbols", "task-with-symbols"},
		{"中文-test-123", "test-123"},
		{"-leading-hyphen", "leading-hyphen"},
		{"trailing-hyphen-", "trailing-hyphen"},
		{"a-very-long-task-description-that-exceeds-thirty-chars", "a-very-long-task-description-t"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SanitizeTeamName(c.in); got != c.want {
			t.Errorf("SanitizeTeamName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// writeConfig dumps yaml to a temp file and returns the path. Bin path
// is auto-set to the test's own executable so os.Stat passes.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Provider_DefaultOmx(t *testing.T) {
	bin := os.Args[0]
	body := `
bot_token: "x:y"
allowed_tg_user_ids: [1]
session:
  tg_chat_id: 1
  team_name: "vibe-bridge-session"
  cwd: "/tmp"
  cli_bin_path: "` + bin + `"
launch:
  worker_count: 1
  agent_type: "codex"
  bootstrap_task: "vibe bridge session"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Provider != "omx" {
		t.Errorf("Provider default: got %q, want omx", cfg.Session.Provider)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestLoad_Provider_Omc(t *testing.T) {
	bin := os.Args[0]
	body := `
bot_token: "x:y"
allowed_tg_user_ids: [1]
session:
  provider: "omc"
  tg_chat_id: 1
  team_name: "vibe-bridge-session"
  cwd: "/tmp"
  cli_bin_path: "` + bin + `"
launch:
  worker_count: 1
  agent_type: "codex"
  bootstrap_task: "vibe bridge session"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Provider != "omc" {
		t.Errorf("Provider: got %q, want omc", cfg.Session.Provider)
	}
	// omc defaults to direct mode regardless of OS.
	if cfg.Launch.Mode != "direct" {
		t.Errorf("omc default launch.mode: got %q, want direct", cfg.Launch.Mode)
	}
}

func TestLoad_OmxBinPath_AliasFallback(t *testing.T) {
	bin := os.Args[0]
	body := `
bot_token: "x:y"
allowed_tg_user_ids: [1]
session:
  tg_chat_id: 1
  team_name: "vibe-bridge-session"
  cwd: "/tmp"
  omx_bin_path: "` + bin + `"
launch:
  worker_count: 1
  agent_type: "codex"
  bootstrap_task: "vibe bridge session"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.CLIBinPath != bin {
		t.Errorf("CLIBinPath promotion: got %q, want %q", cfg.Session.CLIBinPath, bin)
	}
	if len(cfg.Warnings) == 0 {
		t.Errorf("expected deprecation warning, got none")
	}
}

func TestLoad_BothBinPaths_NewWins(t *testing.T) {
	bin := os.Args[0]
	body := `
bot_token: "x:y"
allowed_tg_user_ids: [1]
session:
  tg_chat_id: 1
  team_name: "vibe-bridge-session"
  cwd: "/tmp"
  cli_bin_path: "` + bin + `"
  omx_bin_path: "/nonexistent/path"
launch:
  worker_count: 1
  agent_type: "codex"
  bootstrap_task: "vibe bridge session"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.CLIBinPath != bin {
		t.Errorf("CLIBinPath: got %q, want %q (new key should win)", cfg.Session.CLIBinPath, bin)
	}
	if len(cfg.Warnings) == 0 {
		t.Errorf("expected warning that omx_bin_path is ignored")
	}
}

func TestLoad_Provider_Invalid(t *testing.T) {
	bin := os.Args[0]
	body := `
bot_token: "x:y"
allowed_tg_user_ids: [1]
session:
  provider: "bogus"
  tg_chat_id: 1
  team_name: "vibe-bridge-session"
  cwd: "/tmp"
  cli_bin_path: "` + bin + `"
launch:
  worker_count: 1
  agent_type: "codex"
  bootstrap_task: "vibe bridge session"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected error on invalid provider")
	}
}

func TestLoadAutopilot_DefaultWorkerCount(t *testing.T) {
	// Use os.Args[0] for claude_bin / gh_bin so LookPath validation passes
	// in CI environments without claude/gh installed (matches the pattern
	// other tests in this file use for cli_bin_path).
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
  webhook_secret: "wh_secret"
autopilot:
  repo_dir: "` + t.TempDir() + `"
  claude_bin: "` + bin + `"
  gh_bin: "` + bin + `"
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
`
	cfg, err := LoadAutopilot(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadAutopilot: %v", err)
	}
	if cfg.Autopilot.WorkerCount != 3 {
		t.Errorf("WorkerCount default: got %d, want 3", cfg.Autopilot.WorkerCount)
	}
}

func TestLoadAutopilot_DefaultUpdateIssueStatus(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
  webhook_secret: "wh_secret"
autopilot:
  repo_dir: "` + t.TempDir() + `"
  claude_bin: "` + bin + `"
  gh_bin: "` + bin + `"
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
`
	cfg, err := LoadAutopilot(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadAutopilot: %v", err)
	}
	if cfg.Autopilot.UpdateIssueStatus == nil {
		t.Fatal("UpdateIssueStatus should not be nil")
	}
	if !*cfg.Autopilot.UpdateIssueStatus {
		t.Errorf("UpdateIssueStatus default: got false, want true")
	}
}

func TestLoadAutopilot_UpdateIssueStatus_CanBeDisabled(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
  webhook_secret: "wh_secret"
autopilot:
  repo_dir: "` + t.TempDir() + `"
  claude_bin: "` + bin + `"
  gh_bin: "` + bin + `"
  update_issue_status: false
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
`
	cfg, err := LoadAutopilot(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadAutopilot: %v", err)
	}
	if cfg.Autopilot.UpdateIssueStatus == nil {
		t.Fatal("UpdateIssueStatus should not be nil")
	}
	if *cfg.Autopilot.UpdateIssueStatus {
		t.Errorf("UpdateIssueStatus: got true, want false")
	}
}
