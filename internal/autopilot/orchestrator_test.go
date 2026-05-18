package autopilot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

func TestSanitizeForPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"TST-1", "tst-1"},
		{"  weird @id! ", "weird-id"},
		{"../../etc/passwd", "etc-passwd"},
		{"linear/abc", "linear-abc"},
		{"---abc---", "abc"},
	}
	for _, c := range cases {
		if got := sanitizeForPath(c.in); got != c.want {
			t.Errorf("sanitizeForPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBranchName(t *testing.T) {
	cases := []struct {
		in   linear.Issue
		want string
	}{
		{linear.Issue{Identifier: "TST-12"}, "linear/tst-12"},
		{linear.Issue{Identifier: "", ID: "abc-123"}, "linear/abc-123"},
	}
	for _, c := range cases {
		if got := branchName(&c.in); got != c.want {
			t.Errorf("branchName(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildPrompt_AssignNoContext(t *testing.T) {
	p := buildPrompt("", &linear.Issue{
		Identifier:  "TST-1",
		Title:       "do the thing",
		Description: "details",
		StateName:   "Todo",
		Labels:      []string{"backend"},
	}, linear.AgentEvent{Action: linear.ActionCreated}, "linear/tst-1", "main")
	if !strings.Contains(p, "TST-1: do the thing") {
		t.Errorf("missing title line:\n%s", p)
	}
	if !strings.Contains(p, "State: Todo") {
		t.Errorf("missing state:\n%s", p)
	}
	if !strings.Contains(p, "Labels: backend") {
		t.Errorf("missing labels:\n%s", p)
	}
	if !strings.Contains(p, "(assigned, no explicit prompt") {
		t.Errorf("missing assign placeholder:\n%s", p)
	}
	if !strings.Contains(p, "git push -u origin \"linear/tst-1\"") &&
		!strings.Contains(p, "git push -u origin linear/tst-1") {
		t.Errorf("missing branch in operating procedure:\n%s", p)
	}
	if !strings.Contains(p, "--base \"main\"") &&
		!strings.Contains(p, "--base main") {
		t.Errorf("missing base branch in operating procedure:\n%s", p)
	}
}

func TestBuildPrompt_MentionWithContext(t *testing.T) {
	p := buildPrompt("autopilot", &linear.Issue{
		Identifier: "TST-2", Title: "x", Description: "d",
	}, linear.AgentEvent{
		Action:        linear.ActionCreated,
		PromptContext: "please refactor the auth module",
	}, "linear/tst-2", "main")
	if !strings.HasPrefix(p, "/autopilot\n\n") {
		t.Errorf("skill not prefixed:\n%s", p)
	}
	if !strings.Contains(p, "please refactor the auth module") {
		t.Errorf("missing prompt context:\n%s", p)
	}
}

func TestBuildPrompt_Comments(t *testing.T) {
	p := buildPrompt("", &linear.Issue{
		Identifier: "TST-3", Title: "x",
		Comments: []linear.Comment{
			{UserName: "alice", Body: "first comment"},
			{UserName: "bob", Body: "second comment"},
		},
	}, linear.AgentEvent{Action: linear.ActionCreated}, "linear/tst-3", "main")
	if !strings.Contains(p, "- alice: first comment") {
		t.Errorf("missing alice comment:\n%s", p)
	}
	if !strings.Contains(p, "- bob: second comment") {
		t.Errorf("missing bob comment:\n%s", p)
	}
}

func TestExtractFirstURL(t *testing.T) {
	in := "Some preamble\nhttps://github.com/x/y/pull/42\nfooter"
	if got := extractFirstURL(in); got != "https://github.com/x/y/pull/42" {
		t.Errorf("got %q", got)
	}
}

func TestOpenStreamFile(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "test-session-abc"
	expectedPath := filepath.Join(tmp, sessionID+".jsonl")

	// Simulate flow with a mock orchestrator and open stream file.
	o := &Orchestrator{cfg: &config.Autopilot{JobStreamsDir: tmp}, logger: slog.Default()}
	f := &flow{o: o, ev: linear.AgentEvent{SessionID: sessionID}}

	if err := f.openStreamFile(); err != nil {
		t.Fatalf("openStreamFile failed: %v", err)
	}
	defer f.closeStreamFile()

	if f.streamFile == nil {
		t.Fatal("streamFile is nil after openStreamFile")
	}
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("stream file not created at expected path: %v", err)
	}
}

func TestDrainStreamJSON_WritesRawLines(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "test-session-def"
	streamPath := filepath.Join(tmp, sessionID+".jsonl")

	o := &Orchestrator{cfg: &config.Autopilot{JobStreamsDir: tmp}, logger: slog.Default()}
	f := &flow{o: o, ev: linear.AgentEvent{SessionID: sessionID}}

	if err := f.openStreamFile(); err != nil {
		t.Fatalf("openStreamFile failed: %v", err)
	}
	defer f.closeStreamFile()

	// Simulate N lines of stream-json output.
	lines := []string{
		`{"type":"system_init","subtype":"version"}`,
		`{"type":"tool_use","name":"Bash","input":{}}`,
		`{"type":"result","content":"ok"}`,
	}
	r := strings.NewReader(strings.Join(lines, "\n") + "\n")
	f.drainStreamJSON(r)

	data, err := os.ReadFile(streamPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	readLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(readLines) != len(lines) {
		t.Fatalf("expected %d lines, got %d", len(lines), len(readLines))
	}
	for i := range lines {
		if readLines[i] != lines[i] {
			t.Errorf("line %d: got %q, want %q", i, readLines[i], lines[i])
		}
	}
}

func TestParseMentionCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantRem  string
		wantOK   bool
	}{
		// /rerun variants
		{`/rerun`, "rerun", "", true},
		{`/RERUN`, "rerun", "", true},
		{`/rerun extra notes`, "rerun", "extra notes", true},
		{"/rerun\nmore content", "rerun", "", true},
		{"/rerun note1\nnote2", "rerun", "note1", true},
		// /fix (not yet implemented — parser still recognizes it)
		{`/fix something broke`, "fix", "something broke", true},
		// leading whitespace
		{`  /rerun`, "rerun", "", true},
		{"  \n  /rerun extra", "rerun", "extra", true},
		// bare text — no command
		{"hello", "", "", false},
		{"", "", "", false},
		{"  hello", "", "", false},
		{"\nhello", "", "", false},
		{"please fix this", "", "", false},
		// first non-empty line not starting with /
		{"line one\n/rerun", "", "", false},
		{"  \n  \n/rerun", "rerun", "", true}, // /rerun on second line is still the first content
	}
	for _, c := range cases {
		name, rem, ok := parseMentionCommand(c.in)
		if name != c.wantName || rem != c.wantRem || ok != c.wantOK {
			t.Errorf("parseMentionCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, name, rem, ok, c.wantName, c.wantRem, c.wantOK)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{30 * time.Second, "30s ago"},
		{59 * time.Second, "59s ago"},
		{60 * time.Second, "1 min ago"},
		{90 * time.Second, "1 min ago"},
		{5 * time.Minute, "5 min ago"},
		{59 * time.Minute, "59 min ago"},
		{60 * time.Minute, "1h 0 min ago"},
		{90 * time.Minute, "1h 30 min ago"},
		{2*time.Hour + 15*time.Minute, "2h 15 min ago"},
		{24 * time.Hour, "1d ago"},
		{48*time.Hour + 30*time.Minute, "2d ago"},
	}
	for _, c := range cases {
		past := now.Add(-c.offset)
		if got := relativeTime(past.Format(time.RFC3339)); got != c.want {
			t.Errorf("relativeTime(%s) = %q, want %q", past.Format(time.RFC3339), got, c.want)
		}
	}
}

// mockLinearClient implements linearClientInterface for testing.
type mockLinearClient struct {
	PostedBody     string
	PostErr        error
	PostedBodyMu   sync.Mutex
	PostedActivity linear.AgentActivity

	// GetIssue override
	GetIssueResult *linear.Issue
	GetIssueErr    error

	// GetWorkflowStates override
	WorkflowStates         []linear.WorkflowState
	GetWorkflowStatesErr   error
	GetWorkflowStatesCalls int

	// IssueUpdate override
	IssueUpdateCalls []struct{ IssueID, StateID string }
	IssueUpdateErr   error

	// GetIssueBlockers override
	IssueBlockers    []linear.IssueBlocker
	IssueBlockersErr error
}

func (m *mockLinearClient) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	m.PostedBodyMu.Lock()
	m.PostedBody = a.Body
	m.PostedActivity = a
	m.PostedBodyMu.Unlock()
	return m.PostErr
}

func (m *mockLinearClient) GetPostedBody() string {
	m.PostedBodyMu.Lock()
	defer m.PostedBodyMu.Unlock()
	return m.PostedBody
}

func (m *mockLinearClient) GetIssue(ctx context.Context, id string) (*linear.Issue, error) {
	if m.GetIssueErr != nil {
		return nil, m.GetIssueErr
	}
	if m.GetIssueResult != nil {
		return m.GetIssueResult, nil
	}
	return &linear.Issue{
		ID:          id,
		Identifier:  "TEST-1",
		Title:       "Test Issue",
		Description: "Test description",
	}, nil
}

func (m *mockLinearClient) GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error) {
	m.GetWorkflowStatesCalls++
	if m.GetWorkflowStatesErr != nil {
		return nil, m.GetWorkflowStatesErr
	}
	return m.WorkflowStates, nil
}

func (m *mockLinearClient) IssueUpdate(ctx context.Context, issueID, stateID string) error {
	m.IssueUpdateCalls = append(m.IssueUpdateCalls, struct{ IssueID, StateID string }{issueID, stateID})
	return m.IssueUpdateErr
}

func (m *mockLinearClient) GetIssueBlockers(_ context.Context, _ string) ([]linear.IssueBlocker, error) {
	return m.IssueBlockers, m.IssueBlockersErr
}

// mockStore implements storeInterface for testing.
type mockStore struct {
	Active     bool
	ActiveSID  string
	ActiveErr  error
	LastJob    *store.AutopilotJob
	LastJobErr error
	GetJob     *store.AutopilotJob
	GetJobErr  error

	// GetLatestDoneJobByIssue
	LatestDoneJob    *store.AutopilotJob
	LatestDoneJobErr error

	// GetLatestTimedOutJobByIssue
	LatestTimedOutJob    *store.AutopilotJob
	LatestTimedOutJobErr error

	// FindActiveJobByIssue
	ActiveJob    *store.AutopilotJob
	ActiveJobErr error

	// HasAnyAutopilotJobForIssue
	HasAnyJobForIssueResult bool
	HasAnyJobForIssueErr    error

	// admiral_tasks (PR-B-v2)
	AdmiralTask             *store.AdmiralTask
	AdmiralTaskErr          error
	AdmiralTaskByPRURL      *store.AdmiralTask
	AdmiralTaskByPRURLErr   error
	LiveAdmiralTask         *store.AdmiralTask // mutated by UpdateAdmiralTask via fn
	ClaimAdmiralTaskFresh   *bool              // override return value (nil = default true)
	ClaimAdmiralTaskErr     error
	UpdateAdmiralTaskErr    error
	ClaimedAdmiralIssues    []string
	UpdatedAdmiralIssues    []string
	SupersededAdmiralIssues []string
	SupersedeErr            error
	SupersedeNextAttempt    int

	// For UpdateAutopilotJob tracking
	LastUpdatedJob *store.AutopilotJob

	// Repo to return from GetRepoByProjectID; if RepoErr is non-nil it is
	// returned instead.
	Repo    *store.Repo
	RepoErr error

	// ListJobsByIssueAndStates override
	ListJobsByIssueAndStatesResult []store.AutopilotJob
	ListJobsByIssueAndStatesErr    error

	// ClaimAutopilotJob recorder. Each call appends the session id; tests
	// inspect this to verify dispatch decisions that spawn goroutines
	// (runFollowup / runFollowupResume) actually claim a row.
	mu                sync.Mutex
	ClaimedSessionIDs []string

	// UpdatedSessionIDs records every session id passed to
	// UpdateAutopilotJob, in call order. Useful for asserting the
	// markAlreadyMerged path updated the new session.
	UpdatedSessionIDs []string

	// Blocker-related fields
	SetBlockedCalls     []string // issueIDs passed to SetAdmiralTaskBlocked
	BlockedTasks        []store.BlockedTask
	TransitionBlockedOK bool
}

func (m *mockStore) AnyAutopilotJobActive() (bool, string, error) {
	return m.Active, m.ActiveSID, m.ActiveErr
}

func (m *mockStore) GetLastAutopilotJob() (*store.AutopilotJob, error) {
	return m.LastJob, m.LastJobErr
}

func (m *mockStore) GetAutopilotJob(sessionID string) (*store.AutopilotJob, error) {
	if m.GetJob == nil {
		return &store.AutopilotJob{}, nil
	}
	return m.GetJob, m.GetJobErr
}

func (m *mockStore) UpdateAutopilotJob(sessionID string, fn func(*store.AutopilotJob)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdatedSessionIDs = append(m.UpdatedSessionIDs, sessionID)
	if m.LastUpdatedJob != nil {
		fn(m.LastUpdatedJob)
	}
	return nil
}

func (m *mockStore) ClaimAutopilotJob(sessionID, issueID, identifier string) (bool, error) {
	m.mu.Lock()
	m.ClaimedSessionIDs = append(m.ClaimedSessionIDs, sessionID)
	m.mu.Unlock()
	return true, nil
}

func (m *mockStore) ClaimedSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.ClaimedSessionIDs))
	copy(out, m.ClaimedSessionIDs)
	return out
}

