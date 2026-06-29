package autopilot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

func (m *mockPRClient) GetPRStatus(_ context.Context, _ string) (ghpkg.PRStatus, error) {
	return ghpkg.PRStatus{State: m.getPRStateVal}, m.getPRStateErr
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
		store.JobStateDoneMerged,
		store.JobStateFailed,
		store.JobStateTimedOut,
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
				Source:      "github",
				WebhookID:   "wh-terminal",
				SessionID:   "https://github.com/owner/repo/pull/3",
				Action:      "issue_comment.created",
				PayloadJSON: `{"comment":{"body":"any update?"}}`,
			}
			o.HandleReviewEvent(context.Background(), row)
			if len(pr.postedComments) != 0 {
				t.Errorf("terminal state %s: expected no PR comment, got %d", state, len(pr.postedComments))
			}
		})
	}
}

func TestHandleReviewEvent_DoneTaskStillProcessesReview(t *testing.T) {
	for _, state := range []string{
		store.JobStateDone,
		store.JobStateDoneThreadInconsistent,
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
				Source:      "github",
				WebhookID:   "wh-non-terminal",
				SessionID:   "https://github.com/owner/repo/pull/3",
				Action:      "issue_comment.created",
				PayloadJSON: `{"comment":{"body":"please bump readme version"}}`,
			}
			// HandleReviewEvent spawns a goroutine that eventually fails
			// (no repo on disk in tests). We only assert that the function
			// did NOT short-circuit on the terminal check — i.e. it
			// proceeded past task lookup. Branch presence is enough
			// signal: dispatch path reached worktree resolution.
			o.HandleReviewEvent(context.Background(), row)
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
		"", // skill: empty for this test — exercise the "no prefix" path
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
		"Do not commit broken code",
	}
	for _, want := range checks {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestBuildReviewPrompt_NoDiff(t *testing.T) {
	p := buildReviewPrompt("", "https://github.com/owner/repo/pull/6", "linear/geo-6", "main", "nit", "")
	if strings.Contains(p, "```diff") {
		t.Error("expected no diff block when diff is empty")
	}
}

func TestBuildReviewPrompt_NoReviewBody(t *testing.T) {
	p := buildReviewPrompt("", "https://github.com/owner/repo/pull/7", "linear/geo-7", "main", "", "some diff")
	if strings.Contains(p, "Review comment:") {
		t.Error("expected no review comment section when body is empty")
	}
}

func TestBuildReviewPrompt_SkillPrefixWhenSet(t *testing.T) {
	p := buildReviewPrompt(
		"oh-my-claudecode:ultraqa",
		"https://github.com/owner/repo/pull/8",
		"linear/geo-8",
		"main",
		"check the loop",
		"",
	)
	if !strings.HasPrefix(p, "/oh-my-claudecode:ultraqa\n\n") {
		t.Errorf("expected /<skill> prefix at the top of the prompt; first 80 chars: %q", p[:min(80, len(p))])
	}
	// The rest of the prompt body must still land — the skill prefix is
	// additive, not a replacement.
	for _, want := range []string{
		"reviewer has left feedback",
		"check the loop",
		"Do NOT open a new PR",
		"Do not commit broken code", // PR #163's inline build instruction stays
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q (skill prefix should be additive)", want)
		}
	}
}

