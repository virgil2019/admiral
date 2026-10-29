package autopilot

import (
	"context"
	"errors"
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
// identity before it starts; a repo without an account is refused. The task
// goes back to DONE (not FAILED) so /fix stays retryable once the account is
// registered — the prior run and its claude session are still valid.
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
	auditJob := &store.AutopilotJob{}
	ms.LastUpdatedJob = auditJob

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
	if live.State != store.JobStateDone {
		t.Errorf("expected the task to stay DONE so /fix remains retryable; got %q", live.State)
	}
	if live.ClaudeSessionID != "claude-session" || live.PRURL == "" {
		t.Errorf("the prior run must be left intact; got session %q pr %q",
			live.ClaudeSessionID, live.PRURL)
	}
	// The per-session audit row still records the failure.
	if auditJob.State != store.JobStateFailed {
		t.Errorf("expected the audit job to be FAILED; got %q", auditJob.State)
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

// verifyParent is a parent issue with an Acceptance Criteria heading, so
// runVerify takes the LLM-judge path instead of the auto-pass shortcut.
func verifyParent() *linear.Issue {
	return &linear.Issue{
		Identifier:  "GEO-1",
		Description: "Build login\n\n## Acceptance Criteria\n- user can log in\n",
		TeamID:      "team-1", ProjectID: "proj-1",
	}
}

// TestRunVerify_JudgeRunsAsRepoAccount: the verify judge is a claude process
// that reads the repo (and may call gh), so it runs under the repo's
// registered account — never the host's active one.
func TestRunVerify_JudgeRunsAsRepoAccount(t *testing.T) {
	lc := &mockLinearClient{GetIssueByID: map[string]*linear.Issue{"p1": verifyParent()}}
	db := &mockStore{Repo: &store.Repo{RepoDir: "/repo"}}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, func(context.Context, string, string) (string, error) {
		return "", errors.New("unused")
	})
	var got []string
	o.verifyRunner = func(_ context.Context, _, _ string, env []string) (string, error) {
		got = env
		// Stop the flow here: the identity is what this test asserts on.
		return "", errors.New("stop after capturing env")
	}

	o.runVerify("p1")

	if got == nil {
		t.Fatal("verify judge never ran")
	}
	if !containsEnv(got, "GH_TOKEN=tok-"+defaultTestGhUser) {
		t.Errorf("judge must run as the repo's account; env: %v", got)
	}
	if hasAmbientToken(got) {
		t.Errorf("judge must not carry an ambient token; env: %v", got)
	}
}

// TestRunVerify_RefusesWhenRepoHasNoGhAccount: with no account registered
// there is no identity to judge as, so the judge is not spawned at all and
// the parent is told how to fix it.
func TestRunVerify_RefusesWhenRepoHasNoGhAccount(t *testing.T) {
	lc := &mockLinearClient{GetIssueByID: map[string]*linear.Issue{"p1": verifyParent()}}
	db := &mockStore{Repo: &store.Repo{RepoDir: "/repo"}, RepoNoGhUser: true}
	ran := false
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, func(context.Context, string, string) (string, error) {
		ran = true
		return "", nil
	})

	o.runVerify("p1")

	if ran {
		t.Error("the judge must not run without a gh identity")
	}
	if len(lc.CreatedComments) == 0 {
		t.Fatal("expected an escalation comment naming the missing account")
	}
	if body := lc.CreatedComments[0].Body; !strings.Contains(body, "PATCH /admin/repos/proj-1") {
		t.Errorf("escalation must name the API call that fixes it; got: %s", body)
	}
}

// TestRunResume_PreflightFailureDoesNotClaimTimeout: a /resume refused before
// claude started must not tell the thread the run timed out after
// MaxRunSeconds — that message describes a run that never launched and hides
// the real fix. State stays TIMED_OUT so /resume remains retryable.
func TestRunResume_PreflightFailureDoesNotClaimTimeout(t *testing.T) {
	o, ms, mlc, _ := newIdentityOrchestrator(t, t.TempDir(), "proj-nogh", "")
	live := &store.AdmiralTask{
		IssueID:         "issue-identity",
		State:           store.JobStateTimedOut,
		AttemptN:        1,
		Branch:          "linear/abc-9",
		ClaudeSessionID: "claude-session",
	}
	ms.AdmiralTask = live
	ms.LiveAdmiralTask = live
	ms.LastUpdatedJob = &store.AutopilotJob{}

	ev := linear.AgentEvent{
		SessionID:       "session-resume-nogh",
		IssueID:         "issue-identity",
		IssueIdentifier: "ABC-9",
		Action:          linear.ActionCreated,
	}
	o.runResume(ev, live)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "PATCH /admin/repos/proj-nogh") {
		t.Errorf("expected the PATCH hint in the thread; got: %s", body)
	}
	if strings.Contains(body, "timed out after") {
		t.Errorf("claude never ran, so the thread must not claim a run-length timeout; got: %s", body)
	}
	if live.State != store.JobStateTimedOut {
		t.Errorf("expected the task to stay TIMED_OUT so /resume remains retryable; got %q", live.State)
	}
}