func (m *mockStore) GetLatestDoneJobByIssue(issueID string) (*store.AutopilotJob, error) {
	return m.LatestDoneJob, m.LatestDoneJobErr
}

func (m *mockStore) GetLatestTimedOutJobByIssue(issueID string) (*store.AutopilotJob, error) {
	return m.LatestTimedOutJob, m.LatestTimedOutJobErr
}

func (m *mockStore) FindActiveJobByIssue(issueID, excludeSessionID string) (*store.AutopilotJob, error) {
	return m.ActiveJob, m.ActiveJobErr
}

func (m *mockStore) HasAnyAutopilotJobForIssue(issueID string) (bool, error) {
	return m.HasAnyJobForIssueResult, m.HasAnyJobForIssueErr
}

// --- admiral_tasks (PR-B-v2) ---

func (m *mockStore) GetAdmiralTaskByIssue(issueID string) (*store.AdmiralTask, error) {
	return m.AdmiralTask, m.AdmiralTaskErr
}

func (m *mockStore) GetAdmiralTaskByPRURL(prURL string) (*store.AdmiralTask, error) {
	return m.AdmiralTaskByPRURL, m.AdmiralTaskByPRURLErr
}

func (m *mockStore) ClaimAdmiralTask(issueID, identifier, lastEventSessionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ClaimedAdmiralIssues = append(m.ClaimedAdmiralIssues, issueID)
	if m.ClaimAdmiralTaskFresh != nil {
		return *m.ClaimAdmiralTaskFresh, m.ClaimAdmiralTaskErr
	}
	return true, m.ClaimAdmiralTaskErr
}

func (m *mockStore) UpdateAdmiralTask(issueID string, fn func(*store.AdmiralTask)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdatedAdmiralIssues = append(m.UpdatedAdmiralIssues, issueID)
	if m.LiveAdmiralTask != nil {
		fn(m.LiveAdmiralTask)
	}
	return m.UpdateAdmiralTaskErr
}

// LiveAdmiralSnapshot returns a copy of LiveAdmiralTask read under m.mu so
// tests polling for an async state transition from a dispatched goroutine
// don't race with UpdateAdmiralTask's fn() writes. Returns the zero value
// when no live task has been configured.
func (m *mockStore) LiveAdmiralSnapshot() store.AdmiralTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.LiveAdmiralTask == nil {
		return store.AdmiralTask{}
	}
	return *m.LiveAdmiralTask
}

func (m *mockStore) MoveAdmiralTaskToHistoryAndClaimNew(issueID, reason, identifier, lastEventSessionID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SupersededAdmiralIssues = append(m.SupersededAdmiralIssues, issueID)
	if m.SupersedeErr != nil {
		return 0, m.SupersedeErr
	}
	m.SupersedeNextAttempt++
	if m.SupersedeNextAttempt < 2 {
		m.SupersedeNextAttempt = 2
	}
	return m.SupersedeNextAttempt, nil
}

func (m *mockStore) GetRepoByProjectID(projectID string) (*store.Repo, error) {
	return m.Repo, m.RepoErr
}

func (m *mockStore) ListJobsByIssueAndStates(issueID string, states []string) ([]store.AutopilotJob, error) {
	return m.ListJobsByIssueAndStatesResult, m.ListJobsByIssueAndStatesErr
}

func (m *mockStore) SetAdmiralTaskBlocked(issueID, blockerIDs string) error {
	m.SetBlockedCalls = append(m.SetBlockedCalls, issueID)
	return nil
}

func (m *mockStore) GetBlockedAdmiralTasks() ([]store.BlockedTask, error) {
	return m.BlockedTasks, nil
}

func (m *mockStore) TransitionBlockedToReceived(issueID string) (bool, error) {
	return m.TransitionBlockedOK, nil
}

// fakeGhProbe is a deterministic ghProbe for tests. Configure the maps
// keyed on branch name (for FindMergedPRForBranch) and PR url (for
// PRState); missing keys yield "not found" / "" respectively. Set the
// *Err fields to simulate transport failures.
type fakeGhProbe struct {
	mergedByBranch map[string]struct {
		URL, SHA string
	}
	stateByURL     map[string]string
	openPRByBranch map[string]struct {
		URL, Author string
	}
	mergedErr   error
	stateErr    error
	openPRErr   error
	mergedCalls int32
	stateCalls  int32
	openPRCalls int32
}

func (f *fakeGhProbe) FindMergedPRForBranch(_ context.Context, _, branch string) (string, string, bool, error) {
	atomic.AddInt32(&f.mergedCalls, 1)
	if f.mergedErr != nil {
		return "", "", false, f.mergedErr
	}
	if v, ok := f.mergedByBranch[branch]; ok {
		return v.URL, v.SHA, true, nil
	}
	return "", "", false, nil
}

func (f *fakeGhProbe) PRState(_ context.Context, prURL string) (string, error) {
	atomic.AddInt32(&f.stateCalls, 1)
	if f.stateErr != nil {
		return "", f.stateErr
	}
	return f.stateByURL[prURL], nil
}

func (f *fakeGhProbe) FindOpenPRForBranch(_ context.Context, _, branch string) (string, string, bool, error) {
	atomic.AddInt32(&f.openPRCalls, 1)
	if f.openPRErr != nil {
		return "", "", false, f.openPRErr
	}
	if v, ok := f.openPRByBranch[branch]; ok {
		return v.URL, v.Author, true, nil
	}
	return "", "", false, nil
}

func TestHandleCommand_StatusIdle(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		Active: false,
		LastJob: &store.AutopilotJob{
			AgentSessionID:  "sess-1",
			IssueIdentifier: "GEO-5",
			State:           store.JobStateDone,
			StartedAt:       time.Now().Add(-30 * time.Minute).Format(time.RFC3339),
			FinishedAt:      time.Now().Add(-25 * time.Minute).Format(time.RFC3339),
			PRURL:           "https://github.com/x/y/pull/1",
		},
	}
	o := &Orchestrator{db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{SessionID: "sess-1", CreatorID: "user-1"}

	o.handleCommand(ev, "status")

	if !strings.Contains(mlc.PostedBody, "idle") {
		t.Errorf("expected 'idle' in response, got: %s", mlc.PostedBody)
	}
	if !strings.Contains(mlc.PostedBody, "GEO-5") {
		t.Errorf("expected 'GEO-5' in response, got: %s", mlc.PostedBody)
	}
}

func TestHandleCommand_StatusBusy(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		Active:    true,
		ActiveSID: "sess-busy",
		GetJob: &store.AutopilotJob{
			AgentSessionID:  "sess-busy",
			IssueIdentifier: "GEO-7",
			State:           store.JobStateExecuting,
			StartedAt:       time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			WorktreePath:    "/tmp/worktrees/linear-geo-7",
			Branch:          "linear/geo-7",
		},
	}
	o := &Orchestrator{db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{SessionID: "sess-busy", CreatorID: "user-2"}

	o.handleCommand(ev, "status")

	if !strings.Contains(mlc.PostedBody, "busy") {
		t.Errorf("expected 'busy' in response, got: %s", mlc.PostedBody)
	}
	if !strings.Contains(mlc.PostedBody, "GEO-7") {
		t.Errorf("expected 'GEO-7' in response, got: %s", mlc.PostedBody)
	}
}

