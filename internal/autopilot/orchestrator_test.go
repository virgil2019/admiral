package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	if !strings.Contains(p, "linear/tst-1") {
		t.Errorf("missing branch reference in operating procedure:\n%s", p)
	}
	// admiral hard gate (#161 first-run slice): the prompt MUST tell claude to
	// not push and not open a PR — admiral owns those after the build gate.
	if !strings.Contains(p, "Do NOT run `git push`") {
		t.Errorf("operating procedure should forbid `git push`:\n%s", p)
	}
	if !strings.Contains(p, "do NOT run `gh pr create`") {
		t.Errorf("operating procedure should forbid `gh pr create`:\n%s", p)
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
		{"/rerun\u00a0use v2", "rerun", "use v2", true},
		{"/rerun\tuse v2", "rerun", "use v2", true},
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

func TestParseUserRequest(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		wantText      string
		wantMentioned bool
		wantErr       error
		wantCmd       string // parseMentionCommand name on wantText; "" = bare
		wantRem       string
	}{
		{
			name:     "linear context bundle with /rerun",
			in:       "<issue identifier=\"GEO-421\">\n<title>T</title>\n</issue>\n<comment author=\"a\">\nhello\n</comment>\n<user-request>\n@claudeagent /rerun\n</user-request>\n",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "double space after handle",
			in:       "<issue>x</issue>\n\n<user-request>\n@claudeagent  /rerun\n</user-request>\n",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "bundle with notes",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent /rerun use the v2 schema\n</user-request>",
			wantText: "/rerun use the v2 schema", wantMentioned: true, wantCmd: "rerun", wantRem: "use the v2 schema",
		},
		{
			name:     "mention on its own line",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent\n/fix broken test\n</user-request>",
			wantText: "/fix broken test", wantMentioned: true, wantCmd: "fix", wantRem: "broken test",
		},
		{
			name:     "delegate prompt has no handle",
			in:       "<issue>x</issue>\n<user-request>\nWork on [GEO-421](https://linear.app/x/issue/GEO-421/y)\n</user-request>\n",
			wantText: "Work on [GEO-421](https://linear.app/x/issue/GEO-421/y)", wantMentioned: false,
		},
		{
			name:     "inline tags in an earlier comment are ignored",
			in:       "<comment>\n<user-request>/fix old</user-request>\n</comment>\n<user-request>\n@claudeagent /rerun\n</user-request>",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "inline tags typed in the request are kept",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent /fix parse the <user-request> and </user-request> tags\n</user-request>\n",
			wantText: "/fix parse the <user-request> and </user-request> tags", wantMentioned: true,
			wantCmd: "fix", wantRem: "parse the <user-request> and </user-request> tags",
		},
		{
			name:     "envelope pasted inside the request is ambiguous",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent /fix parser breaks on:\n```\n<user-request>\n/rerun\n</user-request>\n```\n</user-request>\n",
			wantText: "", wantErr: errUserRequestUnparsed,
		},
		{
			name:     "unpaired open tag in an earlier comment does not wedge the issue",
			in:       "<comment>\nsnippet:\n<user-request>\n</comment>\n<user-request>\n@claudeagent /rerun\n</user-request>\n",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "non-breaking space after the handle",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent\u00a0/rerun\n</user-request>\n",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "complete envelope pasted in an earlier comment is fine",
			in:       "<comment>\nlook:\n<user-request>\n/old\n</user-request>\n</comment>\n<user-request>\n@claudeagent /rerun\n</user-request>\n",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "tag lines with surrounding whitespace",
			in:       "<issue>x</issue>\n<user-request> \r\n@claudeagent /rerun\r\n </user-request>\n",
			wantText: "/rerun", wantMentioned: true, wantCmd: "rerun",
		},
		{
			name:     "malformed envelope never yields the bundle",
			in:       "<issue>x</issue>\n<user-request id=\"1\">\n@claudeagent use option B\n</user-request>",
			wantText: "", wantErr: errUserRequestUnparsed,
		},
		{
			name:     "single-line envelope never yields the bundle",
			in:       "<issue>x</issue>\n<user-request>@claudeagent /rerun</user-request>",
			wantText: "", wantErr: errUserRequestUnparsed,
		},
		{
			name:     "only the leading handle is stripped",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent @alice should decide\n</user-request>",
			wantText: "@alice should decide", wantMentioned: true,
		},
		{
			name:     "bundle with bare chat stays bare",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent 随便聊聊\n</user-request>",
			wantText: "随便聊聊", wantMentioned: true,
		},
		{
			name:     "handle only yields empty text",
			in:       "<issue>x</issue>\n<user-request>\n@claudeagent\n</user-request>",
			wantText: "", wantMentioned: true,
		},
		{
			name:     "empty request yields empty text",
			in:       "<issue>x</issue>\n<user-request>\n</user-request>",
			wantText: "",
		},
		{
			name:     "no envelope: not ok, text unchanged",
			in:       "/rerun extra",
			wantText: "/rerun extra", wantErr: errNoUserRequest, wantCmd: "rerun", wantRem: "extra",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, mentioned, err := parseUserRequest(c.in)
			if got != c.wantText || mentioned != c.wantMentioned || !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Fatalf("parseUserRequest = (%q, %v, %v), want (%q, %v, %v)",
					got, mentioned, err, c.wantText, c.wantMentioned, c.wantErr)
			}
			name, rem, cmdOK := parseMentionCommand(got)
			if name != c.wantCmd || rem != c.wantRem || cmdOK != (c.wantCmd != "") {
				t.Fatalf("parseMentionCommand(%q) = (%q, %q, %v), want (%q, %q)",
					got, name, rem, cmdOK, c.wantCmd, c.wantRem)
			}
		})
	}
}

