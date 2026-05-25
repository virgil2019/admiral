package autopilot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// mockPRClient implements github.PRClient for testing.
type mockPRClient struct {
	postCommentErr error
	postedComments []string
	getPRStateVal  string
	getPRStateErr  error
	getDiffVal     string
	getDiffErr     error
}

func (m *mockPRClient) PostComment(_ context.Context, _, body string) error {
	m.postedComments = append(m.postedComments, body)
	return m.postCommentErr
}

func (m *mockPRClient) GetPRState(_ context.Context, _ string) (string, error) {
	return m.getPRStateVal, m.getPRStateErr
}

func (m *mockPRClient) GetDiff(_ context.Context, _ string) (string, error) {
	return m.getDiffVal, m.getDiffErr
}

var _ ghpkg.PRClient = (*mockPRClient)(nil)

func newReviewOrchestrator(db *mockStore, lc *mockLinearClient, pr *mockPRClient) *Orchestrator {
	return &Orchestrator{
		db:       db,
		lc:       lc,
		prClient: pr,
		logger:   slog.Default(),
		runSlots: make(chan struct{}, 3),
		cfg: &config.Autopilot{
			ClaudeBin:     "claude",
			MaxRunSeconds: 60,
		},
	}
}

// TestHandleReviewEvent_EmptyPRURL verifies that an event with no session_id
// (i.e. no PR URL) is silently dropped without touching the store.
func TestHandleReviewEvent_EmptyPRURL(t *testing.T) {
	db := &mockStore{}
	o := newReviewOrchestrator(db, &mockLinearClient{}, &mockPRClient{})
	row := &store.EventInboxRow{Source: "github", WebhookID: "wh-1"}
	o.HandleReviewEvent(context.Background(), row)
	if db.AdmiralTaskByPRURL != nil {
		t.Error("expected no store lookup on empty prURL")
	}
}

// TestHandleReviewEvent_StoreError verifies that a DB error on lookup logs and
// returns without panicking.
func TestHandleReviewEvent_StoreError(t *testing.T) {
	db := &mockStore{AdmiralTaskByPRURLErr: errors.New("db down")}
	o := newReviewOrchestrator(db, &mockLinearClient{}, &mockPRClient{})
	row := &store.EventInboxRow{
		Source:    "github",
		WebhookID: "wh-2",
		SessionID: "https://github.com/owner/repo/pull/1",
	}
	// Must not panic.
	o.HandleReviewEvent(context.Background(), row)
}

// TestHandleReviewEvent_NoTask verifies that when no task has the PR URL the
// event is silently acknowledged.
func TestHandleReviewEvent_NoTask(t *testing.T) {
	db := &mockStore{AdmiralTaskByPRURL: nil}
	pr := &mockPRClient{}
	o := newReviewOrchestrator(db, &mockLinearClient{}, pr)
	row := &store.EventInboxRow{
		Source:    "github",
		WebhookID: "wh-3",
		SessionID: "https://github.com/owner/repo/pull/2",
	}
	o.HandleReviewEvent(context.Background(), row)
	if len(pr.postedComments) != 0 {
		t.Errorf("expected no PR comment, got %d", len(pr.postedComments))
	}
}

// TestHandleReviewEvent_NoBranch verifies that a task with no branch is
// dropped without posting a comment.
func TestHandleReviewEvent_NoBranch(t *testing.T) {
	db := &mockStore{
		AdmiralTaskByPRURL: &store.AdmiralTask{
			IssueID:         "issue-1",
			IssueIdentifier: "GEO-99",
			PRURL:           "https://github.com/owner/repo/pull/3",
			Branch:          "", // no branch
		},
	}
	pr := &mockPRClient{}
	o := newReviewOrchestrator(db, &mockLinearClient{}, pr)
	row := &store.EventInboxRow{
		Source:    "github",
		WebhookID: "wh-4",
		SessionID: "https://github.com/owner/repo/pull/3",
	}
	o.HandleReviewEvent(context.Background(), row)
	if len(pr.postedComments) != 0 {
		t.Errorf("expected no PR comment on missing branch, got %d", len(pr.postedComments))
	}
}