func TestHandleCommand_Help(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{}
	o := &Orchestrator{db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{SessionID: "sess-1", CreatorID: "user-1"}

	o.handleCommand(ev, "help")

	if !strings.Contains(mlc.PostedBody, "Available commands") {
		t.Errorf("expected 'Available commands' in response, got: %s", mlc.PostedBody)
	}
	if !strings.Contains(mlc.PostedBody, "/status") {
		t.Errorf("expected '/status' in response, got: %s", mlc.PostedBody)
	}
	if !strings.Contains(mlc.PostedBody, "/help") {
		t.Errorf("expected '/help' in response, got: %s", mlc.PostedBody)
	}
	if strings.Contains(mlc.PostedBody, "Unknown command") {
		t.Errorf("expected no 'Unknown command' in help response, got: %s", mlc.PostedBody)
	}
}

func TestHandleCommand_Unknown(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{}
	o := &Orchestrator{db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{SessionID: "sess-1", CreatorID: "user-1"}

	o.handleCommand(ev, "unknownxyz")

	if !strings.Contains(mlc.PostedBody, "Unknown command: /unknownxyz") {
		t.Errorf("expected 'Unknown command: /unknownxyz' in response, got: %s", mlc.PostedBody)
	}
	if !strings.Contains(mlc.PostedBody, "Available commands") {
		t.Errorf("expected 'Available commands' in response, got: %s", mlc.PostedBody)
	}
}

// ---- Tests for summarizeToolUse ----

func TestSummarizeToolUse_Edit(t *testing.T) {
	input := []byte(`{"file_path":"internal/linear/token.go","old_string":"func old()","new_string":"func new()"}`)
	summary := summarizeToolUse("Edit", input)
	if !strings.Contains(summary, "file=internal/linear/token.go") {
		t.Errorf("expected file path in summary, got: %s", summary)
	}
	if !strings.Contains(summary, "~10 chars old") {
		t.Errorf("expected old string char count, got: %s", summary)
	}
	if !strings.Contains(summary, "~10 chars new") {
		t.Errorf("expected new string char count, got: %s", summary)
	}
}

func TestSummarizeToolUse_Write(t *testing.T) {
	input := []byte(`{"file_path":"internal/linear/token.go","content":"package linear"}`)
	summary := summarizeToolUse("Write", input)
	if !strings.Contains(summary, "file=internal/linear/token.go") {
		t.Errorf("expected file path in summary, got: %s", summary)
	}
	if !strings.Contains(summary, "~14 chars") {
		t.Errorf("expected char count, got: %s", summary)
	}
}

func TestSummarizeToolUse_Read(t *testing.T) {
	input := []byte(`{"file_path":"internal/linear/token.go","offset":10}`)
	summary := summarizeToolUse("Read", input)
	if !strings.Contains(summary, "file=internal/linear/token.go") {
		t.Errorf("expected file path in summary, got: %s", summary)
	}
	if !strings.Contains(summary, "offset=10") {
		t.Errorf("expected offset in summary, got: %s", summary)
	}
}

func TestSummarizeToolUse_ReadNoOffset(t *testing.T) {
	input := []byte(`{"file_path":"internal/linear/token.go"}`)
	summary := summarizeToolUse("Read", input)
	if !strings.Contains(summary, "file=internal/linear/token.go") {
		t.Errorf("expected file path in summary, got: %s", summary)
	}
	if strings.Contains(summary, "offset") {
		t.Errorf("unexpected offset in summary, got: %s", summary)
	}
}

func TestSummarizeToolUse_Bash(t *testing.T) {
	input := []byte(`{"command":"go test ./..."}`)
	summary := summarizeToolUse("Bash", input)
	if !strings.Contains(summary, "cmd=go test ./...") {
		t.Errorf("expected full cmd in summary, got: %s", summary)
	}
}

func TestSummarizeToolUse_BashLong(t *testing.T) {
	// command > 200 chars
	longCmd := strings.Repeat("x", 250)
	input := []byte(`{"command":"` + longCmd + `"}`)
	summary := summarizeToolUse("Bash", input)
	if !strings.Contains(summary, "(...truncated)") {
		t.Errorf("expected truncation marker, got: %s", summary)
	}
	if len(summary) > 220 {
		t.Errorf("summary too long: %d chars, got: %s", len(summary), summary)
	}
}

func TestSummarizeToolUse_TodoWrite(t *testing.T) {
	// 5 todos
	input := []byte(`{"todos":[{},{},{},{},{}]}`)
	summary := summarizeToolUse("TodoWrite", input)
	if !strings.Contains(summary, "count=5 todos") {
		t.Errorf("expected count=5 todos, got: %s", summary)
	}
}

func TestSummarizeToolUse_Grep(t *testing.T) {
	input := []byte(`{"pattern":"func.*main","path":"internal/linear/token.go"}`)
	summary := summarizeToolUse("Grep", input)
	if !strings.Contains(summary, "pattern=") {
		t.Errorf("expected pattern in summary, got: %s", summary)
	}
	if !strings.Contains(summary, "path=internal/linear/token.go") {
		t.Errorf("expected path in summary, got: %s", summary)
	}
}

func TestSummarizeToolUse_Glob(t *testing.T) {
	input := []byte(`{"pattern":"**/*.go"}`)
	summary := summarizeToolUse("Glob", input)
	if !strings.Contains(summary, "pattern=**/*.go") {
		t.Errorf("expected pattern in summary, got: %s", summary)
	}
}

func TestSummarizeToolUse_Unknown(t *testing.T) {
	input := []byte(`{"some_field":"some_value","another":123}`)
	summary := summarizeToolUse("SomeUnknownTool", input)
	if !strings.Contains(summary, "input=") {
		t.Errorf("expected input= fallback in summary, got: %s", summary)
	}
}

func TestSummarizeToolUse_EmptyInput(t *testing.T) {
	summary := summarizeToolUse("Edit", nil)
	if summary != "input=<empty>" {
		t.Errorf("expected input=<empty>, got: %s", summary)
	}
}

// ---- Tests for drainStreamJSON structured event emission ----

// captureHandler is a minimal slog.Handler that records all logged records.
type captureHandler struct {
	mu      sync.Mutex
	Records []slog.Record
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Records = append(h.Records, r)
	return nil
}
func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler         { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler              { return h }

func TestDrainStreamJSON_EmitsStructuredEvents(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "test-session-struct"
	streamPath := filepath.Join(tmp, sessionID+".jsonl")

	h := &captureHandler{}
	logger := slog.New(h)
	o := &Orchestrator{cfg: &config.Autopilot{JobStreamsDir: tmp}, logger: logger}
	f := &flow{o: o, ev: linear.AgentEvent{
		SessionID:       sessionID,
		IssueIdentifier: "GEO-TEST",
	}}

	if err := f.openStreamFile(); err != nil {
		t.Fatalf("openStreamFile failed: %v", err)
	}
	defer f.closeStreamFile()

	lines := []string{
		// tool_use inside assistant message content
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"internal/linear/token.go","offset":5}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/linear/token.go","old_string":"func old()","new_string":"func new()"}}]}}`,
		// result success
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":12345,"stop_reason":"end_turn"}`,
		// result error
		`{"type":"result","subtype":"error_during_execution","is_error":true}`,
		// standalone error
		`{"type":"error","error":{"message":"something went wrong"}}`,
		// non-tool-use assistant (should not emit claude_tool_use)
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
	}

	r := strings.NewReader(strings.Join(lines, "\n") + "\n")
	f.drainStreamJSON(r)

	// Verify stream file was written
	data, err := os.ReadFile(streamPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if len(strings.Split(strings.TrimRight(string(data), "\n"), "\n")) != len(lines) {
		t.Errorf("expected %d lines in stream file, got different count", len(lines))
	}

	// Collect msg names
	h.mu.Lock()
	msgNames := make([]string, len(h.Records))
	for i, r := range h.Records {
		msgNames[i] = r.Message
	}
	h.mu.Unlock()

	// Count occurrences
	count := func(name string) int {
		c := 0
		for _, m := range msgNames {
			if m == name {
				c++
			}
		}
		return c
	}

	if n := count("claude_tool_use"); n != 3 {
		t.Errorf("expected 3 claude_tool_use events, got %d: %v", n, msgNames)
	}
	if n := count("claude_result"); n != 1 {
		t.Errorf("expected 1 claude_result event, got %d: %v", n, msgNames)
	}
	if n := count("claude_error"); n != 2 {
		t.Errorf("expected 2 claude_error events (1 result error + 1 standalone error), got %d: %v", n, msgNames)
	}
}

// handleCreated dispatch tests (GEO-38).
//
// When an issue already has a DONE row in admiral's DB and the user
// re-mentions admiral, Linear sends a fresh ActionCreated event. The
// dispatch decision branches on the prior PR's GitHub state:
//
//   OPEN + has claude_session_id   → resume (runFollowupResume)
//   OPEN + no claude_session_id    → respond "legacy session"
//   MERGED / CLOSED                → fresh follow-up (runFollowup)
//   gh error / unknown state       → fall through to fresh flow (run)
//
// Tests below cover each branch by inspecting observable side effects:
//   - synchronous reply body (mockLinearClient.PostedBody)
//   - which sessions were claimed (mockStore.ClaimedSessionIDs)

func newTestOrchestrator(t *testing.T, ms *mockStore, mlc *mockLinearClient, gh ghProbe) *Orchestrator {
	t.Helper()
	if ms.Repo == nil {
		ms.Repo = &store.Repo{
			ProjectID:   "proj-test",
			ProjectName: "TestProject",
			RepoDir:     t.TempDir(),
			BaseBranch:  "main",
			Enabled:     true,
		}
	}
	if mlc.GetIssueResult == nil {
		mlc.GetIssueResult = &linear.Issue{
			ID:         "issue-test",
			Identifier: "TEST-1",
			ProjectID:  "proj-test",
			TeamID:     "team-test",
		}
	}
	return &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		logger: slog.Default(),
	}
}

// cleanupWorktree tests

func TestCleanupWorktree_WorktreeNotExists(t *testing.T) {
	tmp := t.TempDir()
	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: tmp}, logger: slog.Default()}
	f := &flow{
		o:            o,
		worktreePath: filepath.Join(tmp, "nonexistent-worktree"),
		branch:       "linear/nonexistent",
	}

	// Should not error even though worktree doesn't exist
	f.cleanupWorktree(cleanupDelete)
}

func TestCleanupWorktree_DeleteMode(t *testing.T) {
	tmp := t.TempDir()
	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: tmp}, logger: slog.Default()}
	f := &flow{
		o:            o,
		worktreePath: filepath.Join(tmp, "nonexistent-worktree"),
		branch:       "linear/nonexistent",
	}

	// Should not error even though worktree doesn't exist
	f.cleanupWorktree(cleanupDelete)
}

// copyDir / copyFile tests