func TestStripLeadingHandle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"@claudeagent /rerun", "/rerun"},
		{"  @claudeagent\n/rerun", "/rerun"},
		{"/rerun", "/rerun"},
		{"@claudeagent", ""},
		{"hello @claudeagent", "hello @claudeagent"},
	}
	for _, c := range cases {
		if got := stripLeadingHandle(c.in); got != c.want {
			t.Errorf("stripLeadingHandle(%q) = %q, want %q", c.in, got, c.want)
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

	// Verify loop (C4) overrides.
	SubIssues         []linear.SubIssue
	SubIssuesErr      error
	SubIssuesByParent map[string][]linear.SubIssue // per-parent override (cascade tests)
	IssueCreateResult *linear.Issue
	IssueCreateErr    error
	IssueCreateInputs []linear.IssueCreateInput
	TeamLabelID       string
	TeamLabelIDErr    error
	CreatedComments   []struct{ IssueID, Body string }
	CreateCommentErr  error
	GetIssueByID      map[string]*linear.Issue // per-id GetIssue override (sub titles)

	// Cascade walk (GetParentID) override.
	ParentByChild  map[string]string
	GetParentIDErr error

	// RemoveIssueLabel recorder (/reset)
	RemovedLabels       []struct{ IssueID, LabelID string }
	RemoveIssueLabelErr error

	// UnassignIssue recorder (/reset)
	UnassignedIssues []string
	UnassignIssueErr error

	// Product verify overrides.
	TopLevelIssues    []linear.Issue
	TopLevelIssuesErr error
	ProjectTeamID     string
	ProjectTeamIDErr  error
}

func (m *mockLinearClient) RemoveIssueLabel(_ context.Context, issueID, labelID string) error {
	m.RemovedLabels = append(m.RemovedLabels, struct{ IssueID, LabelID string }{issueID, labelID})
	return m.RemoveIssueLabelErr
}

func (m *mockLinearClient) UnassignIssue(_ context.Context, issueID string) error {
	m.UnassignedIssues = append(m.UnassignedIssues, issueID)
	return m.UnassignIssueErr
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
	if iss, ok := m.GetIssueByID[id]; ok {
		return iss, nil
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

func (m *mockLinearClient) GetSubIssues(_ context.Context, parentID string) ([]linear.SubIssue, error) {
	if m.SubIssuesErr != nil {
		return nil, m.SubIssuesErr
	}
	if m.SubIssuesByParent != nil {
		if subs, ok := m.SubIssuesByParent[parentID]; ok {
			return subs, nil
		}
		return nil, nil
	}
	return m.SubIssues, nil
}

func (m *mockLinearClient) GetParentID(_ context.Context, childID string) (string, error) {
	if m.GetParentIDErr != nil {
		return "", m.GetParentIDErr
	}
	return m.ParentByChild[childID], nil
}

func (m *mockLinearClient) IssueCreate(_ context.Context, in linear.IssueCreateInput) (*linear.Issue, error) {
	m.IssueCreateInputs = append(m.IssueCreateInputs, in)
	if m.IssueCreateErr != nil {
		return nil, m.IssueCreateErr
	}
	if m.IssueCreateResult != nil {
		return m.IssueCreateResult, nil
	}
	return &linear.Issue{ID: "new-" + in.Title, Identifier: "GEO-NEW", Title: in.Title}, nil
}

func (m *mockLinearClient) GetTeamLabelID(_ context.Context, _, _ string) (string, error) {
	return m.TeamLabelID, m.TeamLabelIDErr
}

func (m *mockLinearClient) CreateComment(_ context.Context, issueID, body string) error {
	m.CreatedComments = append(m.CreatedComments, struct{ IssueID, Body string }{issueID, body})
	return m.CreateCommentErr
}

func (m *mockLinearClient) ListProjectTopLevelIssues(_ context.Context, _ string) ([]linear.Issue, error) {
	return m.TopLevelIssues, m.TopLevelIssuesErr
}

func (m *mockLinearClient) GetProjectTeamID(_ context.Context, _ string) (string, error) {
	return m.ProjectTeamID, m.ProjectTeamIDErr
}

// mockStore implements storeInterface for testing.
type mockStore struct {
	// PendingQuestionLookups counts GetPendingQuestionByID calls (the first
	// step of dispatchAwaitingReply).
	PendingQuestionLookups int
	// PendingQuestion, when set, is returned by GetPendingQuestionByID;
	// AnsweredText records what AnswerPendingQuestion stored.
	PendingQuestion *store.PendingQuestion
	AnsweredText    string

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
	AdmiralTask        *store.AdmiralTask
	AdmiralTaskErr     error
	AdmiralTaskErrOnce bool

	// shutdown / reconcile recorders
	InterruptedReason       string
	InterruptedCount        int
	MarkInterruptedErr      error
	MarkedInterruptedIDs    []string
	AutoResumeCount         int
	AutoResumeErr           error
	AutoResumeResets        int
	ClearedPendingQuestion  string
	AutoResumeRefunds       int
	TaskVerifyResets        []string
	ProductVerifyResets     []string
	MarkedUnrecoverable     []string
	MarkUnrecoverableErr    error
	InFlightTasks           []store.AdmiralTask
	ListInFlightErr         error
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
	// returned instead. A repo without GhUser gets the default test account
	// unless RepoNoGhUser is set.
	Repo         *store.Repo
	RepoErr      error
	RepoNoGhUser bool

	// ListJobsByIssueAndStates override
	ListJobsByIssueAndStatesResult []store.AutopilotJob
	ListJobsByIssueAndStatesErr    error

	// CI-watch reconciler override
	WatchableCITasks    []store.AdmiralTask
	WatchableCITasksErr error

	// ClaimAutopilotJob recorder. Each call appends the session id; tests
	// inspect this to verify dispatch decisions that spawn goroutines
	// (runFollowup / runFollowupResume) actually claim a row.
	mu                sync.Mutex
	ClaimedSessionIDs []string

	// UpdatedSessionIDs records every session id passed to
	// UpdateAutopilotJob, in call order. Useful for asserting the
	// markAlreadyMerged path updated the new session.
	UpdatedSessionIDs []string

	// StateTransitions records every admiral_tasks state the mock saw, in
	// order.
	StateTransitions []string

	// Blocker-related fields
	SetBlockedCalls     []string // issueIDs passed to SetAdmiralTaskBlocked
	BlockedTasks        []store.BlockedTask
	TransitionBlockedOK bool

	// task_verifications (C4) overrides.
	TaskVerification        *store.TaskVerification
	TaskVerificationByID    map[string]*store.TaskVerification // per-parent override; preferred over TaskVerification when set
	TaskVerificationErr     error
	BumpedTaskVerification  *store.TaskVerification
	BumpTaskVerificationErr error
	BumpCalls               []string
	SetStatusCalls          []struct{ ParentID, Status string }
	SetStatusErr            error
	SetSummaryCalls         []struct{ ParentID, Summary string }
	SetSummaryErr           error

	// EnqueueEventWithSource recorder (cascade hop in applyVerifyVerdict).
	Enqueued       []enqueuedEvent
	EnqueueFresh   bool
	EnqueueFreshOK bool // when true, EnqueueFresh is the returned freshness; when false, defaults to true
	EnqueueErr     error

	// product_verifications
	ProductVerification        *store.ProductVerification
	ProductVerificationErr     error
	BumpedProductVerification  *store.ProductVerification
	BumpProductVerificationErr error
	ProductBumpCalls           []string
	ProductSetStatusCalls      []struct{ ProjectID, Status string }
	ProductSetStatusErr        error

	// /reset cascade recorders (PR-B)
	ResetIssueRowsCalls       []string
	ResetIssueRowsErr         error
	DeletedTaskVerifications  []string
	DeleteTaskVerificationErr error
}

func (m *mockStore) ResetIssueRows(issueID string) error {
	m.ResetIssueRowsCalls = append(m.ResetIssueRowsCalls, issueID)
	return m.ResetIssueRowsErr
}

func (m *mockStore) DeleteTaskVerification(parentIssueID string) error {
	m.DeletedTaskVerifications = append(m.DeletedTaskVerifications, parentIssueID)
	return m.DeleteTaskVerificationErr
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

// MarkInterruptedTasks / ListInFlightTasks back the shutdown + reconcile path.
func (m *mockStore) MarkInterruptedTasks(reason string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.InterruptedReason = reason
	if m.LiveAdmiralTask != nil && !isTerminalTaskState(m.LiveAdmiralTask.State) &&
		m.LiveAdmiralTask.State != store.JobStateBlocked &&
		m.LiveAdmiralTask.State != store.JobStateAwaitingInput {
		m.LiveAdmiralTask.State = store.JobStateTimedOut
		m.LiveAdmiralTask.Error = reason
		m.StateTransitions = append(m.StateTransitions, store.JobStateTimedOut)
		return 1, nil
	}
	return m.InterruptedCount, m.MarkInterruptedErr
}

func (m *mockStore) MarkTaskInterrupted(issueID string, attemptN int, reason string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.InterruptedReason = reason
	m.MarkedInterruptedIDs = append(m.MarkedInterruptedIDs, issueID)
	if m.MarkInterruptedErr != nil {
		return false, m.MarkInterruptedErr
	}
	return true, nil
}

func (m *mockStore) ResetTaskVerificationRound(parentIssueID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TaskVerifyResets = append(m.TaskVerifyResets, parentIssueID)
	return nil
}

func (m *mockStore) ResetProductVerificationRound(projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ProductVerifyResets = append(m.ProductVerifyResets, projectID)
	return nil
}

func (m *mockStore) RefundAutoResumeCount(issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AutoResumeRefunds++
}

func (m *mockStore) ClearPendingQuestion(issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ClearedPendingQuestion = issueID
	if m.AdmiralTask != nil {
		m.AdmiralTask.PendingQuestionID = ""
	}
}

func (m *mockStore) ResetAutoResumeCount(issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AutoResumeCount = 0
	m.AutoResumeResets++
}

func (m *mockStore) MarkTaskUnrecoverable(issueID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MarkedUnrecoverable = append(m.MarkedUnrecoverable, issueID)
	return m.MarkUnrecoverableErr
}

func (m *mockStore) IncrementAutoResumeCount(issueID string, attemptN int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AutoResumeCount++
	return m.AutoResumeCount, m.AutoResumeErr
}

func (m *mockStore) ListWatchableCITasks() ([]store.AdmiralTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.WatchableCITasks, m.WatchableCITasksErr
}

func (m *mockStore) ListRecoverableTasks() ([]store.AdmiralTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.InFlightTasks != nil || m.ListInFlightErr != nil {
		// Mirror the real query: a row whose escalation was delivered no
		// longer matches, which is what stops the sweep re-posting it.
		skip := make(map[string]bool, len(m.MarkedUnrecoverable))
		for _, id := range m.MarkedUnrecoverable {
			skip[id] = true
		}
		var out []store.AdmiralTask
		for _, t := range m.InFlightTasks {
			if !skip[t.IssueID] {
				out = append(out, t)
			}
		}
		return out, m.ListInFlightErr
	}
	if m.LiveAdmiralTask != nil {
		return []store.AdmiralTask{*m.LiveAdmiralTask}, nil
	}
	return nil, nil
}

func (m *mockStore) GetAdmiralTaskByIssue(issueID string) (*store.AdmiralTask, error) {
	// AdmiralTaskErrOnce simulates one transient read failure (locked
	// sqlite) followed by success — the shape the watcher's retry covers.
	if m.AdmiralTaskErrOnce {
		m.AdmiralTaskErrOnce = false
		return nil, errors.New("database is locked")
	}
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
		m.StateTransitions = append(m.StateTransitions, m.LiveAdmiralTask.State)
	}
	return m.UpdateAdmiralTaskErr
}

// StateTransitionSnapshot returns the admiral_tasks states the mock observed,
// in order. Tests assert on the sequence (e.g. "the /fix run entered
// EXECUTING") rather than sampling the instantaneous state, which races with
// the run goroutine.
func (m *mockStore) StateTransitionSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.StateTransitions)
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
	if m.RepoErr != nil {
		return nil, m.RepoErr
	}
	if m.Repo == nil || m.Repo.GhUser != "" || m.RepoNoGhUser {
		return m.Repo, nil
	}
	// Copy rather than mutate: the same *store.Repo is shared across the
	// mock's callers.
	cp := *m.Repo
	cp.GhUser = defaultTestGhUser
	return &cp, nil
}

func (m *mockStore) ListJobsByIssueAndStates(issueID string, states []string) ([]store.AutopilotJob, error) {
	return m.ListJobsByIssueAndStatesResult, m.ListJobsByIssueAndStatesErr
}

func (m *mockStore) SetAdmiralTaskBlocked(issueID, blockerIDs string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SetBlockedCalls = append(m.SetBlockedCalls, issueID)
	return nil
}

// SetBlockedSnapshot returns a copy of SetBlockedCalls read under m.mu, for
// tests asserting a park that may be driven from a spawned goroutine.
func (m *mockStore) SetBlockedSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.SetBlockedCalls))
	copy(out, m.SetBlockedCalls)
	return out
}

func (m *mockStore) GetBlockedAdmiralTasks() ([]store.BlockedTask, error) {
	return m.BlockedTasks, nil
}

func (m *mockStore) TransitionBlockedToReceived(issueID string) (bool, error) {
	return m.TransitionBlockedOK, nil
}

func (m *mockStore) InsertPendingQuestion(q store.PendingQuestion) error { return nil }
func (m *mockStore) GetOpenPendingQuestionByIssue(issueID string) (*store.PendingQuestion, error) {
	return nil, nil
}
func (m *mockStore) AnswerPendingQuestion(id, answer string) error {
	m.mu.Lock()
	m.AnsweredText = answer
	m.mu.Unlock()
	return nil
}
func (m *mockStore) SetAdmiralTaskAwaitingInput(issueID, pendingQuestionID string) error {
	return nil
}
func (m *mockStore) TransitionAwaitingInputToExecuting(issueID string) (bool, error) {
	return true, nil
}
func (m *mockStore) GetPendingQuestionByID(id string) (*store.PendingQuestion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PendingQuestionLookups++
	return m.PendingQuestion, nil
}
func (m *mockStore) CancelOpenPendingQuestionsForIssue(issueID string) error { return nil }

func (m *mockStore) GetTaskVerification(parentIssueID string) (*store.TaskVerification, error) {
	if m.TaskVerificationErr != nil {
		return nil, m.TaskVerificationErr
	}
	if m.TaskVerificationByID != nil {
		// Map mode is exclusive: an absent key means "no verification for
		// this parent", not "fall through to the singular default" — avoids
		// tests accidentally leaking the singular value to subs it wasn't
		// meant for.
		if tv, ok := m.TaskVerificationByID[parentIssueID]; ok {
			return tv, nil
		}
		return nil, nil
	}
	return m.TaskVerification, nil
}

func (m *mockStore) BumpTaskVerificationRound(parentIssueID string) (*store.TaskVerification, error) {
	m.mu.Lock()
	m.BumpCalls = append(m.BumpCalls, parentIssueID)
	m.mu.Unlock()
	return m.BumpedTaskVerification, m.BumpTaskVerificationErr
}

func (m *mockStore) SetTaskVerificationStatus(parentIssueID, status string) error {
	m.mu.Lock()
	m.SetStatusCalls = append(m.SetStatusCalls, struct{ ParentID, Status string }{parentIssueID, status})
	m.mu.Unlock()
	return m.SetStatusErr
}

func (m *mockStore) SetTaskVerificationSummary(parentIssueID, summary string) error {
	m.mu.Lock()
	m.SetSummaryCalls = append(m.SetSummaryCalls, struct{ ParentID, Summary string }{parentIssueID, summary})
	m.mu.Unlock()
	return m.SetSummaryErr
}

// enqueuedEvent captures one EnqueueEventWithSource call by mockStore, used by
// cascade-hop tests to assert the verify event for the next level up was filed.
type enqueuedEvent struct {
	Source     string
	WebhookID  string
	Action     string
	SessionID  string
	IssueID    string
	Payload    string
	DeliveryID string
}

func (m *mockStore) EnqueueEventWithSource(source, webhookID, action, sessionID, issueID, payloadJSON, commentID string) (bool, error) {
	m.mu.Lock()
	m.Enqueued = append(m.Enqueued, enqueuedEvent{
		Source: source, WebhookID: webhookID, Action: action,
		SessionID: sessionID, IssueID: issueID, Payload: payloadJSON, DeliveryID: commentID,
	})
	m.mu.Unlock()
	if m.EnqueueErr != nil {
		return false, m.EnqueueErr
	}
	if m.EnqueueFreshOK {
		return m.EnqueueFresh, nil
	}
	return true, nil
}

func (m *mockStore) GetProductVerification(projectID string) (*store.ProductVerification, error) {
	return m.ProductVerification, m.ProductVerificationErr
}

func (m *mockStore) BumpProductVerificationRound(projectID string) (*store.ProductVerification, error) {
	m.mu.Lock()
	m.ProductBumpCalls = append(m.ProductBumpCalls, projectID)
	m.mu.Unlock()
	return m.BumpedProductVerification, m.BumpProductVerificationErr
}

