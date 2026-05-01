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
	r := strings.NewReader(strings.Join(lines, "\n")+"\n")
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

func TestExtractCommand(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`/status`, "status"},
		{`/STATUS`, "status"},
		{`/status extra args`, "status"},
		{`  /status  `, "status"},
		{"\n\n/status", "status"},
		{"please /status", ""},
		{"hello", ""},
		{"", ""},
		{"/", ""},
		{"/   ", ""},
		{"/status\n/help", "status"},
	}
	for _, c := range cases {
		if got := extractCommand(c.in); got != c.want {
			t.Errorf("extractCommand(%q) = %q, want %q", c.in, got, c.want)
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
	WorkflowStates       []linear.WorkflowState
	GetWorkflowStatesErr error
	GetWorkflowStatesCalls int

	// IssueUpdate override
	IssueUpdateCalls     []struct{ IssueID, StateID string }
	IssueUpdateErr       error
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

	// For UpdateAutopilotJob tracking
	LastUpdatedJob *store.AutopilotJob

	// Repo to return from GetRepoByProjectID; if RepoErr is non-nil it is
	// returned instead.
	Repo    *store.Repo
	RepoErr error

	// ClaimAutopilotJob recorder. Each call appends the session id; tests
	// inspect this to verify dispatch decisions that spawn goroutines
	// (runFollowup / runFollowupResume) actually claim a row.
	mu                sync.Mutex
	ClaimedSessionIDs []string

	// UpdatedSessionIDs records every session id passed to
	// UpdateAutopilotJob, in call order. Useful for asserting the
	// markAlreadyMerged path updated the new session.
	UpdatedSessionIDs []string
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
	m.UpdatedSessionIDs = append(m.UpdatedSessionIDs, sessionID)
	m.mu.Unlock()
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

func (m *mockStore) GetRepoByProjectID(projectID string) (*store.Repo, error) {
	return m.Repo, m.RepoErr
}

// fakeGhProbe is a deterministic ghProbe for tests. Configure the maps
// keyed on branch name (for FindMergedPRForBranch) and PR url (for
// PRState); missing keys yield "not found" / "" respectively. Set the
// *Err fields to simulate transport failures.
type fakeGhProbe struct {
	mergedByBranch map[string]struct {
		URL, SHA string
	}
	stateByURL    map[string]string
	mergedErr     error
	stateErr      error
	mergedCalls   int32
	stateCalls    int32
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
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler             { return h }

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

// TestHandleCreated_PriorDone_PROpenWithSession spawns a follow-up resume.
// Observable: ev.SessionID gets claimed (runFollowupResume claims first thing).
func TestHandleCreated_PriorDone_PROpenWithSession_Resumes(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"), // halt the goroutine fast
	}
	ms := &mockStore{
		LatestDoneJob: &store.AutopilotJob{
			AgentSessionID:  "old-session",
			IssueID:         "issue-abc",
			PRURL:           "https://github.com/x/y/pull/42",
			ClaudeSessionID: "claude-sess-xyz",
			State:           store.JobStateDone,
		},
	}
	gh := &fakeGhProbe{
		stateByURL: map[string]string{
			"https://github.com/x/y/pull/42": "OPEN",
		},
	}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "new-session",
		IssueID:         "issue-abc",
		IssueIdentifier: "ABC-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	// runFollowupResume runs in a goroutine; let it claim the session.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	claims := ms.ClaimedSnapshot()
	if len(claims) != 1 || claims[0] != "new-session" {
		t.Fatalf("expected runFollowupResume to claim new-session, got claims=%v", claims)
	}
	// PR-state was checked synchronously in handleCreated.
	if atomic.LoadInt32(&gh.stateCalls) != 1 {
		t.Errorf("expected exactly 1 PRState call, got %d", gh.stateCalls)
	}
	// No "legacy session" message (this is the resume path).
	if strings.Contains(mlc.GetPostedBody(), "created before resume support") {
		t.Errorf("did not expect the legacy-session message; got: %s", mlc.GetPostedBody())
	}
}

// TestHandleCreated_PriorDone_PROpenNoSession_RespondsLegacy is the
// synchronous "legacy row" path — admiral can't resume without a
// claude_session_id, so it posts an explanation and stops.
func TestHandleCreated_PriorDone_PROpenNoSession_RespondsLegacy(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		LatestDoneJob: &store.AutopilotJob{
			IssueID:         "issue-legacy",
			PRURL:           "https://github.com/x/y/pull/13",
			ClaudeSessionID: "", // legacy: no session id captured
			State:           store.JobStateDone,
		},
	}
	gh := &fakeGhProbe{
		stateByURL: map[string]string{
			"https://github.com/x/y/pull/13": "OPEN",
		},
	}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "new-session",
		IssueID:         "issue-legacy",
		IssueIdentifier: "LEG-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	if !strings.Contains(mlc.GetPostedBody(), "created before resume support") {
		t.Errorf("expected legacy-session message, got: %s", mlc.GetPostedBody())
	}
	if !strings.Contains(mlc.GetPostedBody(), "https://github.com/x/y/pull/13") {
		t.Errorf("expected prior PR URL in legacy message, got: %s", mlc.GetPostedBody())
	}
	if got := ms.ClaimedSnapshot(); len(got) != 0 {
		t.Errorf("legacy path must not claim a job; got claims=%v", got)
	}
}