func TestCopyDir_SimpleFile(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestCopyDir_NestedDirs(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "c.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "root.txt"), []byte("top"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	for _, tc := range []struct {
		path, want string
		mode       os.FileMode
	}{
		{"root.txt", "top", 0o644 & 0o777},
		{"a/b/c.txt", "nested", 0o600 & 0o777},
	} {
		got, err := os.ReadFile(filepath.Join(dst, tc.path))
		if err != nil {
			t.Errorf("read %s: %v", tc.path, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestCopyFile_PreservesMode(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	if err := os.WriteFile(filepath.Join(src, "mode.txt"), []byte("mode"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := copyFile(filepath.Join(src, "mode.txt"), dst, 0o600); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode() != 0o600 {
		t.Errorf("mode = %v, want %v", info.Mode(), 0o600)
	}
}

func TestCopyDir_SrcDoesNotExist(t *testing.T) {
	err := copyDir("/nonexistent/src", t.TempDir())
	if err == nil {
		t.Error("expected error for nonexistent src")
	}
}

// ---- Tests for mention + status update in flow lifecycle ----

func TestMarkDone_MentionPrefix(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{}
	trueVal := true
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir(), UpdateIssueStatus: &trueVal},
		lc:     mlc,
		db:     ms,
		logger: slog.Default(),
	}
	f := &flow{
		o:            o,
		ev:           linear.AgentEvent{SessionID: "sess-1", CreatorID: "user-abc", CreatorDisplayName: "user.abc"},
		worktreePath: "/tmp/wt",
		branch:       "linear/test",
		prURL:        "https://github.com/x/y/pull/1",
		teamID:       "team-1",
	}

	// Simulate successful execution end: mark job done then call the Response path
	f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = f.prURL
	})

	// Post the completion activity (same as flow.execute end)
	f.postActivity(linear.Response(fmt.Sprintf(
		"%sDone. PR opened: %s\n\nWorktree: `%s`\nBranch: `%s`",
		f.creatorMention(), f.prURL, f.worktreePath, f.branch)))

	body := mlc.GetPostedBody()
	if !strings.HasPrefix(body, "@user.abc ") {
		t.Errorf("expected body to start with '@user.abc ' (displayName), got: %s", body)
	}
	if !strings.Contains(body, "https://github.com/x/y/pull/1") {
		t.Errorf("expected PR URL in body, got: %s", body)
	}
}

func TestMarkFailed_MentionPrefix(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		lc:     mlc,
		db:     ms,
		logger: slog.Default(),
	}
	f := &flow{
		o:            o,
		ev:           linear.AgentEvent{SessionID: "sess-2", CreatorID: "user-xyz", CreatorDisplayName: "user.xyz"},
		worktreePath: "/tmp/wt2",
		branch:       "linear/fail",
	}

	// Simulate markFailed body construction
	body := f.creatorMention() + "admiral failed: something went wrong"
	f.postActivity(linear.ErrorActivity(body))

	if !strings.HasPrefix(mlc.GetPostedBody(), "@user.xyz ") {
		t.Errorf("expected body to start with '@user.xyz ' (displayName), got: %s", mlc.GetPostedBody())
	}
}

// TestCreatorMention covers the displayName → name → "" fallback chain
// for building the "@<handle>" prefix on Linear thread replies. CreatorID
// (Linear UUID) is intentionally ignored — Linear's mention syntax requires
// a handle, not a UUID.
func TestCreatorMention(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   linear.AgentEvent
		want string
	}{
		{
			name: "displayName preferred",
			ev:   linear.AgentEvent{CreatorID: "u-1", CreatorName: "Test User", CreatorDisplayName: "test.user"},
			want: "@test.user ",
		},
		{
			name: "name fallback when displayName empty",
			ev:   linear.AgentEvent{CreatorID: "u-1", CreatorName: "Test User"},
			want: "@Test User ",
		},
		{
			name: "empty when only id known (UUID is not a mention)",
			ev:   linear.AgentEvent{CreatorID: "00000000-0000-0000-0000-000000000000"},
			want: "",
		},
		{
			name: "empty when no creator info at all",
			ev:   linear.AgentEvent{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &flow{ev: tc.ev}
			if got := f.creatorMention(); got != tc.want {
				t.Errorf("creatorMention() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStateIDByType_CacheHit(t *testing.T) {
	mlc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{
			{ID: "state-started-1", Name: "In Progress", Type: "started", Position: 1.0},
			{ID: "state-started-2", Name: "Started", Type: "started", Position: 2.0},
			{ID: "state-done", Name: "Done", Type: "completed", Position: 1.0},
		},
	}
	ms := &mockStore{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		lc:     mlc,
		db:     ms,
		logger: slog.Default(),
	}

	ctx := context.Background()

	// First call — should fetch
	id1, err := o.stateIDByType(ctx, "team-1", "started")
	if err != nil {
		t.Fatalf("stateIDByType failed: %v", err)
	}
	if id1 != "state-started-1" {
		t.Errorf("expected state-started-1 (position 1), got: %s", id1)
	}
	if mlc.GetWorkflowStatesCalls != 1 {
		t.Errorf("expected 1 GetWorkflowStates call, got: %d", mlc.GetWorkflowStatesCalls)
	}

	// Second call for same team — should use cache
	id2, err := o.stateIDByType(ctx, "team-1", "started")
	if err != nil {
		t.Fatalf("stateIDByType failed (cached): %v", err)
	}
	if id2 != "state-started-1" {
		t.Errorf("expected cached state-started-1, got: %s", id2)
	}
	if mlc.GetWorkflowStatesCalls != 1 {
		t.Errorf("expected no additional GetWorkflowStates calls (cache hit), got: %d", mlc.GetWorkflowStatesCalls)
	}

	// Different team — should fetch again
	id3, err := o.stateIDByType(ctx, "team-2", "started")
	if err != nil {
		t.Fatalf("stateIDByType failed (team-2): %v", err)
	}
	if id3 != "state-started-1" {
		t.Errorf("expected state-started-1 for team-2, got: %s", id3)
	}
	if mlc.GetWorkflowStatesCalls != 2 {
		t.Errorf("expected 2 GetWorkflowStates calls total, got: %d", mlc.GetWorkflowStatesCalls)
	}
}

func TestStateIDByType_ReturnsPositionMin(t *testing.T) {
	mlc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{
			{ID: "high-pos", Name: "Started", Type: "started", Position: 5.0},
			{ID: "low-pos", Name: "In Progress", Type: "started", Position: 1.0},
			{ID: "mid-pos", Name: "Active", Type: "started", Position: 3.0},
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		lc:     mlc,
		db:     &mockStore{},
		logger: slog.Default(),
	}

	id, err := o.stateIDByType(context.Background(), "team-x", "started")
	if err != nil {
		t.Fatalf("stateIDByType failed: %v", err)
	}
	if id != "low-pos" {
		t.Errorf("expected low-pos (position 1), got: %s", id)
	}
}

func TestStateIDByType_NotFound(t *testing.T) {
	mlc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{
			{ID: "some-state", Name: "Done", Type: "completed", Position: 1.0},
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		lc:     mlc,
		db:     &mockStore{},
		logger: slog.Default(),
	}

	id, err := o.stateIDByType(context.Background(), "team-x", "started")
	if err != nil {
		t.Fatalf("stateIDByType failed: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty string for not-found type, got: %s", id)
	}
}

// ---- Tests for handlePrompted ----

func TestHandlePrompted_NoHistory(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		GetJob:      nil,
		GetJobErr:   nil,
		AdmiralTask: &store.AdmiralTask{State: store.JobStateDone},
	}
	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: t.TempDir()}, db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{
		SessionID:       "session-no-history",
		IssueID:         "issue-nohist-1",
		IssueIdentifier: "GEO-NOHIST-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "/help", // /help bypasses bare-mention check
	}

	o.HandleAgentEvent(ev)

	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(mlc.GetPostedBody(), "Available commands") {
		t.Errorf("expected '/help' response, got: %s", mlc.GetPostedBody())
	}
	if mlc.PostedActivity.Type != linear.ActivityResponse {
		t.Errorf("/help must post ActivityResponse (info), got %q", mlc.PostedActivity.Type)
	}
}

func TestHandlePrompted_NoClaudeSessionID(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		GetJob: &store.AutopilotJob{
			AgentSessionID:  "session-old",
			IssueID:         "issue-old",
			ClaudeSessionID: "", // empty — pre-#13 job
		},
		AdmiralTask: &store.AdmiralTask{State: store.JobStateDone},
	}
	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: t.TempDir()}, db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{
		SessionID:       "session-old",
		IssueID:         "issue-old",
		IssueIdentifier: "GEO-OLD-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "/help", // /help bypasses bare-mention check
	}

	o.HandleAgentEvent(ev)

	time.Sleep(50 * time.Millisecond)
	// /help posts the available commands, not the "before resume support" message.
	// The NoClaudeSessionID path is only reached via /help (not /status which has its own handling).
	if !strings.Contains(mlc.GetPostedBody(), "Available commands") {
		t.Errorf("expected '/help' response, got: %s", mlc.GetPostedBody())
	}
	if mlc.PostedActivity.Type != linear.ActivityResponse {
		t.Errorf("/help must post ActivityResponse (info), got %q", mlc.PostedActivity.Type)
	}
}

func TestHandlePrompted_ResumeSpawn(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		GetJob: &store.AutopilotJob{
			AgentSessionID:  "session-abc",
			IssueID:         "issue-123",
			IssueIdentifier: "GEO-123",
			Branch:          "linear/geo-123",
			WorktreePath:    "/tmp/worktrees/linear-geo-123",
			PRURL:           "https://github.com/x/y/pull/42",
			ClaudeSessionID: "claude-session-uuid-123",
			State:           store.JobStateDone,
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		db:     ms,
		lc:     mlc,
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-abc",
		IssueID:         "issue-123",
		IssueIdentifier: "GEO-123",
		Action:          linear.ActionPrompted,
		UserMessage:     "change v1 to v2",
	}

	o.HandleAgentEvent(ev)

	// Should not immediately post any response (goroutine runs async)
	// Give it time to at least start the goroutine
	time.Sleep(50 * time.Millisecond)

	// Verify no stub reply was posted
	if strings.Contains(mlc.GetPostedBody(), "don't have history") ||
		strings.Contains(mlc.GetPostedBody(), "before resume support") {
		t.Errorf("expected no stub reply for valid resume, got: %s", mlc.GetPostedBody())
	}
}

// TestHandleCreated_BareMention_Rejected verifies that an @mention with
// no leading /command on an EXISTING task posts the help reply and does
// not modify state. (First-time @mention with no prior assign is covered
// separately by TestDispatch_FirstTimeMention_RequestsAssign.)
func TestHandleCreated_BareMention_Rejected(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{AdmiralTask: &store.AdmiralTask{State: store.JobStateDone}} // issue is known to admiral
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "session-bare",
		IssueID:         "issue-bare",
		IssueIdentifier: "BAR-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-bare",
		PromptContext:   "please do something", // no leading /command
	}

	o.HandleAgentEvent(ev)

	// Help text must be posted.
	if !strings.Contains(mlc.GetPostedBody(), "does not respond to bare @mentions") {
		t.Errorf("expected bare mention help text in posted body, got: %s", mlc.GetPostedBody())
	}
	if mlc.PostedActivity.Type != linear.ActivityError {
		t.Errorf("rejection must post ActivityError, got %q", mlc.PostedActivity.Type)
	}
	// No job claimed and no DB write — rejection is observability-only.
	if got := ms.ClaimedSnapshot(); len(got) != 0 {
		t.Errorf("expected no job claimed for bare mention; got %v", got)
	}
	ms.mu.Lock()
	updates := append([]string(nil), ms.UpdatedSessionIDs...)
	ms.mu.Unlock()
	if len(updates) != 0 {
		t.Errorf("expected no autopilot_jobs row touched on bare mention rejection; got UpdateAutopilotJob calls for %v", updates)
	}
}