func (m *mockStore) SetProductVerificationStatus(projectID, status string) error {
	m.mu.Lock()
	m.ProductSetStatusCalls = append(m.ProductSetStatusCalls, struct{ ProjectID, Status string }{projectID, status})
	m.mu.Unlock()
	return m.ProductSetStatusErr
}

// fakeGhProbe is a deterministic ghProbe for tests. Configure the maps
// keyed on branch name (for FindMergedPRForBranch) and PR url (for
// PRState); missing keys yield "not found" / "" respectively. Set the
// *Err fields to simulate transport failures.
type fakeGhProbe struct {
	mergedByBranch map[string]struct {
		URL, SHA string
	}
	openPRByBranch map[string]struct {
		URL, Author string
	}
	mergedErr   error
	openPRErr   error
	mergedCalls int32
	openPRCalls int32

	// envs records the environment each gh call was made with, so tests
	// can assert the run authenticated as the repo's registered account.
	envMu sync.Mutex
	envs  [][]string
}

func (f *fakeGhProbe) recordEnv(env []string) {
	f.envMu.Lock()
	defer f.envMu.Unlock()
	f.envs = append(f.envs, env)
}

// Environments returns the environments the gh calls were made with.
func (f *fakeGhProbe) Environments() [][]string {
	f.envMu.Lock()
	defer f.envMu.Unlock()
	return append([][]string(nil), f.envs...)
}

// TotalCalls returns how many gh calls the probe served.
func (f *fakeGhProbe) TotalCalls() int {
	return int(atomic.LoadInt32(&f.mergedCalls) + atomic.LoadInt32(&f.openPRCalls))
}

func (f *fakeGhProbe) FindMergedPRForBranch(_ context.Context, _, branch string, env []string) (string, string, bool, error) {
	atomic.AddInt32(&f.mergedCalls, 1)
	f.recordEnv(env)
	if f.mergedErr != nil {
		return "", "", false, f.mergedErr
	}
	if v, ok := f.mergedByBranch[branch]; ok {
		return v.URL, v.SHA, true, nil
	}
	return "", "", false, nil
}

func (f *fakeGhProbe) FindOpenPRForBranch(_ context.Context, _, branch string, env []string) (string, string, bool, error) {
	atomic.AddInt32(&f.openPRCalls, 1)
	f.recordEnv(env)
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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
//
// remoteFixture sets up a bare origin, a local clone, and an advancer clone
// for pushing external commits. ensureWorktree's contract: reuse the worktree
// if on disk, else rebuild from the local branch ref so admiral's prior
// unpushed commits are preserved; then fetch and refuse to proceed if origin
// has commits the worktree doesn't know about.

type remoteFixture struct {
	originDir   string
	localDir    string
	advancerDir string
	branch      string
}

func setupRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	mustGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
	}

	root := t.TempDir()
	originDir := filepath.Join(root, "origin.git")
	localDir := filepath.Join(root, "local")
	advancerDir := filepath.Join(root, "advancer")
	branch := "linear/test"

	mustGit(root, "init", "--bare", originDir)

	seedDir := filepath.Join(root, "seed")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	mustGit(seedDir, "init", "-b", "main")
	mustGit(seedDir, "config", "user.email", "test@test.com")
	mustGit(seedDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seedDir, "README"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(seedDir, "add", "README")
	mustGit(seedDir, "commit", "-m", "v1")
	mustGit(seedDir, "checkout", "-b", branch)
	mustGit(seedDir, "remote", "add", "origin", originDir)
	mustGit(seedDir, "push", "origin", branch)

	mustGit(root, "clone", originDir, localDir)
	mustGit(localDir, "config", "user.email", "test@test.com")
	mustGit(localDir, "config", "user.name", "test")
	mustGit(localDir, "fetch", "origin", branch)
	mustGit(localDir, "branch", branch, "origin/"+branch)

	mustGit(root, "clone", "-b", branch, originDir, advancerDir)
	mustGit(advancerDir, "config", "user.email", "advancer@test.com")
	mustGit(advancerDir, "config", "user.name", "advancer")

	return &remoteFixture{
		originDir:   originDir,
		localDir:    localDir,
		advancerDir: advancerDir,
		branch:      branch,
	}
}

// advance pushes a commit to origin from advancerDir, simulating a human or
// another admiral session moving the branch forward.
func (f *remoteFixture) advance(t *testing.T, content string) string {
	t.Helper()
	mustGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(f.advancerDir, "README"), []byte(content), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(f.advancerDir, "add", "README")
	mustGit(f.advancerDir, "commit", "-m", "advance:"+content)
	mustGit(f.advancerDir, "push", "origin", f.branch)
	cmd := exec.Command("git", "rev-parse", f.branch)
	cmd.Dir = f.originDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s in %s: %v: %s", f.branch, f.originDir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// resumeWorktreeAt creates a worktree at <path> checked out at <branch> in the
// given local repo, returning the path. Used to seed the reuse path.
func resumeWorktreeAt(t *testing.T, localDir, branch, path string) {
	t.Helper()
	cmd := exec.Command("git", "worktree", "add", path, branch)
	cmd.Dir = localDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed worktree at %s: %v: %s", path, err, out)
	}
}

// commitInWorktree adds a file and commits it inside worktreePath, returning
// the new HEAD SHA. Used to put the worktree ahead of origin (the normal state
// after a timed-out claude run that committed but never pushed).
func commitInWorktree(t *testing.T, worktreePath, filename, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktreePath, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	for _, args := range [][]string{
		{"add", filename},
		{"commit", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = worktreePath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), worktreePath, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v: %s", worktreePath, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newEnsureWorktreeFlow builds a minimal flow suitable for driving
// ensureWorktree against a real fixture. The orchestrator needs a non-nil
// cfg so the bot-identity apply step can read its (empty) BotIdentity field
// without panicking; the empty value short-circuits the apply, matching the
// pre-#162 no-bot behavior.
func newEnsureWorktreeFlow(repoDir, branch, worktreePath string) *flow {
	o := &Orchestrator{logger: slog.Default(), cfg: &config.Autopilot{}}
	return &flow{
		o:       o,
		repoDir: repoDir,
		job: &store.AutopilotJob{
			WorktreePath: worktreePath,
			Branch:       branch,
		},
	}
}

// Reuse, local ahead of origin (the normal timed-out → resume state).
// Origin is unchanged; the worktree has admiral's unpushed commit. push will
// fast-forward, so ensureWorktree must succeed.
func TestEnsureWorktree_ReuseLocalAheadOK(t *testing.T) {
	fx := setupRemoteFixture(t)
	worktreePath := filepath.Join(fx.localDir, ".worktrees", "resume-test")
	resumeWorktreeAt(t, fx.localDir, fx.branch, worktreePath)
	localHead := commitInWorktree(t, worktreePath, "claude.txt", "wip", "claude wip")

	f := newEnsureWorktreeFlow(fx.localDir, fx.branch, worktreePath)
	if err := f.ensureWorktree(); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if f.worktreePath != worktreePath {
		t.Errorf("worktreePath: got %q want %q", f.worktreePath, worktreePath)
	}
	if h := gitHead(t, worktreePath); h != localHead {
		t.Errorf("worktree HEAD: got %s want %s (local should still be ahead)", h, localHead)
	}
}

// Reuse, branch diverged: claude committed locally but someone else pushed a
// disjoint commit to origin. ensureWorktree must refuse to proceed — auto-
// rebase isn't safe, the user has to intervene.
func TestEnsureWorktree_ReuseDivergedErrors(t *testing.T) {
	fx := setupRemoteFixture(t)
	worktreePath := filepath.Join(fx.localDir, ".worktrees", "resume-test")
	resumeWorktreeAt(t, fx.localDir, fx.branch, worktreePath)
	commitInWorktree(t, worktreePath, "claude.txt", "wip", "claude wip")
	fx.advance(t, "external\n") // origin moves to a commit the worktree doesn't have

	f := newEnsureWorktreeFlow(fx.localDir, fx.branch, worktreePath)
	err := f.ensureWorktree()
	if err == nil {
		t.Fatal("expected ensureWorktree to error on divergence, got nil")
	}
	if !errors.Is(err, errBranchDiverged) {
		t.Errorf("error should wrap errBranchDiverged so markFailed can keep the worktree; got %q", err.Error())
	}
	// The user-actionable message must call out the worktree path so the
	// "rebase inside that worktree" instruction is actionable.
	if !strings.Contains(err.Error(), worktreePath) {
		t.Errorf("error should include worktree path %q; got %q", worktreePath, err.Error())
	}
}

// Reuse, local equal to origin: neither side moved since the prior run.
// Should pass — origin is an ancestor of HEAD (equal counts as ancestor).
func TestEnsureWorktree_ReuseEqualOK(t *testing.T) {
	fx := setupRemoteFixture(t)
	worktreePath := filepath.Join(fx.localDir, ".worktrees", "resume-test")
	resumeWorktreeAt(t, fx.localDir, fx.branch, worktreePath)

	f := newEnsureWorktreeFlow(fx.localDir, fx.branch, worktreePath)
	if err := f.ensureWorktree(); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
}

// Rebuild path: the worktree directory was cleaned up but the local branch
// ref still has admiral's prior commits in the parent repo's object store.
// `git worktree add <path> <branch>` re-checks out those local commits;
// ensureWorktree's divergence check should not error because origin is at the
// original base (still an ancestor of the local tip).
func TestEnsureWorktree_RebuildPreservesLocalCommits(t *testing.T) {
	fx := setupRemoteFixture(t)
	stagingPath := filepath.Join(fx.localDir, ".worktrees", "resume-test-staging")
	resumeWorktreeAt(t, fx.localDir, fx.branch, stagingPath)
	localHead := commitInWorktree(t, stagingPath, "claude.txt", "wip", "claude wip")
	// Remove the staging worktree directory but keep the local branch ref it
	// advanced — that's the state ensureWorktree's rebuild branch handles.
	cmd := exec.Command("git", "worktree", "remove", "--force", stagingPath)
	cmd.Dir = fx.localDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remove staging worktree: %v: %s", err, out)
	}

	rebuildPath := filepath.Join(fx.localDir, ".worktrees", "resume-test")
	f := newEnsureWorktreeFlow(fx.localDir, fx.branch, rebuildPath)
	if err := f.ensureWorktree(); err != nil {
		t.Fatalf("ensureWorktree (rebuild): %v", err)
	}
	if h := gitHead(t, rebuildPath); h != localHead {
		t.Errorf("rebuilt worktree HEAD: got %s want %s (local commit must be preserved)", h, localHead)
	}
}

// setupNoRemoteBranchFixture creates a fixture where origin has main but NOT
// the linear/test branch — the never-pushed-branch state that timed-out
// resume (0-commits-pushed) and first-time resume can land in.
func setupNoRemoteBranchFixture(t *testing.T) *remoteFixture {
	t.Helper()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	mustGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
	}

	root := t.TempDir()
	originDir := filepath.Join(root, "origin.git")
	localDir := filepath.Join(root, "local")
	branch := "linear/test"

	mustGit(root, "init", "--bare", originDir)

	seedDir := filepath.Join(root, "seed")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	mustGit(seedDir, "init", "-b", "main")
	mustGit(seedDir, "config", "user.email", "test@test.com")
	mustGit(seedDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seedDir, "README"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(seedDir, "add", "README")
	mustGit(seedDir, "commit", "-m", "v1")
	mustGit(seedDir, "remote", "add", "origin", originDir)
	// Push ONLY main to origin. The branch never reaches origin — mirrors
	// the post-timeout-0-commits state and the first-time-resume state.
	mustGit(seedDir, "push", "origin", "main")

	mustGit(root, "clone", originDir, localDir)
	mustGit(localDir, "config", "user.email", "test@test.com")
	mustGit(localDir, "config", "user.name", "test")

	// Create the local branch ref pointing at origin/main — never pushed to
	// origin, but a valid ref in local so `git worktree add <path> <branch>`
	// can resolve it.
	mustGit(localDir, "branch", branch, "origin/main")

	return &remoteFixture{
		originDir:   originDir,
		localDir:    localDir,
		advancerDir: "", // unused in no-remote-ref tests
		branch:      branch,
	}
}

// Reuse, no remote ref for the branch (HEAD == origin/main): the normal
// timed-out-0-commits state. ensureWorktree must succeed — claude --resume
// works off the local session context; there is no remote state to diverge
// against. Previously this case hard-failed with "couldn't find remote ref".
func TestEnsureWorktree_ReuseNoRemoteRefOK(t *testing.T) {
	fx := setupNoRemoteBranchFixture(t)
	worktreePath := filepath.Join(fx.localDir, ".worktrees", "resume-test")
	resumeWorktreeAt(t, fx.localDir, fx.branch, worktreePath)

	f := newEnsureWorktreeFlow(fx.localDir, fx.branch, worktreePath)
	if err := f.ensureWorktree(); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
}

// Reuse, no remote ref, but the worktree has an unpushed commit: claude
// committed locally before the timeout but never pushed. The divergence
// check is still vacuous (nothing on origin to compare against), so
// ensureWorktree must succeed and leave the local commit intact.
func TestEnsureWorktree_ReuseNoRemoteRefLocalAheadOK(t *testing.T) {
	fx := setupNoRemoteBranchFixture(t)
	worktreePath := filepath.Join(fx.localDir, ".worktrees", "resume-test")
	resumeWorktreeAt(t, fx.localDir, fx.branch, worktreePath)
	localHead := commitInWorktree(t, worktreePath, "claude.txt", "wip", "claude wip")

	f := newEnsureWorktreeFlow(fx.localDir, fx.branch, worktreePath)
	if err := f.ensureWorktree(); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	if h := gitHead(t, worktreePath); h != localHead {
		t.Errorf("worktree HEAD: got %s want %s (local commit must survive)", h, localHead)
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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
		cfg:      &config.Autopilot{MaxRunSeconds: 60, WorktreeRoot: ".worktrees"},
		db:       ms,
		lc:       mlc,
		gh:       gh,
		accounts: fakeAccounts(),
		logger:   slog.Default(),
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

// TestGhCreateWithRetry_RetriesTransientThenSucceeds covers the failure mode
// from the original incident (admiral job failed with a GraphQL "unexpected
// EOF" during `gh pr create`'s internal check-existing-PR step): the first
// attempt should backoff + retry on transient errors, the second should
// succeed.
func TestGhCreateWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	gh := func(ctx context.Context, args ...string) (string, error) {
		calls++
		if calls < 2 {
			return `Post "https://api.github.com/graphql": unexpected EOF`, fmt.Errorf("exit status 1")
		}
		return "https://github.com/owner/repo/pull/42\n", nil
	}
	out, err := ghCreateWithRetry(context.Background(), gh, []time.Duration{0, 0, 0},
		"pr", "create", "--base", "main", "--head", "feat/x")
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if !strings.Contains(out, "pull/42") {
		t.Fatalf("unexpected out: %s", out)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (1 transient + 1 ok), got %d", calls)
	}
}

// TestGhCreateWithRetry_ExhaustsReturnsError ensures a persistent transient
// failure surfaces as a real error once the backoff schedule is exhausted —
// so a chronically-flaky network doesn't loop forever.
func TestGhCreateWithRetry_ExhaustsReturnsError(t *testing.T) {
	calls := 0
	gh := func(ctx context.Context, args ...string) (string, error) {
		calls++
		return `Post "https://api.github.com/graphql": unexpected EOF`, fmt.Errorf("exit status 1")
	}
	out, err := ghCreateWithRetry(context.Background(), gh, []time.Duration{0, 0},
		"pr", "create", "--base", "main", "--head", "feat/x")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (1 + 2 retries), got %d", calls)
	}
	// Output preserved so the caller's classifyGhCreateError dispatch + error
	// message can still see the upstream stderr.
	if !strings.Contains(out, "unexpected EOF") {
		t.Fatalf("expected EOF text preserved in out: %s", out)
	}
}

// TestGhCreateWithRetry_NonTransientNoRetry covers the soft failure shapes
// (already-exists / no-commits-between) and the fatal fall-through: none
// should retry — they're not network blips, and retrying "already exists"
// would just waste time before the caller's lookupPR recovery.
func TestGhCreateWithRetry_NonTransientNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{
			name: "already exists",
			out:  `a pull request for branch "feat/x" into branch "main" already exists`,
		},
		{
			name: "no commits between",
			out:  "pull request create failed: GraphQL: No commits between main and feat/x (createPullRequest)",
		},
		{
			name: "fatal auth",
			out:  "error: GraphQL: Resource not accessible by integration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			gh := func(ctx context.Context, args ...string) (string, error) {
				calls++
				return tc.out, fmt.Errorf("exit status 1")
			}
			_, err := ghCreateWithRetry(context.Background(), gh, []time.Duration{0, 0, 0},
				"pr", "create", "--base", "main", "--head", "feat/x")
			if err == nil {
				t.Fatal("expected error from non-transient failure")
			}
			if calls != 1 {
				t.Fatalf("non-transient must not retry, got %d calls", calls)
			}
		})
	}
}

