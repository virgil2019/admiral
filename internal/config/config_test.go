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
  repos:
    - project_id: "proj-test"
      project_name: "TestProject"
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
  repos:
    - project_id: "proj-test"
      project_name: "TestProject"
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
  repos:
    - project_id: "proj-test"
      project_name: "TestProject"
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

func TestLoadAutopilot_DefaultMaxConcurrentRuns(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
  webhook_secret: "wh_secret"
autopilot:
  repos:
    - project_id: "proj-test"
      project_name: "TestProject"
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
	if cfg.Autopilot.MaxConcurrentRuns != 3 {
		t.Errorf("MaxConcurrentRuns default: got %d, want 3", cfg.Autopilot.MaxConcurrentRuns)
	}
}

func TestLoadAutopilot_MaxConcurrentRuns_FromConfig(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
  webhook_secret: "wh_secret"
autopilot:
  repos:
    - project_id: "proj-test"
      project_name: "TestProject"
      repo_dir: "` + t.TempDir() + `"
  claude_bin: "` + bin + `"
  gh_bin: "` + bin + `"
  max_concurrent_runs: 7
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
`
	cfg, err := LoadAutopilot(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadAutopilot: %v", err)
	}
	if cfg.Autopilot.MaxConcurrentRuns != 7 {
		t.Errorf("MaxConcurrentRuns from config: got %d, want 7", cfg.Autopilot.MaxConcurrentRuns)
	}
}

func TestLoadAutopilot_MaxConcurrentRuns_EnvOverride(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
  webhook_secret: "wh_secret"
autopilot:
  repos:
    - project_id: "proj-test"
      project_name: "TestProject"
      repo_dir: "` + t.TempDir() + `"
  claude_bin: "` + bin + `"
  gh_bin: "` + bin + `"
  max_concurrent_runs: 7
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
`
	t.Setenv("ADMIRAL_MAX_CONCURRENT_RUNS", "11")
	cfg, err := LoadAutopilot(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadAutopilot: %v", err)
	}
	if cfg.Autopilot.MaxConcurrentRuns != 11 {
		t.Errorf("MaxConcurrentRuns env override: got %d, want 11 (env should beat config 7)", cfg.Autopilot.MaxConcurrentRuns)
	}
}

func TestLoadDiscoverer_LinearStatesDefaults(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
autopilot:
  claude_bin: "` + bin + `"
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
discoverer:
  require_label: "agent-ready"
`
	cfg, err := LoadDiscoverer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadDiscoverer: %v", err)
	}
	if cfg.Discoverer.LinearStates.InReview != "In Review" {
		t.Errorf("InReview default: got %q, want %q", cfg.Discoverer.LinearStates.InReview, "In Review")
	}
	if cfg.Discoverer.LinearStates.Reviewed != "Reviewed" {
		t.Errorf("Reviewed default: got %q, want %q", cfg.Discoverer.LinearStates.Reviewed, "Reviewed")
	}
}

func TestLoadDiscoverer_LinearStatesOverride(t *testing.T) {
	bin := os.Args[0]
	body := `
linear:
  api_token: "lin_api_test"
autopilot:
  claude_bin: "` + bin + `"
storage:
  sqlite_path: "` + t.TempDir() + `/autopilot.db"
discoverer:
  require_label: "agent-ready"
  linear_states:
    in_review: "Code Review"
    reviewed: "Approved"
`
	cfg, err := LoadDiscoverer(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadDiscoverer: %v", err)
	}
	if cfg.Discoverer.LinearStates.InReview != "Code Review" {
		t.Errorf("InReview override: got %q, want %q", cfg.Discoverer.LinearStates.InReview, "Code Review")
	}
	if cfg.Discoverer.LinearStates.Reviewed != "Approved" {
		t.Errorf("Reviewed override: got %q, want %q", cfg.Discoverer.LinearStates.Reviewed, "Approved")
	}
}

func TestLoadPickupRules_Defaults(t *testing.T) {
	// No discoverer block at all → defaults apply, no drift from the
	// discoverer's own state_types default.
	p := writeConfig(t, "linear:\n  api_token: x\n")
	rules, err := LoadPickupRules(p)
	if err != nil {
		t.Fatalf("LoadPickupRules: %v", err)
	}
	if rules.RequireLabel != "" {
		t.Errorf("RequireLabel: got %q, want empty", rules.RequireLabel)
	}
	if len(rules.StateTypes) != 2 || rules.StateTypes[0] != "backlog" || rules.StateTypes[1] != "unstarted" {
		t.Errorf("StateTypes default: got %v", rules.StateTypes)
	}
}

func TestLoadPickupRules_FromConfig(t *testing.T) {
	p := writeConfig(t, `
discoverer:
  require_label: "agent-ready"
  state_types: ["triage"]
`)
	rules, err := LoadPickupRules(p)
	if err != nil {
		t.Fatalf("LoadPickupRules: %v", err)
	}
	if rules.RequireLabel != "agent-ready" {
		t.Errorf("RequireLabel: got %q", rules.RequireLabel)
	}
	if len(rules.StateTypes) != 1 || rules.StateTypes[0] != "triage" {
		t.Errorf("StateTypes: got %v", rules.StateTypes)
	}
}

func TestLoadPickupRules_MissingFile(t *testing.T) {
	if _, err := LoadPickupRules("/no/such/config.yaml"); err == nil {
		t.Error("expected error for missing config file")
	}
}