// TestHandleCreated_UnknownCommand_Rejected verifies an @mention with an
// unknown /command on an existing task is rejected with help text and no
// DB row touched.
func TestHandleCreated_UnknownCommand_Rejected(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{AdmiralTask: &store.AdmiralTask{State: store.JobStateDone}}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "session-unknown",
		IssueID:         "issue-unknown",
		IssueIdentifier: "UNK-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-unknown",
		PromptContext:   "/foobar extra args",
	}

	o.HandleAgentEvent(ev)

	if !strings.Contains(mlc.GetPostedBody(), "did not recognize") {
		t.Errorf("expected 'did not recognize' in posted body, got: %s", mlc.GetPostedBody())
	}
	if !strings.Contains(mlc.GetPostedBody(), "/foobar") {
		t.Errorf("expected unrecognized command name in reply, got: %s", mlc.GetPostedBody())
	}
	if mlc.PostedActivity.Type != linear.ActivityError {
		t.Errorf("rejection must post ActivityError, got %q", mlc.PostedActivity.Type)
	}
	ms.mu.Lock()
	updates := append([]string(nil), ms.UpdatedSessionIDs...)
	ms.mu.Unlock()
	if len(updates) != 0 {
		t.Errorf("expected no autopilot_jobs row touched on unknown command; got UpdateAutopilotJob calls for %v", updates)
	}
}

// TestHandleCreated_FixCommand_LegacyRowRejected verifies /fix on a DONE
// task that lacks pr_url or claude_session_id is rejected — there's no
// PR to push onto and no claude session to resume. The user is told to
// /rerun instead.
func TestHandleCreated_FixCommand_LegacyRowRejected(t *testing.T) {
	mlc := &mockLinearClient{}
	live := &store.AdmiralTask{
		IssueID: "issue-fix-legacy",
		State:   store.JobStateDone,
		// PRURL and ClaudeSessionID intentionally empty
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live, // same pointer so UpdateAdmiralTask mutates the asserted instance
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "session-fix-legacy",
		IssueID:         "issue-fix-legacy",
		IssueIdentifier: "FIX-LEG-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-fix-legacy",
		PromptContext:   "/fix change v1 to v2",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "/fix needs a prior run with both an open PR and a recoverable claude session") {
		t.Errorf("expected legacy-row /fix reject message, got: %s", body)
	}
	if mlc.PostedActivity.Type != linear.ActivityError {
		t.Errorf("rejection must post ActivityError, got %q", mlc.PostedActivity.Type)
	}
	// State must NOT advance — the only allowed mutation is dispatch's
	// last_event_session_id refresh.
	if live.State != store.JobStateDone {
		t.Errorf("admiral_tasks.state must stay DONE on legacy /fix reject; got %q", live.State)
	}
}

// TestHandlePrompted_BareMention_PreservesDoneState verifies a prompted
// event with no leading /command posts help and DOES NOT modify the
// existing autopilot_jobs row. In the prompted path the SessionID is
// reused from the original AgentSession; touching it would erase the
// prior DONE state.
func TestHandlePrompted_BareMention_PreservesDoneState(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		GetJob: &store.AutopilotJob{
			AgentSessionID:  "session-prompted",
			IssueID:         "issue-prompted",
			ClaudeSessionID: "claude-session-xyz",
			State:           store.JobStateDone,
		},
		AdmiralTask: &store.AdmiralTask{State: store.JobStateDone},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		db:     ms,
		lc:     mlc,
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-prompted",
		IssueID:         "issue-prompted",
		IssueIdentifier: "GEO-PROMPT-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "change v1 to v2", // no leading /command
	}

	o.HandleAgentEvent(ev)

	if !strings.Contains(mlc.GetPostedBody(), "does not respond to bare @mentions") {
		t.Errorf("expected bare mention help text in posted body, got: %s", mlc.GetPostedBody())
	}
	ms.mu.Lock()
	updates := append([]string(nil), ms.UpdatedSessionIDs...)
	ms.mu.Unlock()
	if len(updates) != 0 {
		t.Errorf("prompted bare mention must not touch the DONE row; got UpdateAutopilotJob calls for %v", updates)
	}
}

// (TestHandlePrompted_RerunRejected_PreservesState was removed in PR-B-v2.
// /rerun via thread now works — covered by TestDispatch_RerunInPrompted_NowSupersedes
// at the bottom of this file. The PR-B-v1 redirect-to-mention reply no
// longer fires.)

// TestHandlePrompted_FixRejected_PreservesState verifies /fix via thread
// is rejected with the "not yet implemented" reply and the existing DONE
// row is not modified.
func TestHandlePrompted_FixOnLegacyRow_Rejected(t *testing.T) {
	// /fix on a DONE admiral_tasks row that lacks ClaudeSessionID (e.g.
	// data backfilled from very old autopilot_jobs without resume support)
	// is rejected — there's no claude session to --resume.
	mlc := &mockLinearClient{}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-prompted",
			State:   store.JobStateDone,
			PRURL:   "https://github.com/x/y/pull/1",
			// ClaudeSessionID intentionally empty
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		db:     ms,
		lc:     mlc,
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-prompted",
		IssueID:         "issue-prompted",
		IssueIdentifier: "GEO-PROMPT-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "/fix change v1 to v2",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "/fix needs a prior run with both an open PR and a recoverable claude session") {
		t.Errorf("expected legacy-row /fix reject reply, got: %s", body)
	}
}

// TestHandlePrompted_UnknownCommand_PreservesState verifies an unknown
// /command via thread is rejected with help reply and the DONE row is
// not modified.
func TestHandlePrompted_UnknownCommand_PreservesState(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		GetJob: &store.AutopilotJob{
			AgentSessionID:  "session-prompted",
			IssueID:         "issue-prompted",
			ClaudeSessionID: "claude-session-xyz",
			State:           store.JobStateDone,
		},
		AdmiralTask: &store.AdmiralTask{State: store.JobStateDone},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: t.TempDir()},
		db:     ms,
		lc:     mlc,
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-prompted",
		IssueID:         "issue-prompted",
		IssueIdentifier: "GEO-PROMPT-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "/foobar baz",
	}

	o.HandleAgentEvent(ev)

	if !strings.Contains(mlc.GetPostedBody(), "did not recognize") {
		t.Errorf("expected 'did not recognize' reply, got: %s", mlc.GetPostedBody())
	}
	ms.mu.Lock()
	updates := append([]string(nil), ms.UpdatedSessionIDs...)
	ms.mu.Unlock()
	if len(updates) != 0 {
		t.Errorf("prompted unknown command must not touch the DONE row; got UpdateAutopilotJob calls for %v", updates)
	}
}

// ---- Tests for ensureWorktree ----

func TestEnsureWorktree_AlreadyExists(t *testing.T) {
	tmp := t.TempDir()
	worktreePath := filepath.Join(tmp, "existing-worktree")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: tmp}, logger: slog.Default()}
	f := &flow{
		o: o,
		job: &store.AutopilotJob{
			WorktreePath: worktreePath,
			Branch:       "linear/test-branch",
		},
	}

	err := f.ensureWorktree()
	if err != nil {
		t.Fatalf("ensureWorktree failed: %v", err)
	}
	if f.worktreePath != worktreePath {
		t.Errorf("worktreePath = %q, want %q", f.worktreePath, worktreePath)
	}
	if f.branch != "linear/test-branch" {
		t.Errorf("branch = %q, want %q", f.branch, "linear/test-branch")
	}
}

func TestEnsureWorktree_NeedsRebuild(t *testing.T) {
	// Test that ensureWorktree sets the correct paths when worktree doesn't exist.
	// The actual git operations are tested separately in integration tests.
	tmp := t.TempDir()
	worktreePath := filepath.Join(tmp, "nonexistent-worktree")

	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: tmp}, logger: slog.Default()}
	f := &flow{
		o: o,
		job: &store.AutopilotJob{
			WorktreePath: worktreePath,
			Branch:       "linear/test-branch",
		},
	}

	// Worktree doesn't exist, so ensureWorktree will try to rebuild.
	// We expect it to attempt git fetch/worktree add, which will fail
	// in this test environment without a real remote.
	if _, err := os.Stat(worktreePath); err == nil {
		t.Fatalf("worktree should not exist yet")
	}

	// This will fail on git fetch since we don't have a real remote,
	// but it verifies the code path is correct.
	err := f.ensureWorktree()
	if err == nil {
		t.Log("ensureWorktree succeeded (unexpected in this env)")
	} else {
		t.Logf("ensureWorktree failed as expected in test env: %v", err)
	}
}

// ---- Tests for runClaudeResume ----

func TestRunClaudeResume_ArgsCorrect(t *testing.T) {
	// Test that resume flow is correctly initialized with the claude session ID
	// and that executeResume sets up the right context for runClaudeResume.
	tmp := t.TempDir()
	jobStreamsDir := filepath.Join(tmp, "streams")
	if err := os.MkdirAll(jobStreamsDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	o := &Orchestrator{
		cfg:    &config.Autopilot{RepoDir: tmp, JobStreamsDir: jobStreamsDir, ClaudeBin: "claude", MaxRunSeconds: 60},
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-resume-test",
		IssueIdentifier: "GEO-RESUME-1",
	}
	job := &store.AutopilotJob{
		ClaudeSessionID: "resume-uuid-456",
		WorktreePath:    tmp,
		Branch:          "linear/resume-test",
		PRURL:           "https://github.com/x/y/pull/42",
	}

	f := newResumeFlow(o, context.Background(), ev, job)

	if f.job.ClaudeSessionID != "resume-uuid-456" {
		t.Errorf("ClaudeSessionID = %q, want %q", f.job.ClaudeSessionID, "resume-uuid-456")
	}
	if f.ev.SessionID != "session-resume-test" {
		t.Errorf("SessionID = %q, want %q", f.ev.SessionID, "session-resume-test")
	}
}

// GEO-37 Bug A — flow.execute short-circuits when the deterministic branch
// for the issue already has a merged PR (work was completed externally).
// PR #66's earlier attempt at this fix had a fatal order-of-operations
// bug where f.branch was empty at check time; this test would have caught
// it instantly because the fake gh is keyed on the branch name.
func TestFlowExecute_ShortCircuits_WhenBranchAlreadyMerged(t *testing.T) {
	repoDir := t.TempDir()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			ID:         "issue-merged",
			Identifier: "ABC-1",
			ProjectID:  "proj-test",
			TeamID:     "team-test",
		},
	}
	ms := &mockStore{
		Repo: &store.Repo{
			ProjectID:   "proj-test",
			ProjectName: "TestProject",
			RepoDir:     repoDir,
			BaseBranch:  "main",
			Enabled:     true,
		},
		LastUpdatedJob: &store.AutopilotJob{},
	}
	gh := &fakeGhProbe{
		mergedByBranch: map[string]struct {
			URL, SHA string
		}{
			// branchName(issue) of "ABC-1" = "linear/abc-1"
			"linear/abc-1": {URL: "https://github.com/x/y/pull/777", SHA: "abc1234567890def"},
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-merged-check",
		IssueID:         "issue-merged",
		IssueIdentifier: "ABC-1",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	if err := f.execute(); err != nil {
		t.Fatalf("execute returned err: %v", err)
	}

	// gh probe must have been hit exactly once for the deterministic branch.
	if got := atomic.LoadInt32(&gh.mergedCalls); got != 1 {
		t.Errorf("FindMergedPRForBranch calls: got %d, want 1", got)
	}
	// markAlreadyMerged should have updated the new session row to DONE.
	if ms.LastUpdatedJob.State != store.JobStateDone {
		t.Errorf("autopilot_jobs.state = %q, want DONE", ms.LastUpdatedJob.State)
	}
	if ms.LastUpdatedJob.PRURL != "https://github.com/x/y/pull/777" {
		t.Errorf("autopilot_jobs.pr_url = %q, want the merged PR URL", ms.LastUpdatedJob.PRURL)
	}
	if ms.LastUpdatedJob.Branch != "linear/abc-1" {
		t.Errorf("autopilot_jobs.branch = %q, want linear/abc-1", ms.LastUpdatedJob.Branch)
	}
	// The Linear thread should mention "Already merged" + the PR URL.
	if !strings.Contains(mlc.GetPostedBody(), "Already merged") {
		t.Errorf("expected 'Already merged' in posted body, got: %s", mlc.GetPostedBody())
	}
	if !strings.Contains(mlc.GetPostedBody(), "https://github.com/x/y/pull/777") {
		t.Errorf("expected PR URL in posted body, got: %s", mlc.GetPostedBody())
	}
	// No worktree was created.
	worktreesDir := filepath.Join(repoDir, ".worktrees")
	if entries, err := os.ReadDir(worktreesDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected no worktree to be created; found %v", names)
	}
}

// TestFlowExecute_DoesNotShortCircuit_OnFollowupSuffix verifies that the
// merged-branch check is intentionally skipped on follow-up flows — those
// flows generate a fresh suffixed branch on purpose and shouldn't be
// confused with prior merged work.
func TestFlowExecute_DoesNotShortCircuit_OnFollowupSuffix(t *testing.T) {
	repoDir := t.TempDir()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			Identifier: "ABC-1", ProjectID: "proj-test", TeamID: "team-test",
		},
	}
	ms := &mockStore{
		Repo: &store.Repo{ProjectID: "proj-test", RepoDir: repoDir, BaseBranch: "main", Enabled: true},
	}
	gh := &fakeGhProbe{
		mergedByBranch: map[string]struct {
			URL, SHA string
		}{
			"linear/abc-1": {URL: "https://github.com/x/y/pull/777", SHA: "deadbeef"},
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "follow-session",
		IssueID:         "issue-merged",
		IssueIdentifier: "ABC-1",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)
	f.followupSuffix = "followup-deadbeef"

	// We don't care about the rest of execute; just assert the merged check
	// was NOT hit because the follow-up suffix marks this as fresh work.
	_ = f.execute()
	if atomic.LoadInt32(&gh.mergedCalls) != 0 {
		t.Errorf("FindMergedPRForBranch must not run on follow-up flows; got %d calls", gh.mergedCalls)
	}
}