// TestGhCreateWithRetry_TransientThenAlreadyExistsStopsRetrying covers the
// post-EOF idempotency path: attempt 1 dies with a transient error mid-create
// (mutation may or may not have committed server-side), attempt 2 sees
// "already exists" — the helper must hand that back to the caller without
// further retry so ensurePR's lookupPR recovery branch can run.
func TestGhCreateWithRetry_TransientThenAlreadyExistsStopsRetrying(t *testing.T) {
	calls := 0
	gh := func(ctx context.Context, args ...string) (string, error) {
		calls++
		if calls == 1 {
			return `Post "https://api.github.com/graphql": unexpected EOF`, fmt.Errorf("exit status 1")
		}
		return `a pull request for branch "feat/x" into branch "main" already exists`, fmt.Errorf("exit status 1")
	}
	out, err := ghCreateWithRetry(context.Background(), gh, []time.Duration{0, 0, 0},
		"pr", "create", "--base", "main", "--head", "feat/x")
	if err == nil {
		t.Fatal("expected error to surface so the caller dispatch can run")
	}
	if !strings.Contains(out, "already exists") {
		t.Fatalf("expected already-exists output preserved for caller, got: %s", out)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (1 transient + 1 non-transient stop), got %d", calls)
	}
}

// TestGhCreateWithRetry_RespectsContextCancellation guards against the
// backoff sleep ignoring a cancelled context (which would defer abort by up
// to the longest delay in the schedule).
func TestGhCreateWithRetry_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	gh := func(_ context.Context, args ...string) (string, error) {
		calls++
		return `Post "https://api.github.com/graphql": unexpected EOF`, fmt.Errorf("exit status 1")
	}
	_, err := ghCreateWithRetry(ctx, gh, []time.Duration{time.Hour}, "pr", "create")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancellation aborts the backoff, got %d", calls)
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

// TestDispatch_RerunInContextBundle_Supersedes covers the current Linear
// @mention payload: PromptContext is the full <issue>/<comment>/... bundle
// and the actual comment only appears in the trailing <user-request>.
func TestDispatch_RerunInContextBundle_Supersedes(t *testing.T) {
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
		SourceCommentID: "comment-rerun-bundle",
		PromptContext: "<issue identifier=\"DONE-1\">\n<title>T</title>\n</issue>\n" +
			"<comment author=\"admiral\">\nadmiral does not respond to bare @mentions.\n</comment>\n\n" +
			"<user-request>\n@claudeagent /rerun\n</user-request>\n",
	}

	o.HandleAgentEvent(ev)

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

// TestDispatch_BareHandleOnAwaitingInput_DoesNotAnswer: an @mention with
// nothing after the handle must not be recorded as the ask_user answer.
func TestDispatch_BareHandleOnAwaitingInput_DoesNotAnswer(t *testing.T) {
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:           "issue-await",
			State:             store.JobStateAwaitingInput,
			AttemptN:          1,
			PendingQuestionID: "q-1",
		},
	}
	mlc := &mockLinearClient{}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.dispatch(linear.AgentEvent{
		SessionID:       "sess-await",
		IssueID:         "issue-await",
		IssueIdentifier: "AWAIT-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-bare-handle",
		PromptContext:   "<issue identifier=\"AWAIT-1\">\n</issue>\n<user-request>\n@claudeagent\n</user-request>\n",
	})
	ms.mu.Lock()
	lookups := ms.PendingQuestionLookups
	ms.mu.Unlock()
	if lookups != 0 {
		t.Fatalf("bare handle must not take the ask_user reply path (pending-question lookups: %d)", lookups)
	}
	if body := mlc.GetPostedBody(); !strings.Contains(body, "waiting for your answer") {
		t.Errorf("expected a pending-question hint, got: %q", body)
	}
}