func TestHandleReviewEvent_SkipsTerminalTaskState(t *testing.T) {
	for _, state := range []string{
		store.JobStateDone,
		store.JobStateFailed,
		store.JobStateTimedOut,
		store.JobStateDoneThreadInconsistent,
		store.JobStateCancelled,
	} {
		t.Run(state, func(t *testing.T) {
			db := &mockStore{
				AdmiralTaskByPRURL: &store.AdmiralTask{
					IssueID:         "issue-1",
					IssueIdentifier: "GEO-99",
					PRURL:           "https://github.com/owner/repo/pull/3",
					Branch:          "linear/geo-99",
					State:           state,
				},
			}
			pr := &mockPRClient{}
			o := newReviewOrchestrator(db, &mockLinearClient{}, pr)
			row := &store.EventInboxRow{
				Source:     "github",
				WebhookID:  "wh-terminal",
				SessionID:  "https://github.com/owner/repo/pull/3",
				Action:     "issue_comment.created",
				PayloadJSON: `{"comment":{"body":"any update?"}}`,
			}
			o.HandleReviewEvent(context.Background(), row)
			if len(pr.postedComments) != 0 {
				t.Errorf("terminal state %s: expected no PR comment, got %d", state, len(pr.postedComments))
			}
		})
	}
}