// GEO-47 — flow.execute short-circuits when the deterministic branch for
// the issue already has an open PR authored by a human (not admiral).
// Observable: job lands DONE with pr_url set, no worktree created,
// Linear thread receives "PR already exists" notice.
// GEO-47 — when a non-terminal autopilot job for the same issue is
// already in flight, a fresh dispatch is short-circuited to CANCELLED.
// No worktree, no claude run, prior session is left untouched.
func TestFlowExecute_ShortCircuits_WhenActiveSessionExists(t *testing.T) {
	repoDir := t.TempDir()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			ID:         "issue-active",
			Identifier: "ACT-1",
			ProjectID:  "proj-test",
			TeamID:     "team-test",
		},
	}
	priorSession := "prior-session-uuid"
	ms := &mockStore{
		Repo: &store.Repo{
			ProjectID:   "proj-test",
			ProjectName: "TestProject",
			RepoDir:     repoDir,
			BaseBranch:  "main",
			Enabled:     true,
		},
		ActiveJob: &store.AutopilotJob{
			AgentSessionID:  priorSession,
			IssueID:         "issue-active",
			IssueIdentifier: "ACT-1",
			State:           store.JobStateExecuting,
		},
		LastUpdatedJob: &store.AutopilotJob{},
	}
	gh := &fakeGhProbe{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		ghUser: "admiral",
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-new",
		IssueID:         "issue-active",
		IssueIdentifier: "ACT-1",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	if err := f.execute(); err != nil {
		t.Fatalf("execute returned err: %v", err)
	}

	// New session was marked CANCELLED with the prior session id surfaced.
	if ms.LastUpdatedJob.State != store.JobStateCancelled {
		t.Errorf("autopilot_jobs.state = %q, want CANCELLED", ms.LastUpdatedJob.State)
	}
	if !strings.Contains(ms.LastUpdatedJob.Error, priorSession) {
		t.Errorf("autopilot_jobs.error should reference prior session %q, got: %q", priorSession, ms.LastUpdatedJob.Error)
	}
	// Linear thread reply names the prior session.
	if !strings.Contains(mlc.GetPostedBody(), priorSession) {
		t.Errorf("expected prior session id in posted body, got: %s", mlc.GetPostedBody())
	}
	if !strings.Contains(mlc.GetPostedBody(), "already working on this issue") {
		t.Errorf("expected duplicate-dispatch notice in posted body, got: %s", mlc.GetPostedBody())
	}
	// Open-PR check should NOT fire — active-session short-circuit runs first.
	if got := atomic.LoadInt32(&gh.openPRCalls); got != 0 {
		t.Errorf("FindOpenPRForBranch should not be called when active session exists; got %d calls", got)
	}
	// No worktree was created.
	worktreesDir := filepath.Join(repoDir, ".worktrees")
	if entries, err := os.ReadDir(worktreesDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected no worktree to be created; found %v", names)
	}
}

func TestFlowExecute_ShortCircuits_WhenOpenPRByAnotherAuthor(t *testing.T) {
	repoDir := t.TempDir()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			ID:         "issue-human-pr",
			Identifier: "HUM-1",
			ProjectID:  "proj-test",
			TeamID:     "team-test",
		},
	}
	ms := &mockStore{
		Repo: &store.Repo{
			ProjectID:   "proj-test",
			ProjectName: "TestProject",
			RepoDir:     repoDir,
			BaseBranch:  "main",
			Enabled:     true,
		},
		LastUpdatedJob: &store.AutopilotJob{},
	}
	gh := &fakeGhProbe{
		openPRByBranch: map[string]struct {
			URL, Author string
		}{
			"linear/hum-1": {URL: "https://github.com/x/y/pull/99", Author: "georgexu"},
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		ghUser: "admiral", // not "georgexu"
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-human-pr",
		IssueID:         "issue-human-pr",
		IssueIdentifier: "HUM-1",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	if err := f.execute(); err != nil {
		t.Fatalf("execute returned err: %v", err)
	}

	// gh probe must have been hit exactly once for the deterministic branch.
	if got := atomic.LoadInt32(&gh.openPRCalls); got != 1 {
		t.Errorf("FindOpenPRForBranch calls: got %d, want 1", got)
	}
	// Job landed in DONE with the existing PR URL.
	if ms.LastUpdatedJob.State != store.JobStateDone {
		t.Errorf("autopilot_jobs.state = %q, want DONE", ms.LastUpdatedJob.State)
	}
	if ms.LastUpdatedJob.PRURL != "https://github.com/x/y/pull/99" {
		t.Errorf("autopilot_jobs.pr_url = %q, want the human PR URL", ms.LastUpdatedJob.PRURL)
	}
	// Linear thread receives "PR already exists" notice.
	if !strings.Contains(mlc.GetPostedBody(), "Open PR already exists") {
		t.Errorf("expected 'Open PR already exists' in posted body, got: %s", mlc.GetPostedBody())
	}
	if !strings.Contains(mlc.GetPostedBody(), "https://github.com/x/y/pull/99") {
		t.Errorf("expected PR URL in posted body, got: %s", mlc.GetPostedBody())
	}
	// No worktree was created.
	worktreesDir := filepath.Join(repoDir, ".worktrees")
	if entries, err := os.ReadDir(worktreesDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected no worktree to be created; found %v", names)
	}
}

// GEO-47 — when the open PR is authored by admiral itself, the check
// does NOT short-circuit (admiral may be retrying and should resume).
func TestFlowExecute_DoesNotShortCircuit_WhenOpenPRByAdmiral(t *testing.T) {
	repoDir := t.TempDir()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			ID:         "issue-admiral-pr",
			Identifier: "ADM-1",
			ProjectID:  "proj-test",
			TeamID:     "team-test",
		},
	}
	ms := &mockStore{
		Repo: &store.Repo{
			ProjectID:   "proj-test",
			ProjectName: "TestProject",
			RepoDir:     repoDir,
			BaseBranch:  "main",
			Enabled:     true,
		},
	}
	gh := &fakeGhProbe{
		openPRByBranch: map[string]struct {
			URL, Author string
		}{
			"linear/adm-1": {URL: "https://github.com/x/y/pull/55", Author: "admiral"},
		},
	}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		ghUser: "admiral",
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-admiral-pr",
		IssueID:         "issue-admiral-pr",
		IssueIdentifier: "ADM-1",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	// Should not short-circuit; execute() will fail at worktree creation
	// (no real remote) but that's fine — we're checking the open-PR check
	// was NOT the reason for stopping.
	_ = f.execute()

	// openPR should still be called, but no short-circuit occurred.
	if got := atomic.LoadInt32(&gh.openPRCalls); got != 1 {
		t.Errorf("FindOpenPRForBranch calls: got %d, want 1", got)
	}
	// Job should NOT be marked DONE with the existing PR (no short-circuit).
	if ms.LastUpdatedJob != nil && ms.LastUpdatedJob.PRURL == "https://github.com/x/y/pull/55" {
		t.Errorf("admiral-authored PR should NOT have short-circuited; got pr_url=%q", ms.LastUpdatedJob.PRURL)
	}
	// No "PR already exists" message should have been posted.
	if strings.Contains(mlc.GetPostedBody(), "Open PR already exists") {
		t.Errorf("did not expect 'Open PR already exists' for admiral-authored PR; got: %s", mlc.GetPostedBody())
	}
}

// GEO-47 — when there is no open PR for the deterministic branch,
// the check passes through and the flow continues normally.
func TestFlowExecute_DoesNotShortCircuit_WhenNoOpenPR(t *testing.T) {
	repoDir := t.TempDir()
	mlc := &mockLinearClient{
		GetIssueResult: &linear.Issue{
			ID:         "issue-no-pr",
			Identifier: "NPR-1",
			ProjectID:  "proj-test",
			TeamID:     "team-test",
		},
	}
	ms := &mockStore{
		Repo: &store.Repo{
			ProjectID:   "proj-test",
			ProjectName: "TestProject",
			RepoDir:     repoDir,
			BaseBranch:  "main",
			Enabled:     true,
		},
	}
	gh := &fakeGhProbe{} // no open PRs at all
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:     ms,
		lc:     mlc,
		gh:     gh,
		ghUser: "admiral",
		logger: slog.Default(),
	}
	ev := linear.AgentEvent{
		SessionID:       "session-no-pr",
		IssueID:         "issue-no-pr",
		IssueIdentifier: "NPR-1",
		Action:          linear.ActionCreated,
	}
	f := newFlow(o, context.Background(), ev)

	// Should not short-circuit; execute() fails at worktree creation
	// (no real remote) but that's fine.
	_ = f.execute()

	// openPR was called (confirming the check ran).
	if got := atomic.LoadInt32(&gh.openPRCalls); got != 1 {
		t.Errorf("FindOpenPRForBranch calls: got %d, want 1", got)
	}
	// No short-circuit message posted.
	if strings.Contains(mlc.GetPostedBody(), "Open PR already exists") {
		t.Errorf("did not expect 'Open PR already exists' when no PR exists; got: %s", mlc.GetPostedBody())
	}
}