// TestDispatch_MentionWithoutSourceComment_Supersedes: Linear does not
// always set sourceCommentId on @mention-created sessions (seen on klink
// GEO-421, 2026-10-26). A <user-request> that starts with an @handle is
// still a mention, not a repeat assign.
func TestDispatch_MentionWithoutSourceComment_Supersedes(t *testing.T) {
	mlc := &mockLinearClient{GetIssueErr: fmt.Errorf("synthetic short-circuit")}
	ms := &mockStore{AdmiralTask: &store.AdmiralTask{
		IssueID: "issue-done", State: store.JobStateDone, AttemptN: 1, PRURL: "https://github.com/x/y/pull/1",
	}}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.HandleAgentEvent(linear.AgentEvent{
		SessionID:       "sess-mention-nosrc",
		IssueID:         "issue-done",
		IssueIdentifier: "DONE-1",
		Action:          linear.ActionCreated,
		PromptContext:   "<issue identifier=\"DONE-1\">\n</issue>\n\n<user-request>\n@claudeagent  /rerun\n</user-request>\n",
	})
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && len(ms.ClaimedSnapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.SupersededAdmiralIssues; len(got) != 1 || got[0] != "issue-done" {
		t.Errorf("expected supersession on issue-done; got %v (posted: %q)", got, mlc.GetPostedBody())
	}
}

// TestDispatch_DelegateBundle_StillRepeatAssign: a delegate's bundle has a
// <user-request> without a leading handle and must stay a delegate.
func TestDispatch_DelegateBundle_StillRepeatAssign(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{AdmiralTask: &store.AdmiralTask{IssueID: "issue-known", State: store.JobStateDone, AttemptN: 1}}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.HandleAgentEvent(linear.AgentEvent{
		SessionID:       "sess-delegate-bundle",
		IssueID:         "issue-known",
		IssueIdentifier: "KNW-1",
		Action:          linear.ActionCreated,
		PromptContext:   "<issue identifier=\"KNW-1\">\n</issue>\n\n<user-request>\nWork on [KNW-1](https://linear.app/x/issue/KNW-1/y)\n</user-request>\n",
	})
	if body := mlc.GetPostedBody(); !strings.Contains(body, "Task already exists") {
		t.Errorf("expected repeat-assign reply, got: %s", body)
	}
}

// TestDispatch_PromptedWithHandle_Supersedes: users type the @handle in
// agent-thread replies too ("@claudeagent /rerun").
func TestDispatch_PromptedWithHandle_Supersedes(t *testing.T) {
	mlc := &mockLinearClient{GetIssueErr: fmt.Errorf("synthetic short-circuit")}
	ms := &mockStore{AdmiralTask: &store.AdmiralTask{IssueID: "issue-thread", State: store.JobStateDone, AttemptN: 1}}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.HandleAgentEvent(linear.AgentEvent{
		SessionID:       "sess-thread-handle",
		IssueID:         "issue-thread",
		IssueIdentifier: "THR-3",
		Action:          linear.ActionPrompted,
		UserMessage:     "@claudeagent /rerun",
	})
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && len(ms.SupersededAdmiralIssues) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ms.SupersededAdmiralIssues; len(got) != 1 || got[0] != "issue-thread" {
		t.Errorf("expected supersession; got %v (posted: %q)", got, mlc.GetPostedBody())
	}
}

// TestDispatch_PromptedAnswerKeepsHandle: a thread reply that starts with
// someone else's handle is an ask_user answer and must be recorded whole.
func TestDispatch_PromptedAnswerKeepsHandle(t *testing.T) {
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-await", State: store.JobStateAwaitingInput, AttemptN: 1, PendingQuestionID: "q-1",
		},
		PendingQuestion: &store.PendingQuestion{ID: "q-1"},
	}
	mlc := &mockLinearClient{}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.dispatch(linear.AgentEvent{
		SessionID:       "sess-answer",
		IssueID:         "issue-await",
		IssueIdentifier: "AWAIT-2",
		Action:          linear.ActionPrompted,
		UserMessage:     "@alice owns it, keep v1",
	})
	ms.mu.Lock()
	answered := ms.AnsweredText
	ms.mu.Unlock()
	if answered != "@alice owns it, keep v1" {
		t.Fatalf("answer = %q, want the whole reply (posted: %q)", answered, mlc.GetPostedBody())
	}
}

// TestDispatch_UnparsedEnvelope_FailsClosed: an envelope admiral can't
// locate must not be taken as a delegate (which would start a fresh run).
func TestDispatch_UnparsedEnvelope_FailsClosed(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{} // no task row: a delegate here would dispatchFreshAssign
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.HandleAgentEvent(linear.AgentEvent{
		SessionID:       "sess-unparsed",
		IssueID:         "issue-new",
		IssueIdentifier: "NEW-1",
		Action:          linear.ActionCreated,
		PromptContext:   "<issue identifier=\"NEW-1\">\n</issue>\n<user-request id=\"1\">\n@claudeagent /status\n</user-request>\n",
	})
	if body := mlc.GetPostedBody(); !strings.Contains(body, "could not read") {
		t.Fatalf("unparsed envelope must be rejected with a parse-failure hint, not dispatched as a delegate; posted %q, claimed %v",
			body, ms.ClaimedSnapshot())
	}
}

// TestDispatch_UnparsedEnvelope_OnLiveTask_ExplainsParseFailure: with a
// live task the user gets a parse-failure hint, not a bare/empty-answer one.
func TestDispatch_UnparsedEnvelope_OnLiveTask_ExplainsParseFailure(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		AdmiralTask:     &store.AdmiralTask{IssueID: "issue-await", State: store.JobStateAwaitingInput, AttemptN: 1, PendingQuestionID: "q-1"},
		PendingQuestion: &store.PendingQuestion{ID: "q-1"},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.dispatch(linear.AgentEvent{
		SessionID:       "sess-unparsed-await",
		IssueID:         "issue-await",
		IssueIdentifier: "AWAIT-3",
		Action:          linear.ActionCreated,
		SourceCommentID: "c-1",
		PromptContext:   "<issue>x</issue>\n<user-request>@claudeagent use Postgres</user-request>\n",
	})
	if body := mlc.GetPostedBody(); !strings.Contains(body, "could not read") {
		t.Errorf("expected a parse-failure hint, got: %q", body)
	}
	ms.mu.Lock()
	answered := ms.AnsweredText
	ms.mu.Unlock()
	if answered != "" {
		t.Errorf("unparsed envelope must not answer the question; answered %q", answered)
	}
}

// TestDispatch_PromptedLoneHandle_DoesNotAnswer: a lone "@claudeagent" in the
// agent thread is an empty reply, not an answer.
func TestDispatch_PromptedLoneHandle_DoesNotAnswer(t *testing.T) {
	mlc := &mockLinearClient{}
	ms := &mockStore{
		AdmiralTask:     &store.AdmiralTask{IssueID: "issue-await", State: store.JobStateAwaitingInput, AttemptN: 1, PendingQuestionID: "q-1"},
		PendingQuestion: &store.PendingQuestion{ID: "q-1"},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	o.dispatch(linear.AgentEvent{
		SessionID:       "sess-lone-handle",
		IssueID:         "issue-await",
		IssueIdentifier: "AWAIT-4",
		Action:          linear.ActionPrompted,
		UserMessage:     "@claudeagent",
	})
	ms.mu.Lock()
	answered := ms.AnsweredText
	ms.mu.Unlock()
	if answered != "" {
		t.Errorf("lone handle must not answer the question; answered %q", answered)
	}
	if body := mlc.GetPostedBody(); !strings.Contains(body, "waiting for your answer") {
		t.Errorf("expected a pending-question hint, got: %q", body)
	}
}

// TestDispatch_RerunOnDone_ParksWhenBlocked verifies /rerun respects blockers
// like a fresh assign: a DONE task whose issue still has unresolved blockers is
// superseded (fresh attempt claimed) and then parked BLOCKED instead of
// spawning a run. The BlockerWatcher resumes it once the blockers clear. This
// closes the gap where /rerun bypassed the dependency gate and shipped a
// sub-issue before its foundation.
func TestDispatch_RerunOnDone_ParksWhenBlocked(t *testing.T) {
	mlc := &mockLinearClient{
		IssueBlockers: []linear.IssueBlocker{{IssueID: "id-A", IssueIdentifier: "GEO-A"}},
	}
	ms := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:  "issue-b",
			State:    store.JobStateDone,
			AttemptN: 1,
			PRURL:    "https://github.com/x/y/pull/1",
		},
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-rerun",
		IssueID:         "issue-b",
		IssueIdentifier: "GEO-B",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-rerun",
		PromptContext:   "/rerun",
	}

	o.HandleAgentEvent(ev)

	// Supersede still happens before the gate (fresh attempt claimed).
	if got := ms.SupersededAdmiralIssues; len(got) != 1 || got[0] != "issue-b" {
		t.Errorf("expected supersession on issue-b; got %v", got)
	}
	// Parked BLOCKED, synchronously, before any run goroutine spawns.
	if got := ms.SetBlockedSnapshot(); len(got) != 1 || got[0] != "issue-b" {
		t.Errorf("expected issue-b parked BLOCKED; got %v", got)
	}
	// Give any (erroneously spawned) run goroutine a chance to claim, then
	// assert none did.
	time.Sleep(50 * time.Millisecond)
	if got := ms.ClaimedSnapshot(); len(got) != 0 {
		t.Errorf("blocked rerun must not spawn a run (no autopilot_jobs claim); got %v", got)
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

	// Wait for the /fix goroutine to flip state to EXECUTING. Assert on the
	// recorded transition sequence rather than the instantaneous state: the
	// run fails fast in this test (the issue fetch is short-circuited), so
	// sampling the live state races with the goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if slices.Contains(ms.StateTransitionSnapshot(), store.JobStateExecuting) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if transitions := ms.StateTransitionSnapshot(); !slices.Contains(transitions, store.JobStateExecuting) {
		t.Errorf("expected admiral_tasks to transition to EXECUTING during /fix run; got %v", transitions)
	}
	snap := ms.LiveAdmiralSnapshot()
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

