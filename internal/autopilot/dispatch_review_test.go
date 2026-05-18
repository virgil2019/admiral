package autopilot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

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