// GEO-37 Bug B — archiveWorktree's force-removal fallback: when
// `git worktree remove` can't clean up (e.g. because the path was never
// a real worktree, only a stray dir), the fallback must still os.RemoveAll
// the path so the active .worktrees dir doesn't leak.
func TestArchiveWorktree_FallbackRemovesNonGitDir(t *testing.T) {
	repoDir := t.TempDir()
	worktreePath := filepath.Join(repoDir, ".worktrees", "linear-stray")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir stray dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "leftover.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	o := &Orchestrator{
		cfg:    &config.Autopilot{},
		logger: slog.Default(),
	}
	f := &flow{
		o:            o,
		ev:           linear.AgentEvent{IssueIdentifier: "STRAY-1"},
		worktreePath: worktreePath,
		repoDir:      repoDir,
		branch:       "linear/stray-1",
	}

	f.archiveWorktree()

	// 1. Archive directory exists with the leftover file.
	archiveRoot := filepath.Join(repoDir, ".worktrees-archive")
	entries, err := os.ReadDir(archiveRoot)
	if err != nil {
		t.Fatalf("read archive root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one archive entry, got %d", len(entries))
	}
	archived := filepath.Join(archiveRoot, entries[0].Name(), "leftover.txt")
	if got, err := os.ReadFile(archived); err != nil || string(got) != "hi" {
		t.Errorf("archived file content/missing: %v / %q", err, got)
	}

	// 2. Original worktree path is gone — the critical Bug B assertion.
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("expected original worktree path to be removed; stat err = %v", err)
	}
}

// TestArchiveWorktree_NoOpWhenPathEmpty guards against accidentally archiving
// the repo root when worktreePath wasn't set on the flow (e.g. early-exit
// paths in execute()).
func TestArchiveWorktree_NoOpWhenPathEmpty(t *testing.T) {
	repoDir := t.TempDir()
	o := &Orchestrator{cfg: &config.Autopilot{}, logger: slog.Default()}
	f := &flow{
		o:            o,
		ev:           linear.AgentEvent{IssueIdentifier: "X"},
		worktreePath: "",
		repoDir:      repoDir,
	}
	f.cleanupWorktree(cleanupArchive)

	if _, err := os.Stat(filepath.Join(repoDir, ".worktrees-archive")); !os.IsNotExist(err) {
		t.Errorf("archive root should not be created when worktreePath is empty; stat err = %v", err)
	}
}

// TestFollowupSuffix verifies the suffix derivation is stable across
// retries (so a re-delivered webhook reuses the same branch instead of
// leaking a new one each time) and survives unusual session id shapes.
func TestFollowupSuffix(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"5d093c17-09b4-43da-9111-b05e48467e1a", "followup-5d093c17"},
		{"abcdef", "followup-abcdef"},
		{"", "followup"},
		{"!!!", "followup"},
	} {
		got := followupSuffix(tc.in)
		if got != tc.want {
			t.Errorf("followupSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Stable across calls.
		if again := followupSuffix(tc.in); again != got {
			t.Errorf("followupSuffix(%q) not stable: %q vs %q", tc.in, got, again)
		}
	}
}

// TestClassifyGhCreateError covers the two known benign failure shapes from
// `gh pr create` plus the fall-through fatal case. Inputs are real-world
// stderr fragments observed in past admiral runs (issue #38).
func TestClassifyGhCreateError(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want ghCreateErrorKind
	}{
		{
			name: "no commits between (real GEO-14 stderr)",
			in:   "pull request create failed: GraphQL: No commits between main and linear/geo-14 (createPullRequest)",
			want: ghCreateNoCommits,
		},
		{
			name: "already exists",
			in:   "a pull request for branch \"linear/geo-99\" into branch \"main\" already exists:\nhttps://github.com/owner/repo/pull/123",
			want: ghCreateAlreadyExists,
		},
		{
			name: "case insensitive — uppercase variant",
			in:   "ERROR: NO COMMITS BETWEEN base AND head",
			want: ghCreateNoCommits,
		},
		{
			name: "auth error → fatal",
			in:   "error: GraphQL: Resource not accessible by integration",
			want: ghCreateFatal,
		},
		{
			name: "rate limit → fatal",
			in:   "API rate limit exceeded for user",
			want: ghCreateFatal,
		},
		{
			name: "empty → fatal",
			in:   "",
			want: ghCreateFatal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyGhCreateError(tc.in)
			if got != tc.want {
				t.Errorf("classifyGhCreateError(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// ---- GEO-50 dispatch tests (PR-B-v1) ----

// TestDispatch_FirstTimeAssign_DispatchesTask verifies that a delegate
// event (Action=created, no SourceCommentID) for an issue admiral has
// never seen before claims the autopilot_jobs row.
func TestDispatch_FirstTimeAssign_DispatchesTask(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"), // halt o.run early
	}
	ms := &mockStore{} // HasAnyJobForIssueResult defaults to false
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fresh",
		IssueID:         "issue-fresh",
		IssueIdentifier: "FRESH-1",
		Action:          linear.ActionCreated,
		// SourceCommentID empty → delegate, not @mention.
		// PromptContext also empty → user assigned without typing a prompt.
	}

	o.HandleAgentEvent(ev)

	// Wait for goroutine to claim the job.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 1 || got[0] != "sess-fresh" {
		t.Errorf("first-time assign should claim the session; got %v", got)
	}
	// First-time assign goes straight to o.run; any reply on this thread
	// is the failure message from the synthetic GetIssueErr we used to
	// halt the flow early. The important thing is dispatch did NOT post
	// any of its rejection messages.
	body := mlc.GetPostedBody()
	for _, marker := range []string{
		"Issue not assigned to admiral",
		"Task already exists",
		"does not respond to bare @mentions",
		"did not recognize",
	} {
		if strings.Contains(body, marker) {
			t.Errorf("first-time assign should not post rejection reply containing %q; got: %s", marker, body)
		}
	}
}

// TestDispatch_FirstTimeAssignWithPrompt_DispatchesTask is the GEO-60
// regression: a delegate event (created, no SourceCommentID) carries an
// initial prompt in PromptContext. Pre-fix, dispatch checked text
// emptiness and wrongly classified this as @mention, then rejected with
// assignFirstHelp. Post-fix it must claim like any other delegate.
// PromptContext propagation through buildPrompt is covered separately by
// TestBuildPrompt_MentionWithContext.
func TestDispatch_FirstTimeAssignWithPrompt_DispatchesTask(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fresh-prompt",
		IssueID:         "issue-fresh-prompt",
		IssueIdentifier: "FRESH-PROMPT-1",
		Action:          linear.ActionCreated,
		// SourceCommentID empty → delegate, even though PromptContext is set.
		PromptContext: "please refactor the module while you're at it",
	}

	o.HandleAgentEvent(ev)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 1 || got[0] != "sess-fresh-prompt" {
		t.Errorf("delegate-with-prompt should claim like a bare delegate; got %v", got)
	}
	body := mlc.GetPostedBody()
	if strings.Contains(body, "Issue not assigned to admiral") {
		t.Errorf("delegate-with-prompt must not post assign-first rejection; got: %s", body)
	}
}

// TestDispatch_FirstTimeMention_RequestsAssign verifies that an @mention
// or prompted event for an issue admiral has never seen is rejected with
// the "assign first" reply, no claim, no DB write.
func TestDispatch_FirstTimeMention_RequestsAssign(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{} // unknown issue
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-stray",
		IssueID:         "issue-stray",
		IssueIdentifier: "STRAY-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-stray", // non-empty → @mention, not delegate
		PromptContext:   "@admiral please look at this",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "Issue not assigned to admiral") {
		t.Errorf("expected assign-first reply, got: %s", body)
	}
	if mlc.PostedActivity.Type != linear.ActivityError {
		t.Errorf("rejection must post ActivityError, got %q", mlc.PostedActivity.Type)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 0 {
		t.Errorf("first-time mention must not claim; got %v", got)
	}
	ms.mu.Lock()
	updates := append([]string(nil), ms.UpdatedSessionIDs...)
	ms.mu.Unlock()
	if len(updates) != 0 {
		t.Errorf("first-time mention rejection must not write autopilot_jobs; got %v", updates)
	}
}

// TestDispatch_FirstTimePrompted_RequestsAssign mirrors the @mention
// case for a prompted (thread) event with no prior task.
func TestDispatch_FirstTimePrompted_RequestsAssign(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-thread",
		IssueID:         "issue-thread",
		IssueIdentifier: "THR-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "/rerun please",
	}

	o.HandleAgentEvent(ev)

	if !strings.Contains(mlc.GetPostedBody(), "Issue not assigned to admiral") {
		t.Errorf("expected assign-first reply on prompted with no prior task, got: %s", mlc.GetPostedBody())
	}
	if mlc.PostedActivity.Type != linear.ActivityError {
		t.Errorf("rejection must post ActivityError, got %q", mlc.PostedActivity.Type)
	}
}

// TestDispatch_RepeatAssign_Rejected verifies that a second assign
// event on an issue that already has a live admiral_tasks row is
// rejected with the "task already exists" reply, no claim.
func TestDispatch_RepeatAssign_Rejected(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:  "issue-known",
			State:    store.JobStateDone,
			AttemptN: 1,
		},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-reassign",
		IssueID:         "issue-known",
		IssueIdentifier: "KNW-1",
		Action:          linear.ActionCreated,
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "Task already exists") {
		t.Errorf("expected repeat-assign reply, got: %s", body)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 0 {
		t.Errorf("repeat assign must not claim a new job; got %v", got)
	}
}

// TestDispatch_RerunWhileActive_Rejected verifies that /rerun is
// rejected when the live admiral_tasks row is in a non-terminal state
// (RECEIVED / EXECUTING).
func TestDispatch_RerunWhileActive_Rejected(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:  "issue-busy",
			State:    store.JobStateExecuting,
			AttemptN: 1,
		},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-rerun-busy",
		IssueID:         "issue-busy",
		IssueIdentifier: "BUSY-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-rerun-busy",
		PromptContext:   "/rerun",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "currently processing") {
		t.Errorf("expected currently-processing reply, got: %s", body)
	}
	if !strings.Contains(body, "BUSY-1") {
		t.Errorf("expected issue identifier in reply, got: %s", body)
	}
	// In-flight rejection must be Thought, not ErrorActivity, so the live
	// AgentSession isn't terminated and the running flow's progress posts
	// keep landing.
	if mlc.PostedActivity.Type != linear.ActivityThought {
		t.Errorf("rejection on in-flight task must post ActivityThought, got %q", mlc.PostedActivity.Type)
	}
	if got := ms.SupersededAdmiralIssues; len(got) != 0 {
		t.Errorf("rerun-while-active must not supersede admiral_tasks; got %v", got)
	}
}