// TestDispatch_FixOnDoneMerged_SuggestsRerun verifies /fix on a task
// whose PR has been merged (discoverer flipped DONE -> DONE_MERGED)
// is rejected with a message pointing the user at /rerun, instead of
// falling through to the catch-all "/fix not supported in state" branch.
func TestDispatch_FixOnDoneMerged_SuggestsRerun(t *testing.T) {
	mlc := &mockLinearClient{}
	live := &store.AdmiralTask{
		IssueID:         "issue-merged",
		State:           store.JobStateDoneMerged,
		AttemptN:        1,
		PRURL:           "https://github.com/x/y/pull/3",
		ClaudeSessionID: "claude-merged",
	}
	ms := &mockStore{
		AdmiralTask:     live,
		LiveAdmiralTask: live,
	}
	o := newTestOrchestrator(t, ms, mlc, &fakeGhProbe{})
	ev := linear.AgentEvent{
		SessionID:       "sess-fix-merged",
		IssueID:         "issue-merged",
		IssueIdentifier: "MERGED-1",
		Action:          linear.ActionCreated,
		SourceCommentID: "comment-fix-merged",
		PromptContext:   "/fix one more tweak",
	}

	o.HandleAgentEvent(ev)

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "merged") {
		t.Errorf("expected merge-aware reject reply, got: %s", body)
	}
	if !strings.Contains(body, "/rerun") {
		t.Errorf("expected suggestion to use /rerun, got: %s", body)
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

	// Assert on the recorded transition sequence rather than the
	// instantaneous state: the run fails fast in this test (the issue fetch
	// is short-circuited), so sampling the live state races with the
	// dispatched goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if slices.Contains(ms.StateTransitionSnapshot(), store.JobStateExecuting) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if transitions := ms.StateTransitionSnapshot(); !slices.Contains(transitions, store.JobStateExecuting) {
		t.Errorf("/fix-via-thread must transition admiral_tasks to EXECUTING; got %v", transitions)
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

// ---- Tests for isPreparePhaseError ----
//
// isPreparePhaseError gates whether a /resume failure routes to markTimedOut
// (preserve state, allow retry) vs markFailed (close off /resume). The
// matchers target the stable verb prefixes the resume helpers wrap their
// errors with, so a failure inside ensureWorktree / openStreamFile /
// writeMCPConfig keeps the task TIMED_OUT.

func TestIsPreparePhaseError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ensure_worktree", fmt.Errorf("ensure worktree: git fetch origin: exit status 128"), true},
		{"ensure_worktree_wrapped",
			fmt.Errorf("ensure worktree: %w", errors.New("fatal: couldn't find remote ref linear/x")),
			true},
		{"open_stream_file", fmt.Errorf("open stream file: permission denied"), true},
		{"mcp_config", fmt.Errorf("mcp config write failed: %w", errors.New("EACCES")), true},
		{"claude_resume", fmt.Errorf("claude resume: exit status 1"), false},
		{"claude_exit_deadline", fmt.Errorf("claude exit: signal: killed: context deadline exceeded"), false},
		{"push", fmt.Errorf("push: failed to push origin linear/x: 403"), false},
		{"unrelated", errors.New("something else entirely"), false},
		{"fetch_alone_without_ensure_worktree_wrapper",
			errors.New("fatal: couldn't find remote ref linear/x"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPreparePhaseError(c.err); got != c.want {
				t.Errorf("isPreparePhaseError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestFlowBindRepoRunBudget(t *testing.T) {
	tests := []struct {
		name      string
		verifyCmd string
		want      time.Duration
	}{
		// A gated repo's budget must cover claude plus every gate
		// fix-forward retry (prFixBudget arithmetic), not a single
		// MaxRunSeconds+120 window — otherwise the outer ctx kills the gate
		// mid-build and verify_max_retries is unreachable (GEO-428).
		{"gated scales with gate retries", "make check", prFixBudget(60, 2)},
		{"ungated keeps plain budget", "", 180 * time.Second},
		// The gate trims before deciding whether to run; gating decisions
		// must agree, or a whitespace-only verify_cmd buys the scaled budget
		// but runs a gate that can only fail.
		{"whitespace-only verify_cmd is ungated", "   ", 180 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &Orchestrator{cfg: &config.Autopilot{MaxRunSeconds: 60, VerifyMaxRetries: 2}}
			f := newFlow(o, context.Background(), linear.AgentEvent{})
			err := f.bindRepo(&store.Repo{
				RepoDir:    "/tmp/repo",
				BaseBranch: "main",
				VerifyCmd:  tt.verifyCmd,
				GhUser:     "someone",
			}, "proj-1")
			if err != nil {
				t.Fatalf("bindRepo: %v", err)
			}
			dl, ok := f.ctx.Deadline()
			if !ok {
				t.Fatal("no deadline on flow ctx after bindRepo — run budget not applied")
			}
			remaining := time.Until(dl)
			// Small tolerance for the clock between WithTimeout and Until.
			if remaining > tt.want+2*time.Second || remaining < tt.want-2*time.Second {
				t.Fatalf("run budget = %s, want ≈ %s", remaining, tt.want)
			}
			if f.cancelBudget != nil {
				f.cancelBudget()
			}
		})
	}
}

// bindRepo must refuse before applying the budget: an unidentifiable run is
// dead anyway, and the deadline must not have moved.
func TestFlowBindRepoGhUserUnsetNoBudget(t *testing.T) {
	o := &Orchestrator{cfg: &config.Autopilot{MaxRunSeconds: 60, VerifyMaxRetries: 2}}
	f := newFlow(o, context.Background(), linear.AgentEvent{})
	err := f.bindRepo(&store.Repo{RepoDir: "/tmp/repo", BaseBranch: "main"}, "proj-1")
	if !isGhUserUnset(err) {
		t.Fatalf("bindRepo err = %v, want ghUserUnset", err)
	}
	if _, ok := f.ctx.Deadline(); ok {
		t.Fatal("budget applied despite bindRepo failure")
	}
	if f.cancelBudget != nil {
		t.Fatal("cancelBudget set despite bindRepo failure")
	}
}

// ctxAwarePoster fails posts that run on a dead context — the real client
// honors request ctx, so a post attempted on a canceled ctx never lands.
// Wrapping the recording mock keeps the assertions honest about WHERE the
// message is posted from.
type ctxAwarePoster struct{ *mockLinearClient }

func (p *ctxAwarePoster) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.mockLinearClient.PostAgentActivity(ctx, sessionID, a)
}

// gateFailFixture builds a flow wired to recording mocks, ready for a
// handleGateFailure call. worktreePath is a non-git temp dir so the
// best-effort remote-branch cleanup fails fast locally instead of touching
// any real repo.
func gateFailFixture(t *testing.T) (*Orchestrator, *mockStore, *mockLinearClient, *flow) {
	t.Helper()
	ms := &mockStore{LiveAdmiralTask: &store.AdmiralTask{
		IssueID: "iss-1", State: store.JobStateExecuting,
	}}
	mlc := &mockLinearClient{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, VerifyMaxRetries: 2},
		db:     ms,
		lc:     &ctxAwarePoster{mockLinearClient: mlc},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f := newFlow(o, context.Background(), linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1"})
	f.worktreePath = t.TempDir()
	f.branch = "linear/geo-428"
	f.verifyCmd = "make check"
	return o, ms, mlc, f
}

// The run budget expiring mid-gate is a timeout, not a build failure — but
// the classification belongs to runWithAttempt (marking here too would
// settle the row and post the message twice). handleGateFailure only flags
// the flow and returns the classified error.
func TestHandleGateFailure_BudgetExpiredDefersClassification(t *testing.T) {
	_, ms, mlc, f := gateFailFixture(t)
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 2, LastBuildOutput: "boom"})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handleGateFailure err = %v, want wrapped context.DeadlineExceeded", err)
	}
	// The persisted error text is what dispatchResume keys on — the writer's
	// output must keep matching the reader's prefix, or /resume silently
	// re-opens the gate-expiry funnel.
	if !isGateExpiryTimeout(err.Error()) {
		t.Fatalf("handleGateFailure err = %q, want it to match gateExpiryErrPrefix", err)
	}
	if !f.gateExpired {
		t.Fatal("gateExpired not set — timedOutMessage would advertise unsafe /resume")
	}
	if f.gateExpiredBuildTail != "boom" {
		t.Fatalf("gateExpiredBuildTail = %q, want the gate's last build output", f.gateExpiredBuildTail)
	}
	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateTimedOut {
		t.Fatalf("admiral task state = %s, want TIMED_OUT persisted before return (crash-safe; classification still posts once upstream)", got)
	}
	if body := mlc.GetPostedBody(); body != "" {
		t.Fatalf("posted body = %q, want nothing posted (single markTimedOut upstream)", body)
	}
}

// flakyPoster wraps ctxAwarePoster, failing the first post before letting
// one through — a transient Linear blip.
type flakyPoster struct {
	*ctxAwarePoster
	failures int32
}

func (p *flakyPoster) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	if atomic.AddInt32(&p.failures, 1) <= 1 {
		return errors.New("transient linear blip")
	}
	return p.ctxAwarePoster.PostAgentActivity(ctx, sessionID, a)
}

// A gate-killed run must never be left interrupt-resumable, even when the
// shutdown lands after handleGateFailure returned — classification (via
// markInterrupted) is the last line of defense against the reconciler
// resuming past the verify gate.
func TestMarkInterrupted_GateFailedLandsFailedNotResumable(t *testing.T) {
	_, ms, mlc, f := gateFailFixture(t)
	f.gateFailed = true

	f.markInterrupted(errors.New("run budget expired during build gate: context deadline exceeded"))

	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateFailed {
		t.Fatalf("admiral task state = %s, want FAILED (the interrupt marker would auto-resume past the gate)", got)
	}
	if body := mlc.GetPostedBody(); !strings.Contains(body, "/rerun") {
		t.Fatalf("posted body = %q, want rerun guidance", body)
	}
}

// On a gated repo ANY timeout — not just gate expiry — must steer away from
// /resume: the resume path pushes and PRs without re-running the verify
// gate, so resuming gate-killed code past the gate is the same funnel.
func TestTimedOutMessage_GatedRepoSteersToRerun(t *testing.T) {
	o := &Orchestrator{cfg: &config.Autopilot{MaxRunSeconds: 1800}}
	// First-run gated flow (bindRepo applied the run budget): any timeout
	// steers to /rerun — resume would push code the gate never saw.
	gated := newFlow(o, context.Background(), linear.AgentEvent{})
	gated.verifyCmd = "make check"
	gated.outerBudget = runBudget(1800, 2, "make check")
	gated.claudeSessionID = "sess-12345678"

	s := gated.timedOutMessage()
	if !strings.Contains(s, "prefer /rerun") {
		t.Fatalf("gated timedOutMessage = %q, want rerun steer", s)
	}
	if strings.Contains(s, "resume from where I left off") {
		t.Fatalf("gated timedOutMessage = %q, want no resume affordance", s)
	}
}

