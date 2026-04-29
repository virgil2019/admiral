package autopilot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	PostedBody string
	PostErr    error
}

func (m *mockLinearClient) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	m.PostedBody = a.Body
	return m.PostErr
}

func (m *mockLinearClient) GetIssue(ctx context.Context, id string) (*linear.Issue, error) {
	return nil, nil
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
}

func (m *mockStore) AnyAutopilotJobActive() (bool, string, error) {
	return m.Active, m.ActiveSID, m.ActiveErr
}

func (m *mockStore) GetLastAutopilotJob() (*store.AutopilotJob, error) {
	return m.LastJob, m.LastJobErr
}

func (m *mockStore) GetAutopilotJob(sessionID string) (*store.AutopilotJob, error) {
	return m.GetJob, m.GetJobErr
}

func (m *mockStore) UpdateAutopilotJob(sessionID string, fn func(*store.AutopilotJob)) error {
	return nil
}

func (m *mockStore) ClaimAutopilotJob(sessionID, issueID, identifier string) (bool, error) {
	return true, nil
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