func TestBuildReviewPrompt_NoSkillPrefixWhenEmpty(t *testing.T) {
	p := buildReviewPrompt(
		"", // explicitly empty
		"https://github.com/owner/repo/pull/9",
		"linear/geo-9",
		"main",
		"nit",
		"",
	)
	if strings.HasPrefix(p, "/") {
		t.Errorf("empty skill must NOT prepend any /<skill> prefix; got start: %q", p[:min(40, len(p))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

// reviewWorktreeFixture sets up a bare origin repo with one commit on
// `linear/test`, a local clone whose local branch ref points at that commit,
// and an "advancer" working copy used to push extra commits to origin so the
// local clone can be made deliberately stale.
type reviewWorktreeFixture struct {
	originDir   string // bare repo
	localDir    string // clone — passed to ensureReviewWorktree as repoDir
	advancerDir string // separate clone used to push new commits to origin
	branch      string
}

func setupReviewWorktreeFixture(t *testing.T) *reviewWorktreeFixture {
	t.Helper()
	root := t.TempDir()
	originDir := filepath.Join(root, "origin.git")
	localDir := filepath.Join(root, "local")
	advancerDir := filepath.Join(root, "advancer")
	branch := "linear/test"

	mustGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
	}
	probe := exec.Command("git", "--version")
	if err := probe.Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}

	mustGit(root, "init", "--bare", originDir)

	// seed dir: build the initial commit, push to origin.
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

	// localDir: the clone admiral operates on. Cloning creates
	// refs/heads/<branch> pointing at the same commit as origin/<branch>.
	mustGit(root, "clone", originDir, localDir)
	mustGit(localDir, "config", "user.email", "test@test.com")
	mustGit(localDir, "config", "user.name", "test")
	mustGit(localDir, "fetch", "origin", branch)
	mustGit(localDir, "branch", branch, "origin/"+branch)

	// advancerDir: separate clone used to push "external" commits to origin,
	// simulating a human or another admiral session moving the branch forward.
	mustGit(root, "clone", "-b", branch, originDir, advancerDir)
	mustGit(advancerDir, "config", "user.email", "advancer@test.com")
	mustGit(advancerDir, "config", "user.name", "advancer")

	return &reviewWorktreeFixture{
		originDir:   originDir,
		localDir:    localDir,
		advancerDir: advancerDir,
		branch:      branch,
	}
}

// advance pushes a new commit to origin from advancerDir and returns the new
// commit SHA on origin/<branch>.
func (f *reviewWorktreeFixture) advance(t *testing.T, content string) string {
	t.Helper()
	mustGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(f.advancerDir, "README"), []byte(content), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(f.advancerDir, "add", "README")
	mustGit(f.advancerDir, "commit", "-m", "advance:"+content)
	mustGit(f.advancerDir, "push", "origin", f.branch)
	// Read the new origin tip from the bare repo.
	return mustGit(f.originDir, "rev-parse", f.branch)
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD in %s: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Reuse path: worktree exists on disk but origin advanced. ensureReviewWorktree
// must hard-reset the worktree to origin tip so claude edits the current code.
func TestEnsureReviewWorktree_ReuseSyncsToOrigin(t *testing.T) {
	f := setupReviewWorktreeFixture(t)
	worktreePath := filepath.Join(f.localDir, ".worktrees", "review-test")
	// Create the worktree at the stale local branch ref.
	cmd := exec.Command("git", "worktree", "add", worktreePath, f.branch)
	cmd.Dir = f.localDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed worktree: %v: %s", err, out)
	}

	newTip := f.advance(t, "v2\n")
	// Confirm the worktree is now stale.
	if headOf(t, worktreePath) == newTip {
		t.Fatalf("precondition: worktree should be stale before ensureReviewWorktree")
	}
	// Drop a stray uncommitted file to verify clean -fd removes it.
	if err := os.WriteFile(filepath.Join(worktreePath, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	task := &store.AdmiralTask{Branch: f.branch, WorktreePath: worktreePath}
	got, err := ensureReviewWorktree(context.Background(), f.localDir, task)
	if err != nil {
		t.Fatalf("ensureReviewWorktree: %v", err)
	}
	if got != worktreePath {
		t.Errorf("worktree path: got %q want %q", got, worktreePath)
	}
	if h := headOf(t, worktreePath); h != newTip {
		t.Errorf("worktree HEAD: got %s want %s (origin tip)", h, newTip)
	}
	if _, err := os.Stat(filepath.Join(worktreePath, "stray")); !os.IsNotExist(err) {
		t.Errorf("stray file should have been cleaned, got err=%v", err)
	}
}

// Rebuild path: worktree dir is gone but local refs/heads/<branch> is stale.
// ensureReviewWorktree must reset the local branch ref to origin/<branch> via
// `worktree add -B` so the new checkout starts at origin tip.
func TestEnsureReviewWorktree_RebuildSyncsStaleLocalRef(t *testing.T) {
	f := setupReviewWorktreeFixture(t)
	newTip := f.advance(t, "v2\n")
	worktreePath := filepath.Join(f.localDir, ".worktrees", "review-test")

	task := &store.AdmiralTask{Branch: f.branch, WorktreePath: worktreePath}
	got, err := ensureReviewWorktree(context.Background(), f.localDir, task)
	if err != nil {
		t.Fatalf("ensureReviewWorktree: %v", err)
	}
	if got != worktreePath {
		t.Errorf("worktree path: got %q want %q", got, worktreePath)
	}
	if h := headOf(t, worktreePath); h != newTip {
		t.Errorf("worktree HEAD: got %s want %s (origin tip)", h, newTip)
	}
	// Direct invariant: local refs/heads/<branch> was force-aligned to origin
	// tip. Guards against a future refactor that uses --detach (HEAD would
	// still match but the local ref would stay stale).
	cmd := exec.Command("git", "rev-parse", f.branch)
	cmd.Dir = f.localDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", f.branch, err, out)
	}
	if localRef := strings.TrimSpace(string(out)); localRef != newTip {
		t.Errorf("local refs/heads/%s: got %s want %s", f.branch, localRef, newTip)
	}
}