// The gate-expiry thread message is the only steer away from /resume — a
// transient Linear failure must not silently drop it.
func TestMarkTimedOut_GateExpiryMessageRetriesThroughBlip(t *testing.T) {
	mlc := &mockLinearClient{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60},
		db:     &mockStore{LiveAdmiralTask: &store.AdmiralTask{IssueID: "iss-1"}},
		lc:     &flakyPoster{ctxAwarePoster: &ctxAwarePoster{mockLinearClient: mlc}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f := newFlow(o, context.Background(), linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1"})
	f.gateExpired = true
	f.claudeSessionID = "sess-12345678"

	f.markTimedOut(errors.New("run budget expired during build gate: context deadline exceeded"))

	if body := mlc.GetPostedBody(); !strings.Contains(body, "prefer /rerun") {
		t.Fatalf("posted body = %q, want the gate-expiry steer delivered through the blip", body)
	}
}

// Retry backoff must respect the caller's ctx: a dead ctx burns no sleep
// time toward attempts that can only fail.
func TestPostActivityWithRetryCtx_DeadCtxReturnsFast(t *testing.T) {
	mlc := &mockLinearClient{PostErr: errors.New("nope")}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60},
		db:     &mockStore{},
		lc:     mlc,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f := newFlow(o, context.Background(), linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := f.postActivityWithRetryCtx(ctx, linear.Response("x"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("postActivityWithRetryCtx err = nil, want the last post error")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("dead-ctx retry took %s, want it to skip the backoff", elapsed)
	}
}

// countingPoster records how many posts actually went out.
type countingPoster struct {
	*ctxAwarePoster
	posts int32
}

func (p *countingPoster) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	atomic.AddInt32(&p.posts, 1)
	return p.ctxAwarePoster.PostAgentActivity(ctx, sessionID, a)
}

// A transient failure (network blip on the git push) must not permanently
// suppress the cleanup — a later caller with a healthy ctx retries it.
func TestDeleteRemoteBranchBestEffort_RetriesAfterTransientFailure(t *testing.T) {
	dir := t.TempDir()
	attempted := filepath.Join(dir, "attempted")
	gitLog := filepath.Join(dir, "git.log")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + gitLog + "\n" +
		"if [ -f " + attempted + " ]; then exit 0; fi\n" +
		"touch " + attempted + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, _, _, f := gateFailFixture(t)
	f.worktreePath = dir

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.deleteRemoteBranchBestEffort(ctx) // first attempt: transient failure
	f.deleteRemoteBranchBestEffort(ctx) // later caller: healthy retry

	recorded, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatalf("fake git never ran: %v", err)
	}
	if got := strings.Count(string(recorded), "push origin --delete"); got != 2 {
		t.Fatalf("cleanup attempted %d times, want 2 (retry after the transient failure)", got)
	}
}

// The gate-expiry branch must persist the TIMED_OUT row BEFORE its (up to
// 30s) remote-branch cleanup: a crash mid-cleanup must not leave the row
// EXECUTING for the boot reconciler to auto-resume past the verify gate.
func TestHandleGateFailure_ExpiryPersistsBeforeCleanup(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	script := "#!/bin/sh\ntouch " + started + "\n" +
		"while [ ! -f " + release + " ]; do sleep 0.02; done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, ms, _, f := gateFailFixture(t)
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 1, LastBuildOutput: "boom"})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake git never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Cleanup is now blocked mid-flight; the row must already be TIMED_OUT.
	seen := false
	for time.Now().Before(deadline) {
		if ms.LiveAdmiralSnapshot().State == store.JobStateTimedOut {
			seen = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seen {
		t.Fatalf("row still %s while cleanup runs, want TIMED_OUT persisted before the cleanup", ms.LiveAdmiralSnapshot().State)
	}
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatalf("release fake git: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("handleGateFailure err = nil, want non-nil")
	}
}

// A /rerun superseding the row suppresses the FAILED write (attempt guard)
// but not the rerun-guidance post — classification (markInterrupted) must
// not settle and post a second time when the flow already did.
func TestSettleGateFailedByShutdown_SupersededRowPostsOnce(t *testing.T) {
	ms := &mockStore{LiveAdmiralTask: &store.AdmiralTask{IssueID: "iss-1", AttemptN: 1, State: store.JobStateExecuting}}
	mlc := &mockLinearClient{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60},
		db:     ms,
		lc:     &countingPoster{ctxAwarePoster: &ctxAwarePoster{mockLinearClient: mlc}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f := newFlow(o, context.Background(), linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1"})
	f.worktreePath = t.TempDir()
	f.branch = "linear/geo-428"
	f.expectAttemptN = 2 // the row was superseded by a /rerun
	f.gateFailed = true

	// Race branch settle, then classification — the exact round-7 sequence.
	f.settleGateFailedByShutdown()
	f.markInterrupted(errors.New("run budget expired during build gate: context deadline exceeded"))

	if got := atomic.LoadInt32(&o.lc.(*countingPoster).posts); got != 1 {
		t.Fatalf("rerun guidance posted %d times, want exactly once", got)
	}
}

// The detailed build report is a genuine gate failure's ONLY thread message
// (markFailed skips already-terminal rows), so it must survive a transient
// Linear blip.
func TestHandleGateFailure_GenuineFailReportRetriesThroughBlip(t *testing.T) {
	ms := &mockStore{LiveAdmiralTask: &store.AdmiralTask{IssueID: "iss-1", State: store.JobStateExecuting}}
	mlc := &mockLinearClient{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, VerifyMaxRetries: 2},
		db:     ms,
		lc:     &flakyPoster{ctxAwarePoster: &ctxAwarePoster{mockLinearClient: mlc}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f := newFlow(o, context.Background(), linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1"})
	f.worktreePath = t.TempDir()
	f.branch = "linear/geo-428"
	f.verifyCmd = "make check"

	if err := f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 2, LastBuildOutput: "boom"}); err == nil {
		t.Fatal("handleGateFailure err = nil, want gate-failure error")
	}
	if body := mlc.GetPostedBody(); !strings.Contains(body, "could not get the build to pass") {
		t.Fatalf("posted body = %q, want the build report delivered through the blip", body)
	}
}

// markFailed is the last message a non-timeout failure gets — it must post
// even when the run budget ctx is already dead (the error that landed here
// raced the expiry without wrapping it).
func TestMarkFailed_PostsThroughDeadBudgetCtx(t *testing.T) {
	_, _, mlc, f := gateFailFixture(t)
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	f.markFailed(errors.New("ensure PR: gh exited 1"))

	if body := mlc.GetPostedBody(); !strings.Contains(body, "admiral failed") {
		t.Fatalf("posted body = %q, want the failure delivered despite the dead budget ctx", body)
	}
}

// /resume on a gate-expiry TIMED_OUT row must be refused: resuming pushes
// and PRs without re-running the verify gate — exactly the funnel the
// timeout message steers away from.
func TestDispatchResume_RefusesGateExpiryRow(t *testing.T) {
	mlc := &mockLinearClient{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60},
		db:     &mockStore{},
		lc:     mlc,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	task := &store.AdmiralTask{
		State:           store.JobStateTimedOut,
		ClaudeSessionID: "sess-123",
		WorktreePath:    t.TempDir(),
		Error:           gateExpiryErrPrefix + " (attempts=2): context deadline exceeded",
	}
	o.dispatchResume(linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1", IssueIdentifier: "GEO-428"}, task)

	if body := mlc.GetPostedBody(); !strings.Contains(body, "verify gate") {
		t.Fatalf("posted body = %q, want a refusal naming the verify gate", body)
	}
}

// The budget that would have run the remote-branch cleanup died with the
// gate — a branch claude pushed against the prompt during fix-forward must
// still be removed, so the gate-expiry path cleans up under a fresh ctx.
func TestHandleGateFailure_BudgetExpiredCleansRemoteBranch(t *testing.T) {
	binDir := t.TempDir()
	gitLog := filepath.Join(binDir, "git.log")
	script := "#!/bin/sh\necho \"$@\" >> " + gitLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, _, _, f := gateFailFixture(t)
	f.branch = "linear/geo-428"
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if err := f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 2}); err == nil ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handleGateFailure err = %v, want wrapped DeadlineExceeded", err)
	}
	recorded, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatalf("fake git never ran: %v", err)
	}
	if !strings.Contains(string(recorded), "push origin --delete linear/geo-428") {
		t.Fatalf("fake git recorded %q, want the remote-branch cleanup", recorded)
	}
}

// Shutdown landing WHILE the gate-expiry path cleans up (before
// classification) must not leave the run interrupt-resumable: nothing is
// persisted yet, so runWithAttempt would mark it TIMED_OUT with the
// InterruptedByShutdown marker and the reconciler would auto-resume past
// the verify gate. The race branch settles FAILED instead.
func TestHandleGateFailure_ShutdownDuringExpiryCleanupLandsFailed(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	gitLog := filepath.Join(dir, "git.log")
	script := "#!/bin/sh\ntouch " + started + "\n" +
		"while [ ! -f " + release + " ]; do sleep 0.02; done\n" +
		"echo \"$@\" >> " + gitLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	o, ms, mlc, f := gateFailFixture(t)
	rootCtx, rootCancel := context.WithCancel(context.Background())
	o.rootCtx = rootCtx
	defer rootCancel()
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 2, LastBuildOutput: "boom"})
	}()

	// Wait until the cleanup's git push is running, then land the shutdown
	// inside the gate-expiry branch's cleanup window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake git never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rootCancel()
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatalf("release fake git: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("handleGateFailure err = nil, want non-nil")
	}
	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateFailed {
		t.Fatalf("admiral task state = %s, want FAILED (not interrupt-resumable past the gate)", got)
	}
	if body := mlc.GetPostedBody(); !strings.Contains(body, "/rerun") {
		t.Fatalf("posted body = %q, want rerun guidance", body)
	}
	// The expiry branch already ran the remote-branch cleanup; the shutdown
	// settle must not repeat it (a duplicate git push burns shutdown time).
	recorded, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatalf("fake git never ran: %v", err)
	}
	if got := strings.Count(string(recorded), "push origin --delete"); got != 1 {
		t.Fatalf("remote-branch cleanup ran %d times, want exactly once", got)
	}
}

// A genuine build failure (budget still alive) keeps the existing FAILED
// outcome: build-fail message on the thread, FAILED persisted, plain
// gateErrMsg returned.
func TestHandleGateFailure_BuildFailureStaysFailed(t *testing.T) {
	_, ms, mlc, f := gateFailFixture(t)

	err := f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 2, LastBuildOutput: "boom"})

	if err == nil || !strings.Contains(err.Error(), "build gate failed after 2 attempt(s)") {
		t.Fatalf("handleGateFailure err = %v, want gate-failure wording", err)
	}
	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateFailed {
		t.Fatalf("admiral task state = %s, want FAILED", got)
	}
	body := mlc.GetPostedBody()
	if !strings.Contains(body, "could not get the build to pass") ||
		!strings.Contains(body, "Verify command: `make check`") {
		t.Fatalf("posted body = %q, want build-failure report with verify command", body)
	}
}

// The default timeout message names the clock that fired: the claude
// phase's own MaxRunSeconds cap for a mid-claude kill (outer ctx alive),
// the run budget for a post-gate expiry (outer ctx dead). A gate-expiry
// timeout instead steers to /rerun with diagnostics: resuming skips the
// verify gate.
func TestTimedOutMessageNamesAppliedBudget(t *testing.T) {
	o := &Orchestrator{cfg: &config.Autopilot{MaxRunSeconds: 1800}}
	budget := runBudget(1800, 2, "make check") // 6 × 1920s

	// Claude-phase kill: outer budget still alive → per-phase cap. Claude
	// ran, so the session is recorded and the resume affordance applies.
	claudeKill := newFlow(o, context.Background(), linear.AgentEvent{})
	claudeKill.outerBudget = budget
	claudeKill.claudeSessionID = "sess-12345678"
	if s := claudeKill.timedOutMessage(); !strings.Contains(s, "after 1800s") {
		t.Fatalf("claude-phase timedOutMessage = %q, want MaxRunSeconds wording", s)
	}

	// Post-gate expiry: the outer deadline fired → the run budget. Claude
	// ran (session recorded) — the pre-claude wording must not hijack this.
	outerKill := newFlow(o, context.Background(), linear.AgentEvent{})
	outerKill.outerBudget = budget
	outerKill.claudeSessionID = "sess-12345678"
	var cancel context.CancelFunc
	outerKill.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if s := outerKill.timedOutMessage(); !strings.Contains(s, fmt.Sprintf("after %ds", int(budget.Seconds()))) {
		t.Fatalf("post-gate timedOutMessage = %q, want the applied run budget", s)
	}

	// Gate expiry: rerun guidance + diagnostics, no resume affordance.
	gated := newFlow(o, context.Background(), linear.AgentEvent{})
	gated.gateExpired = true
	gated.worktreePath = "/wt/linear-gxo-428"
	gated.gateExpiredBuildTail = "boom"
	s := gated.timedOutMessage()
	if !strings.Contains(s, "prefer /rerun") || !strings.Contains(s, "/wt/linear-gxo-428") || !strings.Contains(s, "boom") {
		t.Fatalf("gate-expired timedOutMessage = %q, want rerun guidance with worktree and build tail", s)
	}
	if strings.Contains(s, "Task timed out after") || strings.Contains(s, "resume from where I left off") {
		t.Fatalf("gate-expired timedOutMessage = %q, want no resume affordance", s)
	}

	// Gate expiry with no recorded session must not render a dangling
	// "(claude session: ...)" suffix.
	noSession := newFlow(o, context.Background(), linear.AgentEvent{})
	noSession.gateExpired = true
	noSession.worktreePath = "/wt/linear-gxo-428"
	if s := noSession.timedOutMessage(); strings.Contains(s, "(claude session:") {
		t.Fatalf("gate-expired timedOutMessage = %q, want no session suffix when claudeSessionID is empty", s)
	}

	// Resume-shaped flows on a gated repo (/fix, /resume: verifyCmd known
	// via bindIdentity, but no run budget applied) keep the historic
	// wording — "/rerun over /resume" is first-run guidance; for a /fix
	// patching an existing PR, /rerun would abandon the PR entirely.
	fixShaped := newFlow(o, context.Background(), linear.AgentEvent{})
	fixShaped.verifyCmd = "make check"
	fixShaped.claudeSessionID = "sess-12345678"
	if s := fixShaped.timedOutMessage(); !strings.Contains(s, "resume from where I left off") || strings.Contains(s, "prefer /rerun") {
		t.Fatalf("fix-shaped timedOutMessage = %q, want historic resume wording (the steer is first-run guidance)", s)
	}

	// Resume-shaped flow (no bindRepo) where claude ran: historic
	// MaxRunSeconds wording.
	plain := newFlow(o, context.Background(), linear.AgentEvent{})
	plain.claudeSessionID = "sess-12345678"
	if s := plain.timedOutMessage(); !strings.Contains(s, "after 1800s") {
		t.Fatalf("plain timedOutMessage = %q, want MaxRunSeconds wording", s)
	}

	// Claude never started (pre-run window deadline): no session to resume,
	// so no resume affordance — rerun guidance instead.
	pre := newFlow(o, context.Background(), linear.AgentEvent{})
	pre.outerBudget = 0
	if s := pre.timedOutMessage(); !strings.Contains(s, "/rerun") ||
		strings.Contains(s, "resume from where I left off") {
		t.Fatalf("pre-claude timedOutMessage = %q, want rerun guidance without resume affordance", s)
	}
}