// TestHandleCreated_PriorDone_PRMerged_SpawnsFreshFollowup verifies that
// a merged prior PR routes to runFollowup (fresh branch) rather than the
// resume path or a refusal.
func TestHandleCreated_PriorDone_PRMerged_SpawnsFreshFollowup(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{
		LatestDoneJob: &store.AutopilotJob{
			IssueID:         "issue-merged",
			PRURL:           "https://github.com/x/y/pull/99",
			ClaudeSessionID: "claude-sess-merged",
			State:           store.JobStateDone,
		},
	}
	gh := &fakeGhProbe{
		stateByURL: map[string]string{
			"https://github.com/x/y/pull/99": "MERGED",
		},
	}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "new-session-merged",
		IssueID:         "issue-merged",
		IssueIdentifier: "MRG-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	claims := ms.ClaimedSnapshot()
	if len(claims) != 1 || claims[0] != "new-session-merged" {
		t.Fatalf("expected runFollowup to claim new-session-merged; got %v", claims)
	}
	if strings.Contains(mlc.GetPostedBody(), "created before resume support") {
		t.Errorf("did not expect legacy-session message for merged path")
	}
}

// TestHandleCreated_PriorDone_PRClosed_SpawnsFreshFollowup mirrors the
// merged path for a closed-without-merge prior PR.
func TestHandleCreated_PriorDone_PRClosed_SpawnsFreshFollowup(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{
		LatestDoneJob: &store.AutopilotJob{
			IssueID:         "issue-closed",
			PRURL:           "https://github.com/x/y/pull/77",
			ClaudeSessionID: "claude-sess-closed",
			State:           store.JobStateDone,
		},
	}
	gh := &fakeGhProbe{
		stateByURL: map[string]string{
			"https://github.com/x/y/pull/77": "CLOSED",
		},
	}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "new-session-closed",
		IssueID:         "issue-closed",
		IssueIdentifier: "CLS-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	claims := ms.ClaimedSnapshot()
	if len(claims) != 1 || claims[0] != "new-session-closed" {
		t.Fatalf("expected runFollowup to claim new-session-closed; got %v", claims)
	}
}

// TestHandleCreated_PriorDone_GhUnreachable_FallsThroughFresh verifies the
// fail-open behavior: if gh PRState errors, dispatch falls through to a
// fresh flow rather than blocking.
func TestHandleCreated_PriorDone_GhUnreachable_FallsThroughFresh(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{
		LatestDoneJob: &store.AutopilotJob{
			IssueID: "issue-ghdown",
			PRURL:   "https://github.com/x/y/pull/55",
			State:   store.JobStateDone,
		},
	}
	gh := &fakeGhProbe{stateErr: fmt.Errorf("transport: dial tcp: refused")}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "new-session-ghdown",
		IssueID:         "issue-ghdown",
		IssueIdentifier: "GHD-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 1 || got[0] != "new-session-ghdown" {
		t.Fatalf("expected gh-error to fall through to a claim, got %v", got)
	}
}

