package autopilot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// newIdentityOrchestrator wires an orchestrator whose repo row carries the
// given gh account ("" = none registered).
func newIdentityOrchestrator(t *testing.T, repoDir, projectID, ghUser string) (*Orchestrator, *mockStore, *mockLinearClient, *fakeGhProbe) {
	t.Helper()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			ID:         "issue-identity",
			Identifier: "ABC-9",
			ProjectID:  projectID,
			TeamID:     "team-test",
		},
	}
	ms := &mockStore{
		Repo: &store.Repo{
			ProjectID:   projectID,
			ProjectName: "TestProject",
			RepoDir:     repoDir,
			BaseBranch:  "main",
			Enabled:     true,
			GhUser:      ghUser,
		},
		RepoNoGhUser:   ghUser == "",
		LastUpdatedJob: &store.AutopilotJob{},
	}
	gh := &fakeGhProbe{}
	o := &Orchestrator{
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
	}
	return o, ms, mlc, gh
}

// TestFlowExecute_RefusesWhenRepoHasNoGhAccount: with no account registered
// for the repo, admiral has no GitHub identity to act as. It must refuse
// before any work and tell the user how to register one — falling back to
// whichever account is active on the host is the bug this gate prevents.
func TestFlowExecute_RefusesWhenRepoHasNoGhAccount(t *testing.T) {
	repoDir := t.TempDir()
	o, ms, mlc, gh := newIdentityOrchestrator(t, repoDir, "proj-nogh", "")

	ev := linear.AgentEvent{
		SessionID:       "session-nogh",
		IssueID:         "issue-identity",
		IssueIdentifier: "ABC-9",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	err := f.execute()
	if err == nil {
		t.Fatal("expected execute to fail for a repo with no gh account")
	}
	if !strings.Contains(err.Error(), `PATCH /admin/repos/proj-nogh`) {
		t.Errorf("error must name the API call that fixes it; got: %v", err)
	}
	if !strings.Contains(err.Error(), "gh_user") {
		t.Errorf("error must name the missing setting; got: %v", err)
	}
	// No gh call and no worktree: the refusal happens before any work.
	if got := gh.TotalCalls(); got != 0 {
		t.Errorf("expected no gh calls, got %d", got)
	}
	if entries, err := os.ReadDir(filepath.Join(repoDir, ".worktrees")); err == nil && len(entries) > 0 {
		t.Errorf("expected no worktree to be created; found %v", entries)
	}
	// The refusal must reach the Linear thread, so the user learns how to
	// fix it without digging through logs.
	f.markFailed(err)
	if body := mlc.GetPostedBody(); !strings.Contains(body, "PATCH /admin/repos/proj-nogh") {
		t.Errorf("expected the PATCH hint in the Linear thread; got: %s", body)
	}
	if ms.LastUpdatedJob == nil || ms.LastUpdatedJob.State != store.JobStateFailed {
		t.Errorf("expected the run to be marked FAILED; got %+v", ms.LastUpdatedJob)
	}
}

// TestFlowExecute_GhCallsUseRepoAccount: every gh call of a run authenticates
// as the account the repo registered, and never as an ambient token.
func TestFlowExecute_GhCallsUseRepoAccount(t *testing.T) {
	repoDir := t.TempDir()
	o, _, _, gh := newIdentityOrchestrator(t, repoDir, "proj-acct", "virgil2019")

	ev := linear.AgentEvent{
		SessionID:       "session-acct",
		IssueID:         "issue-identity",
		IssueIdentifier: "ABC-9",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	// The flow stops at the worktree step (no real repo), but the gh
	// pre-flight calls happen before that.
	_ = f.execute()

	envs := gh.Environments()
	if len(envs) == 0 {
		t.Fatal("expected at least one gh call with an environment")
	}
	for i, env := range envs {
		if !containsEnv(env, "GH_TOKEN=tok-virgil2019") {
			t.Errorf("gh call %d must run as the repo's account; env: %v", i, env)
		}
		if hasAmbientToken(env) {
			t.Errorf("gh call %d must not carry an ambient token; env: %v", i, env)
		}
	}
}

// TestRunFix_RefusesWhenRepoHasNoGhAccount: the /fix path resolves the
// identity before it starts; a repo without an account is refused, and the
// task ends FAILED with the error.
func TestRunFix_RefusesWhenRepoHasNoGhAccount(t *testing.T) {
	repoDir := t.TempDir()
	o, ms, mlc, _ := newIdentityOrchestrator(t, repoDir, "proj-nogh", "")
	live := &store.AdmiralTask{
		IssueID:         "issue-identity",
		State:           store.JobStateDone,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/42",
		Branch:          "linear/abc-9",
		ClaudeSessionID: "claude-session",
	}
	ms.AdmiralTask = live
	ms.LiveAdmiralTask = live

	ev := linear.AgentEvent{
		SessionID:       "session-fix-nogh",
		IssueID:         "issue-identity",
		IssueIdentifier: "ABC-9",
		Action:          linear.ActionCreated,
	}
	o.runFix(ev, live)

	if body := mlc.GetPostedBody(); !strings.Contains(body, "PATCH /admin/repos/proj-nogh") {
		t.Errorf("expected the PATCH hint in the Linear thread; got: %s", body)
	}
	if live.State != store.JobStateFailed {
		t.Errorf("expected the task to be FAILED; got %q", live.State)
	}
	if !strings.Contains(live.Error, "gh_user") {
		t.Errorf("expected the error to name the missing setting; got: %q", live.Error)
	}
}

func containsEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// hasAmbientToken reports whether the env carries a token that is not the
// one admiral pins — the host's GH_TOKEN / GITHUB_TOKEN must be gone.
func hasAmbientToken(env []string) bool {
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch key {
		case "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			return true
		}
	}
	return false
}