// A row that already settled terminal (the gate's own FAILED write) must not
// be re-stamped and re-posted by markFailed — one diagnosis, one message.
// The worktree cleanup still runs.
func TestMarkFailed_SkipsAlreadyTerminalRow(t *testing.T) {
	ms := &mockStore{
		AdmiralTask:     &store.AdmiralTask{IssueID: "iss-1", State: store.JobStateFailed},
		LiveAdmiralTask: &store.AdmiralTask{IssueID: "iss-1", State: store.JobStateFailed},
	}
	mlc := &mockLinearClient{}
	o := &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60},
		db:     ms,
		lc:     mlc,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f := newFlow(o, context.Background(), linear.AgentEvent{IssueID: "iss-1", SessionID: "sess-1"})
	f.worktreePath = "" // cleanupWorktree no-ops; not under test here

	f.markFailed(errors.New("build gate failed after 2 attempt(s)"))

	if body := mlc.GetPostedBody(); body != "" {
		t.Fatalf("markFailed posted %q on an already-terminal row, want silence", body)
	}
	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateFailed {
		t.Fatalf("admiral task state = %s, want FAILED untouched", got)
	}
}

// A shutdown that lands mid-gate must NOT leave the run auto-resumable: the
// resume path skips the verify gate, so an interrupted row would be resumed
// by the reconciler straight past the gate. It persists FAILED with rerun
// guidance instead (markInterrupted no-ops on the terminal row).
func TestHandleGateFailure_ShutdownMidGatePersistsFailedNoAutoResume(t *testing.T) {
	o, ms, mlc, f := gateFailFixture(t)
	rootCtx, rootCancel := context.WithCancel(context.Background())
	o.rootCtx = rootCtx
	defer rootCancel()
	rootCancel() // shutdown before the gate outcome is decided
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithCancel(rootCtx)
	defer cancel()

	err := f.handleGateFailure("GEO-428", VerifyRetryResult{Attempts: 1, LastBuildOutput: "boom"})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handleGateFailure err = %v, want wrapped context.Canceled", err)
	}
	if got := ms.LiveAdmiralSnapshot().State; got != store.JobStateFailed {
		t.Fatalf("admiral task state = %s, want FAILED (not interrupt-resumable past the gate)", got)
	}
	if body := mlc.GetPostedBody(); !strings.Contains(body, "/rerun") {
		t.Fatalf("posted body = %q, want rerun guidance", body)
	}
}

// mockCIWatcher records mounts; Watching mirrors the live slot map so the
// reconciler's dedupe path is exercised against real lookups.
type mockCIWatcher struct {
	mu       sync.Mutex
	watching map[string]bool
	mounted  []string
}

func (m *mockCIWatcher) WatchPR(_ context.Context, prURL, _, _, _, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watching[prURL] = true
	m.mounted = append(m.mounted, prURL)
}

func (m *mockCIWatcher) Watching(prURL string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.watching[prURL]
}

func (m *mockCIWatcher) SetFixRound(fn func(context.Context, *store.AdmiralTask, string) error) {}

// The reconciler mounts watchers for watchable rows that have none, resolves
// each row's repo via Linear project, and is idempotent — a second pass over
// now-watched PRs mounts nothing. Wrong-thread notes are exactly why a
// duplicate must not reach WatchPR.
func TestReconcileCIWatchers_MountsUnwatchedIdempotently(t *testing.T) {
	ms := &mockStore{
		WatchableCITasks: []store.AdmiralTask{
			{IssueID: "iss-1", IssueIdentifier: "GEO-1", State: store.JobStateDone,
				PRURL: "https://github.com/x/y/pull/1", LastEventSessionID: "sess-1"},
			{IssueID: "iss-2", IssueIdentifier: "GEO-2", State: "DONE_THREAD_INCONSISTENT",
				PRURL: "https://github.com/x/y/pull/2", LastEventSessionID: "sess-2"},
		},
		Repo: &store.Repo{RepoDir: "/tmp/repo", BaseBranch: "main", GhUser: "someone", Enabled: true},
	}
	mlc := &mockLinearClient{GetIssueByID: map[string]*linear.Issue{
		"iss-1": {ID: "iss-1", ProjectID: "proj-1"},
		"iss-2": {ID: "iss-2", ProjectID: "proj-1"},
	}}
	mcw := &mockCIWatcher{watching: map[string]bool{}}
	o := &Orchestrator{
		cfg:       &config.Autopilot{MaxRunSeconds: 60},
		db:        ms,
		lc:        mlc,
		ciWatcher: mcw,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	o.reconcileCIWatchers()

	if got := len(mcw.mounted); got != 2 {
		t.Fatalf("mounted %d watchers (%v), want both unwatched PRs", got, mcw.mounted)
	}

	// Second pass: both PRs now have live watchers — nothing new mounts.
	o.reconcileCIWatchers()
	if got := len(mcw.mounted); got != 2 {
		t.Fatalf("re-mount happened on the second pass (%v), want idempotent", mcw.mounted)
	}
}

// A repo that cannot be resolved (missing Linear project, unregistered repo,
// disabled repo) is skipped with a warning, not mounted and not fatal.
func TestReconcileCIWatchers_SkipsUnresolvableRepo(t *testing.T) {
	ms := &mockStore{
		WatchableCITasks: []store.AdmiralTask{
			{IssueID: "iss-bad", IssueIdentifier: "GEO-9", State: store.JobStateDone,
				PRURL: "https://github.com/x/y/pull/9", LastEventSessionID: "sess-9"},
		},
		Repo: nil, // no repo registered for the project
	}
	mlc := &mockLinearClient{GetIssueByID: map[string]*linear.Issue{
		"iss-bad": {ID: "iss-bad", ProjectID: "proj-none"},
	}}
	mcw := &mockCIWatcher{watching: map[string]bool{}}
	o := &Orchestrator{
		cfg:       &config.Autopilot{MaxRunSeconds: 60},
		db:        ms,
		lc:        mlc,
		ciWatcher: mcw,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	o.reconcileCIWatchers()

	if got := len(mcw.mounted); got != 0 {
		t.Fatalf("mounted %d watchers for an unresolvable repo, want 0", got)
	}
}

// The compensator must reproduce the webhook enqueue contract exactly:
// source=github, PR URL in the session field, extractReviewBody-compatible
// payloads, deterministic gh-recon webhook IDs, and the bot loop guard —
// admiral's own reviews and comments must never be re-enqueued.
func TestProcessFeedback_EnqueueShapeAndLoopGuard(t *testing.T) {
	ms := &mockStore{}
	o := &Orchestrator{db: ms, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	row := store.AdmiralTask{IssueID: "iss-1", IssueIdentifier: "GEO-1",
		State: store.JobStateDone, PRURL: "https://github.com/x/y/pull/7"}

	reviews := []ghReview{
		{ID: 101, State: "CHANGES_REQUESTED", Body: "fix the loop", User: struct {
			Login string `json:"login"`
		}{Login: "reviewer1"}},
		{ID: 102, State: "PENDING", Body: "draft", User: struct {
			Login string `json:"login"`
		}{Login: "reviewer2"}},
		{ID: 103, State: "APPROVED", Body: "nice", User: struct {
			Login string `json:"login"`
		}{Login: "someone"}}, // ghUser set below to "SOMEONE" (case-insensitive guard)
	}
	comments := []ghComment{
		{ID: 201, Body: "this line is broken", User: struct {
			Login string `json:"login"`
		}{Login: "reviewer1"}},
		{ID: 202, Body: "self note", User: struct {
			Login string `json:"login"`
		}{Login: "someone"}},
	}
	o.processFeedback(row, "someone", reviews, comments)

	if len(ms.Enqueued) != 2 {
		t.Fatalf("enqueued %d events (%+v), want exactly 2 (self and PENDING filtered)", len(ms.Enqueued), ms.Enqueued)
	}
	var rev, com struct {
		WebhookID, Action, SessionID, Payload string
	}
	for _, e := range ms.Enqueued {
		if e.Source != "github" {
			t.Errorf("source = %q, want github", e.Source)
		}
		if e.SessionID != row.PRURL {
			t.Errorf("session = %q, want the PR URL (HandleReviewEvent reads it from there)", e.SessionID)
		}
		if strings.HasPrefix(e.Action, "pull_request_review_comment.") {
			com.WebhookID, com.Action, com.Payload = e.WebhookID, e.Action, e.Payload
		} else {
			rev.WebhookID, rev.Action, rev.Payload = e.WebhookID, e.Action, e.Payload
		}
	}
	if rev.WebhookID != "gh-recon-review-101" || rev.Action != "submitted" {
		t.Errorf("review event = %s/%s, want gh-recon-review-101/submitted", rev.WebhookID, rev.Action)
	}
	var rp struct {
		Review struct {
			Body  string `json:"body"`
			State string `json:"state"`
		} `json:"review"`
	}
	if err := json.Unmarshal([]byte(rev.Payload), &rp); err != nil {
		t.Fatalf("review payload unmarshal: %v", err)
	}
	if rp.Review.Body != "fix the loop" || rp.Review.State != "changes_requested" {
		t.Errorf("review payload = %+v, want body + lowercased state", rp.Review)
	}
	if com.WebhookID != "gh-recon-comment-201" || !strings.HasPrefix(com.Action, "pull_request_review_comment.") {
		t.Errorf("comment event = %s/%s, want gh-recon-comment-201 + comment action", com.WebhookID, com.Action)
	}
	var cp struct {
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
	}
	if err := json.Unmarshal([]byte(com.Payload), &cp); err != nil {
		t.Fatalf("comment payload unmarshal: %v", err)
	}
	if cp.Comment.Body != "this line is broken" {
		t.Errorf("comment body = %q, want the review comment body", cp.Comment.Body)
	}
}

// fetchRecentFeedback must parse gh api's reviews and comments arrays.
func TestFetchRecentFeedback_ParsesGhOutput(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	// POSIX sh only — CI's /bin/sh is dash, which rejects bash's [[ ]].
	script := `#!/bin/sh
case "$*" in
  *"/reviews"*) echo '[{"id":11,"state":"COMMENTED","body":"inline note","user":{"login":"rev"}}]' ;;
  *) echo '[{"id":22,"body":"thread body","user":{"login":"rev"}}]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	o := &Orchestrator{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	reviews, comments, err := o.fetchRecentFeedback(context.Background(), dir,
		"https://github.com/x/y/pull/7", os.Environ())
	if err != nil {
		t.Fatalf("fetchRecentFeedback: %v", err)
	}
	if len(reviews) != 1 || reviews[0].ID != 11 || reviews[0].Body != "inline note" || reviews[0].User.Login != "rev" {
		t.Fatalf("reviews = %+v, want the stubbed review", reviews)
	}
	if len(comments) != 1 || comments[0].ID != 22 || comments[0].Body != "thread body" {
		t.Fatalf("comments = %+v, want the stubbed comment", comments)
	}
}