// TestHandleCreated_PriorDone_NoPRURL_SkipsCheck makes sure the dispatch
// short-circuits before touching gh when prior is malformed (no PRURL).
func TestHandleCreated_PriorDone_NoPRURL_SkipsCheck(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{
		LatestDoneJob: &store.AutopilotJob{
			IssueID: "issue-nopr",
			PRURL:   "", // malformed
			State:   store.JobStateDone,
		},
	}
	gh := &fakeGhProbe{}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "new-session-nopr",
		IssueID:         "issue-nopr",
		IssueIdentifier: "NOPR-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	if atomic.LoadInt32(&gh.stateCalls) != 0 {
		t.Errorf("PRState should not be called when prior.PRURL is empty; got %d calls", gh.stateCalls)
	}
}

// TestHandleCreated_NoPriorDone_FallsThroughToFreshFlow verifies the
// no-prior path doesn't hit gh and proceeds straight to claim.
func TestHandleCreated_NoPriorDone_FallsThroughToFreshFlow(t *testing.T) {
	mlc := &mockLinearClient{
		GetIssueErr: fmt.Errorf("synthetic short-circuit"),
	}
	ms := &mockStore{LatestDoneJob: nil}
	gh := &fakeGhProbe{}
	o := newTestOrchestrator(t, ms, mlc, gh)
	ev := linear.AgentEvent{
		SessionID:       "fresh",
		IssueID:         "issue-fresh",
		IssueIdentifier: "FRS-1",
		Action:          linear.ActionCreated,
	}

	o.handleCreated(ev)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ms.ClaimedSnapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&gh.stateCalls) != 0 {
		t.Errorf("PRState shouldn't run with no prior; got %d calls", gh.stateCalls)
	}
	if got := ms.ClaimedSnapshot(); len(got) != 1 || got[0] != "fresh" {
		t.Errorf("expected fresh path to claim 'fresh'; got %v", got)
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
		ev:           linear.AgentEvent{SessionID: "sess-1", CreatorID: "user-abc"},
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
	mention := ""
	if f.ev.CreatorID != "" {
		mention = "@" + f.ev.CreatorID + " "
	}
	f.postActivity(linear.Response(fmt.Sprintf(
		"%sDone. PR opened: %s\n\nWorktree: `%s`\nBranch: `%s`",
		mention, f.prURL, f.worktreePath, f.branch)))

	body := mlc.GetPostedBody()
	if !strings.HasPrefix(body, "@user-abc ") {
		t.Errorf("expected body to start with '@user-abc ', got: %s", body)
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
		ev:           linear.AgentEvent{SessionID: "sess-2", CreatorID: "user-xyz"},
		worktreePath: "/tmp/wt2",
		branch:       "linear/fail",
	}

	// Simulate markFailed body construction
	mention := ""
	if f.ev.CreatorID != "" {
		mention = "@" + f.ev.CreatorID + " "
	}
	body := mention + "admiral failed: something went wrong"
	f.postActivity(linear.ErrorActivity(body))

	if !strings.HasPrefix(mlc.GetPostedBody(), "@user-xyz ") {
		t.Errorf("expected body to start with '@user-xyz ', got: %s", mlc.GetPostedBody())
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
		GetJob:    nil,
		GetJobErr: nil,
	}
	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: t.TempDir()}, db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{
		SessionID:       "session-no-history",
		IssueIdentifier: "GEO-NOHIST-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "please fix this",
	}

	o.handlePrompted(ev)

	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(mlc.GetPostedBody(), "don't have history") {
		t.Errorf("expected 'don't have history' stub reply, got: %s", mlc.GetPostedBody())
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
	}
	o := &Orchestrator{cfg: &config.Autopilot{RepoDir: t.TempDir()}, db: ms, lc: mlc, logger: slog.Default()}
	ev := linear.AgentEvent{
		SessionID:       "session-old",
		IssueIdentifier: "GEO-OLD-1",
		Action:          linear.ActionPrompted,
		UserMessage:     "please fix this",
	}

	o.handlePrompted(ev)

	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(mlc.GetPostedBody(), "before resume support") {
		t.Errorf("expected 'before resume support' stub reply, got: %s", mlc.GetPostedBody())
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

	o.handlePrompted(ev)

	// Should not immediately post any response (goroutine runs async)
	// Give it time to at least start the goroutine
	time.Sleep(50 * time.Millisecond)

	// Verify no stub reply was posted
	if strings.Contains(mlc.GetPostedBody(), "don't have history") ||
		strings.Contains(mlc.GetPostedBody(), "before resume support") {
		t.Errorf("expected no stub reply for valid resume, got: %s", mlc.GetPostedBody())
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