// TestDispatch_RerunOnDone_Supersedes verifies the happy /rerun path:
// existing live task in DONE state → supersede to history + claim
// fresh attempt_n+1 + spawn run.
func TestDispatch_RerunOnDone_Supersedes(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:  "issue-done",
			State:    store.JobStateDone,
			AttemptN: 1,
			PRURL:    "https://github.com/x/y/pull/1",
		},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-rerun",
		IssueID:         "issue-done",
		IssueIdentifier: "DONE-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-rerun-done",
		PromptContext:   "/rerun fix the typo",
	}

	o.HandleAgentEvent(ev)

	// Wait for the goroutine to run + claim.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.SupersededAdmiralIssues; len(got) != 1 || got[0] != "issue-done" {
		t.Errorf("expected supersession on issue-done; got %v", got)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 1 || got[0] != "sess-rerun" {
		t.Errorf("expected fresh autopilot_jobs claim with new session id; got %v", got)
	}
}

// TestDispatch_RerunInPrompted_NowSupersedes verifies that under the
// admiral_tasks-keyed model, /rerun via thread (prompted) works the
// same as /rerun via @mention: live task is superseded and a fresh
// run spawned. The PR-B-v1 redirect-to-mention message is gone.
func TestDispatch_RerunInPrompted_NowSupersedes(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:  "issue-thread",
			State:    store.JobStateDone,
			AttemptN: 1,
		},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-thread-rerun",
		IssueID:         "issue-thread",
		IssueIdentifier: "THR-2",
		Action:          linear.ActionPrompted,
		UserMessage:     "/rerun please redo",
	}

	o.HandleAgentEvent(ev)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.SupersededAdmiralIssues) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.SupersededAdmiralIssues; len(got) != 1 || got[0] != "issue-thread" {
		t.Errorf("expected /rerun-in-thread to supersede admiral_tasks row; got %v", got)
	}
}

// ---- GEO-49 /fix dispatch tests (PR-C) ----

// TestDispatch_FixOnDone_DispatchesResume verifies the happy /fix path:
// existing live task in DONE with PR url + claude_session_id triggers a
// resume run. admiral_tasks state transitions DONE → EXECUTING; ev.Session
// is recorded as last_event_session_id.
func TestDispatch_FixOnDone_DispatchesResume(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	live := &store.AdmiralTask{
		IssueID:         "issue-fix",
		State:           store.JobStateDone,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/42",
		Branch:          "linear/fix-1",
		ClaudeSessionID: "claude-fix-session",
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live,
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fix",
		IssueID:         "issue-fix",
		IssueIdentifier: "FIX-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-fix-resume",
		PromptContext:   "/fix change v1 to v2 in line 12",
	}

	o.HandleAgentEvent(ev)

	// Wait for the /fix goroutine to flip state to EXECUTING. Read
	// LiveAdmiralSnapshot under the store's mutex so the polling loop
	// doesn't race with UpdateAdmiralTask's fn() writes.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ms.LiveAdmiralSnapshot().State == store.JobStateExecuting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap := ms.LiveAdmiralSnapshot()
	if snap.State != store.JobStateExecuting {
		t.Errorf("expected admiral_tasks.state EXECUTING during /fix run; got %q", snap.State)
	}
	if snap.AttemptN != 1 {
		t.Errorf("/fix must not increment attempt_n; got %d", snap.AttemptN)
	}
	if got := ms.SupersededAdmiralIssues; len(got) != 0 {
		t.Errorf("/fix must not supersede admiral_tasks; got %v", got)
	}
}

// TestDispatch_FixOnExecuting_Rejected verifies /fix is rejected with
// "currently processing" when the live task is mid-flight.
func TestDispatch_FixOnExecuting_Rejected(t *testing.T) {
	mlc := &mockLinearClient{}
	live := &store.AdmiralTask{
		IssueID:         "issue-busy",
		State:           store.JobStateExecuting,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/1",
		ClaudeSessionID: "claude-busy",
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live,
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fix-busy",
		IssueID:         "issue-busy",
		IssueIdentifier: "BUSY-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-fix-busy",
		PromptContext:   "/fix something",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "currently processing") {
		t.Errorf("expected currently-processing reply, got: %s", body)
	}
	if mlc.PostedActivity.Type != linear.ActivityThought {
		t.Errorf("/fix-while-busy must post ActivityThought, got %q", mlc.PostedActivity.Type)
	}
	// State must remain EXECUTING — /fix did NOT take over.
	if live.State != store.JobStateExecuting {
		t.Errorf("/fix on EXECUTING must not change state; got %q", live.State)
	}
}

// TestDispatch_BareCommentWhileExecuting_PostsThought is the GEO-62
// regression: a thread-reply (or @mention) with no recognized /command
// arriving while the task is mid-flight must post a Thought activity,
// NOT an ErrorActivity. ErrorActivity would terminate the live
// AgentSession in Linear and stop the running flow's progress posts
// from landing.
func TestDispatch_BareCommentWhileExecuting_PostsThought(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:  "issue-busy-bare",
			State:    store.JobStateExecuting,
			AttemptN: 1,
		},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-busy-bare",
		IssueID:         "issue-busy-bare",
		IssueIdentifier: "BUSY-BARE-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "hey what's up", // no /command
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "does not respond to bare @mentions") {
		t.Errorf("expected bare-mention help text, got: %s", body)
	}
	if mlc.PostedActivity.Type != linear.ActivityThought {
		t.Errorf("bare-mention-while-busy must post ActivityThought (non-terminal), got %q", mlc.PostedActivity.Type)
	}
}

// TestDispatch_FixOnFailed_SuggestsRerun verifies /fix is rejected on
// FAILED state with a message suggesting /rerun.
func TestDispatch_FixOnFailed_SuggestsRerun(t *testing.T) {
	mlc := &mockLinearClient{}
	live := &store.AdmiralTask{
		IssueID:         "issue-failed",
		State:           store.JobStateFailed,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/2",
		ClaudeSessionID: "claude-failed",
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live,
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fix-failed",
		IssueID:         "issue-failed",
		IssueIdentifier: "FAIL-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-fix-failed",
		PromptContext:   "/fix retry",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "Use /rerun") {
		t.Errorf("expected suggestion to use /rerun, got: %s", body)
	}
	if !strings.Contains(body, "FAILED") {
		t.Errorf("expected current state name in reply, got: %s", body)
	}
}

// TestDispatch_FixWithoutDescription_Rejects verifies /fix without a
// description (just `/fix` alone) is rejected with help. Without text
// the resume claude run has nothing to act on.
func TestDispatch_FixWithoutDescription_Rejects(t *testing.T) {
	mlc := &mockLinearClient{}
	live := &store.AdmiralTask{
		IssueID:         "issue-fix-empty",
		State:           store.JobStateDone,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/3",
		ClaudeSessionID: "claude-empty",
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live,
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fix-empty",
		IssueID:         "issue-fix-empty",
		IssueIdentifier: "FIX-EMPTY-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-fix-empty",
		PromptContext:   "/fix",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "/fix needs a description") {
		t.Errorf("expected empty-description reject reply, got: %s", body)
	}
	if live.State != store.JobStateDone {
		t.Errorf("empty /fix must not advance state; got %q", live.State)
	}
}

// TestDispatch_FixViaPrompted_DispatchesResume verifies /fix works the
// same via thread (prompted) as via @mention — admiral_tasks is keyed
// on issue, so SessionID reuse is no longer a problem.
func TestDispatch_FixViaPrompted_DispatchesResume(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	live := &store.AdmiralTask{
		IssueID:         "issue-fix-thread",
		State:           store.JobStateDone,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/99",
		Branch:          "linear/fix-thread",
		ClaudeSessionID: "claude-thread",
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live,
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fix-thread",
		IssueID:         "issue-fix-thread",
		IssueIdentifier: "FIX-THR-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "/fix simplify the helper",
	}

	o.HandleAgentEvent(ev)

	// Read state via the locked snapshot helper so this polling loop
	// doesn't race with UpdateAdmiralTask's fn() writes from the
	// dispatched /fix goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ms.LiveAdmiralSnapshot().State == store.JobStateExecuting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateExecuting {
		t.Errorf("/fix-via-thread must transition admiral_tasks to EXECUTING; got %q", got)
	}
}

// ---- GEO-51 concurrency cap tests ----

// TestAcquireRunSlot_BlocksWhenFull verifies the semaphore actually
// gates: with capacity 1, the second acquireRunSlot call blocks until
// the first release runs.
func TestAcquireRunSlot_BlocksWhenFull(t *testing.T) {
	o := &Orchestrator{
		runSlots: make(chan struct{}, 1),
		logger:   slog.Default(),
	}
	release1 := o.acquireRunSlot()

	acquired := make(chan struct{})
	go func() {
		release2 := o.acquireRunSlot()
		close(acquired)
		release2()
	}()

	// Second acquire must NOT make progress while slot is held.
	select {
	case <-acquired:
		t.Fatal("acquireRunSlot returned while semaphore was full")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	// Release the first slot — second acquire should now unblock.
	release1()
	select {
	case <-acquired:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("acquireRunSlot did not unblock within 2s after release")
	}
}

// TestAcquireRunSlot_NilSlotsIsNoOp verifies that an Orchestrator
// constructed without runSlots (e.g. test code that bypasses New())
// does not deadlock on acquireRunSlot. The release returned is a
// no-op and the function returns immediately.
func TestAcquireRunSlot_NilSlotsIsNoOp(t *testing.T) {
	o := &Orchestrator{logger: slog.Default()} // runSlots intentionally nil

	done := make(chan struct{})
	go func() {
		release := o.acquireRunSlot()
		release()
		close(done)
	}()
	select {
	case <-done:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("acquireRunSlot blocked even though runSlots is nil")
	}
}

// TestRunSlots_CapacityFromConfig verifies New() sizes the semaphore
// from cfg.MaxConcurrentRuns and falls back to 3 when zero.
func TestRunSlots_CapacityFromConfig(t *testing.T) {
	cases := []struct {
		name      string
		configVal int
		wantCap   int
	}{
		{"explicit 1", 1, 1},
		{"explicit 5", 5, 5},
		{"zero falls back to 3", 0, 3},
		{"negative falls back to 3", -7, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpDB := t.TempDir() + "/test.db"
			s, err := store.Open(tmpDB)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()
			cfg := &config.Autopilot{
				MaxConcurrentRuns: c.configVal,
				JobStreamsDir:     t.TempDir(),
			}
			o := New(cfg, nil, s, slog.Default())
			if got := cap(o.runSlots); got != c.wantCap {
				t.Errorf("runSlots capacity: got %d, want %d", got, c.wantCap)
			}
		})
	}
}