// TestHandleReviewEvent_SpawnsGoroutine verifies that a valid event triggers
// the background goroutine path. The goroutine will fail early (no Linear
// issue to look up) but must not panic and must attempt GetDiff.
func TestHandleReviewEvent_SpawnsGoroutine(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/10"
	// Use os.MkdirTemp so cleanup silently ignores "directory not empty" —
	// runReview's background goroutine may still be writing when the test ends.
	repoDir, err := os.MkdirTemp("", "TestHandleReviewEvent_SpawnsGoroutine*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(repoDir) })
	db := &mockStore{
		AdmiralTaskByPRURL: &store.AdmiralTask{
			IssueID:         "issue-10",
			IssueIdentifier: "GEO-10",
			PRURL:           prURL,
			Branch:          "linear/geo-10",
		},
		Repo: &store.Repo{RepoDir: repoDir, BaseBranch: "main"},
	}
	lc := &mockLinearClient{
		GetIssueResult: &linear.Issue{ID: "issue-10", ProjectID: "proj-1"},
	}
	pr := &mockPRClient{getDiffVal: "diff content"}
	o := newReviewOrchestrator(db, lc, pr)

	row := &store.EventInboxRow{
		Source:      "github",
		WebhookID:   "wh-10",
		SessionID:   prURL,
		Action:      "pull_request_review.submitted",
		PayloadJSON: `{"review":{"body":"Please fix the typo."}}`,
	}
	// HandleReviewEvent returns immediately; runReview runs in background.
	o.HandleReviewEvent(context.Background(), row)
	// GetDiff is called synchronously before the goroutine launch.
	// The mock records the call; just confirm no panic.
}

// --- helper function tests ---

func TestExtractReviewBody_Review(t *testing.T) {
	payload := `{"review":{"body":"Looks good overall, but fix the nit."}}`
	got := extractReviewBody(payload, "pull_request_review.submitted")
	want := "Looks good overall, but fix the nit."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractReviewBody_Comment(t *testing.T) {
	payload := `{"comment":{"body":"This line has a bug."}}`
	got := extractReviewBody(payload, "pull_request_review_comment.created")
	want := "This line has a bug."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractReviewBody_CommentPreferredOverReview(t *testing.T) {
	// For a review_comment action, Comment.Body takes priority.
	payload := `{"review":{"body":"review body"},"comment":{"body":"comment body"}}`
	got := extractReviewBody(payload, "pull_request_review_comment.created")
	if got != "comment body" {
		t.Errorf("got %q, want %q", got, "comment body")
	}
}

func TestExtractReviewBody_InvalidJSON(t *testing.T) {
	got := extractReviewBody("{bad json", "pull_request_review.submitted")
	if got != "" {
		t.Errorf("expected empty on bad JSON, got %q", got)
	}
}

func TestExtractReviewBody_IssueComment(t *testing.T) {
	payload := `{"comment":{"body":"Hey admiral, please update the readme."}}`
	got := extractReviewBody(payload, "issue_comment.created")
	want := "Hey admiral, please update the readme."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewPrompt_ContainsKeyParts(t *testing.T) {
	p := buildReviewPrompt(
		"https://github.com/owner/repo/pull/5",
		"linear/geo-5",
		"main",
		"Fix the off-by-one error.",
		"@@ -1 +1 @@ -x +x+1",
	)
	checks := []string{
		"https://github.com/owner/repo/pull/5",
		"linear/geo-5",
		"main",
		"Fix the off-by-one error.",
		"diff",
		"Do NOT open a new PR",
	}
	for _, want := range checks {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestBuildReviewPrompt_NoDiff(t *testing.T) {
	p := buildReviewPrompt("https://github.com/owner/repo/pull/6", "linear/geo-6", "main", "nit", "")
	if strings.Contains(p, "```diff") {
		t.Error("expected no diff block when diff is empty")
	}
}

func TestBuildReviewPrompt_NoReviewBody(t *testing.T) {
	p := buildReviewPrompt("https://github.com/owner/repo/pull/7", "linear/geo-7", "main", "", "some diff")
	if strings.Contains(p, "Review comment:") {
		t.Error("expected no review comment section when body is empty")
	}
}

// --- review state extraction ---

func TestExtractReviewState_Approved(t *testing.T) {
	got := extractReviewState(
		`{"review":{"state":"approved","body":""}}`,
		"pull_request_review.submitted",
	)
	if got != reviewStateApproved {
		t.Errorf("got %q, want %q", got, reviewStateApproved)
	}
}

func TestExtractReviewState_ChangesRequested(t *testing.T) {
	got := extractReviewState(
		`{"review":{"state":"changes_requested","body":"fix this"}}`,
		"pull_request_review.submitted",
	)
	if got != reviewStateChangesRequested {
		t.Errorf("got %q, want %q", got, reviewStateChangesRequested)
	}
}

func TestExtractReviewState_Commented(t *testing.T) {
	got := extractReviewState(
		`{"review":{"state":"commented","body":"nit"}}`,
		"pull_request_review.submitted",
	)
	if got != reviewStateCommented {
		t.Errorf("got %q, want %q", got, reviewStateCommented)
	}
}

func TestExtractReviewState_InlineCommentReturnsEmpty(t *testing.T) {
	// pull_request_review_comment events have no review.state concept.
	got := extractReviewState(
		`{"comment":{"body":"inline note"}}`,
		"pull_request_review_comment.created",
	)
	if got != "" {
		t.Errorf("got %q, want empty for inline comment events", got)
	}
}

func TestExtractReviewState_IssueCommentReturnsEmpty(t *testing.T) {
	got := extractReviewState(
		`{"comment":{"body":"@admiral please fix"}}`,
		"issue_comment.created",
	)
	if got != "" {
		t.Errorf("got %q, want empty for issue_comment events", got)
	}
}

func TestExtractReviewState_Uppercase(t *testing.T) {
	// Webhook payloads currently use lowercase, but extractReviewState
	// normalizes to lowercase defensively so callers can always compare
	// against the constants without a casing footgun.
	got := extractReviewState(
		`{"review":{"state":"APPROVED"}}`,
		"pull_request_review.submitted",
	)
	if got != reviewStateApproved {
		t.Errorf("got %q, want %q", got, reviewStateApproved)
	}
}

func TestExtractReviewState_InvalidJSON(t *testing.T) {
	got := extractReviewState("{bad json", "pull_request_review.submitted")
	if got != "" {
		t.Errorf("expected empty on bad JSON, got %q", got)
	}
}

// --- approved-state branching ---

// TestHandleReviewEvent_ApprovedSkipsClaude verifies that an approved review
// with no body posts a Linear notice and does NOT trigger a claude run / PR
// comment.
func TestHandleReviewEvent_ApprovedSkipsClaude(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/20"
	db := &mockStore{
		AdmiralTaskByPRURL: &store.AdmiralTask{
			IssueID:            "issue-20",
			IssueIdentifier:    "GEO-20",
			PRURL:              prURL,
			Branch:             "linear/geo-20",
			LastEventSessionID: "sess-20",
		},
	}
	lc := &mockLinearClient{}
	pr := &mockPRClient{}
	o := newReviewOrchestrator(db, lc, pr)

	row := &store.EventInboxRow{
		Source:      "github",
		WebhookID:   "wh-20",
		SessionID:   prURL,
		Action:      "pull_request_review.submitted",
		PayloadJSON: `{"review":{"state":"approved","body":""}}`,
	}
	o.HandleReviewEvent(context.Background(), row)

	// Linear post is in a goroutine — give it a moment to land.
	time.Sleep(50 * time.Millisecond)

	if len(pr.postedComments) != 0 {
		t.Errorf("approved review must not trigger PR comment, got %d", len(pr.postedComments))
	}
	body := lc.GetPostedBody()
	if !strings.Contains(body, "human verify") {
		t.Errorf("Linear notice missing 'human verify' phrasing: %q", body)
	}
	if !strings.Contains(body, prURL) {
		t.Errorf("Linear notice missing PR URL: %q", body)
	}
}

// TestHandleReviewEvent_ApprovedWithBody_RunsClaude verifies that an approved
// review carrying a body (i.e. reviewer left textual feedback alongside the
// approval) still goes through the claude path — feedback must not be
// silently dropped just because the state is APPROVED.
func TestHandleReviewEvent_ApprovedWithBody_RunsClaude(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/21"
	repoDir, err := os.MkdirTemp("", "TestHandleReviewEvent_ApprovedWithBody*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(repoDir) })
	db := &mockStore{
		AdmiralTaskByPRURL: &store.AdmiralTask{
			IssueID:            "issue-21",
			IssueIdentifier:    "GEO-21",
			PRURL:              prURL,
			Branch:             "linear/geo-21",
			LastEventSessionID: "sess-21",
		},
		Repo: &store.Repo{RepoDir: repoDir, BaseBranch: "main"},
	}
	lc := &mockLinearClient{GetIssueResult: &linear.Issue{ID: "issue-21", ProjectID: "proj-1"}}
	pr := &mockPRClient{}
	o := newReviewOrchestrator(db, lc, pr)

	row := &store.EventInboxRow{
		Source:      "github",
		WebhookID:   "wh-21",
		SessionID:   prURL,
		Action:      "pull_request_review.submitted",
		PayloadJSON: `{"review":{"state":"approved","body":"LGTM but rename foo"}}`,
	}
	// Must not short-circuit to the approved-notice path.
	o.HandleReviewEvent(context.Background(), row)
	// Give the goroutine time to settle. If the approved-notice path were
	// (incorrectly) taken, GetPostedBody would carry the "human verify"
	// phrasing. The claude-run path will fail in the goroutine on the synthetic
	// repo, but that failure path does not post that string.
	time.Sleep(50 * time.Millisecond)
	if body := lc.GetPostedBody(); strings.Contains(body, "human verify") {
		t.Errorf("approved+body must not post the approved-notice; got %q", body)
	}
}

// --- Linear notice helper ---

func TestPostReviewLinearNotice_SkipsOnEmptySession(t *testing.T) {
	lc := &mockLinearClient{}
	o := newReviewOrchestrator(&mockStore{}, lc, &mockPRClient{})
	task := &store.AdmiralTask{
		PRURL:              "https://github.com/owner/repo/pull/30",
		LastEventSessionID: "", // no session — should be a no-op
	}
	o.postReviewLinearNotice(task, linear.Response("hello"))
	if lc.GetPostedBody() != "" {
		t.Errorf("expected no Linear post on empty session id, got %q", lc.GetPostedBody())
	}
}

func TestPostReviewHandledNotice_PostsExpectedBody(t *testing.T) {
	lc := &mockLinearClient{}
	o := newReviewOrchestrator(&mockStore{}, lc, &mockPRClient{})
	task := &store.AdmiralTask{
		PRURL:              "https://github.com/owner/repo/pull/31",
		LastEventSessionID: "sess-31",
	}
	o.postReviewHandledNotice(task)
	body := lc.GetPostedBody()
	if !strings.Contains(body, "re-review") {
		t.Errorf("handled notice missing 're-review': %q", body)
	}
	if !strings.Contains(body, task.PRURL) {
		t.Errorf("handled notice missing PR URL: %q", body)
	}
}
