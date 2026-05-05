package autopilot

import (
	"context"
	"sync"
	"testing"

	"github.com/georgehuang/admiral/internal/linear"
)

// mockReplierClient is a mock linearClientPoster for testing.
type mockReplierClient struct {
	mu      sync.Mutex
	posts   []postCall
	lastErr error
}

type postCall struct {
	SessionID string
	Activity  linear.AgentActivity
}

func (m *mockReplierClient) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, postCall{SessionID: sessionID, Activity: a})
	return m.lastErr
}

func TestAgentSessionReplier_Ack(t *testing.T) {
	mc := &mockReplierClient{}
	r := NewAgentSessionReplier(mc)

	err := r.Ack(context.Background(), "sess-1", "noted, still working")
	if err != nil {
		t.Fatalf("Ack() = %v, want nil", err)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(mc.posts))
	}
	if mc.posts[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", mc.posts[0].SessionID, "sess-1")
	}
	if mc.posts[0].Activity.Type != linear.ActivityThought {
		t.Errorf("Activity.Type = %v, want %v", mc.posts[0].Activity.Type, linear.ActivityThought)
	}
	if mc.posts[0].Activity.Body != "noted, still working" {
		t.Errorf("Activity.Body = %q, want %q", mc.posts[0].Activity.Body, "noted, still working")
	}
	if mc.posts[0].Activity.Ephemeral {
		t.Errorf("Activity.Ephemeral = true, want false")
	}
}

func TestAgentSessionReplier_Reply(t *testing.T) {
	mc := &mockReplierClient{}
	r := NewAgentSessionReplier(mc)

	err := r.Reply(context.Background(), "sess-1", "Done. PR opened.")
	if err != nil {
		t.Fatalf("Reply() = %v, want nil", err)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(mc.posts))
	}
	if mc.posts[0].Activity.Type != linear.ActivityResponse {
		t.Errorf("Activity.Type = %v, want %v", mc.posts[0].Activity.Type, linear.ActivityResponse)
	}
	if mc.posts[0].Activity.Body != "Done. PR opened." {
		t.Errorf("Activity.Body = %q, want %q", mc.posts[0].Activity.Body, "Done. PR opened.")
	}
}

func TestAgentSessionReplier_Fail(t *testing.T) {
	mc := &mockReplierClient{}
	r := NewAgentSessionReplier(mc)

	err := r.Fail(context.Background(), "sess-1", "admiral failed: something went wrong")
	if err != nil {
		t.Fatalf("Fail() = %v, want nil", err)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(mc.posts))
	}
	if mc.posts[0].Activity.Type != linear.ActivityError {
		t.Errorf("Activity.Type = %v, want %v", mc.posts[0].Activity.Type, linear.ActivityError)
	}
	if mc.posts[0].Activity.Body != "admiral failed: something went wrong" {
		t.Errorf("Activity.Body = %q, want %q", mc.posts[0].Activity.Body, "admiral failed: something went wrong")
	}
}

func TestAgentSessionReplier_Progress(t *testing.T) {
	mc := &mockReplierClient{}
	r := NewAgentSessionReplier(mc)

	err := r.Progress(context.Background(), "sess-1", "Reading issue context...", true)
	if err != nil {
		t.Fatalf("Progress() = %v, want nil", err)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(mc.posts))
	}
	if mc.posts[0].Activity.Type != linear.ActivityThought {
		t.Errorf("Activity.Type = %v, want %v", mc.posts[0].Activity.Type, linear.ActivityThought)
	}
	if mc.posts[0].Activity.Body != "Reading issue context..." {
		t.Errorf("Activity.Body = %q, want %q", mc.posts[0].Activity.Body, "Reading issue context...")
	}
	if !mc.posts[0].Activity.Ephemeral {
		t.Errorf("Activity.Ephemeral = false, want true")
	}
}

func TestAgentSessionReplier_RecordAction(t *testing.T) {
	mc := &mockReplierClient{}
	r := NewAgentSessionReplier(mc)

	err := r.RecordAction(context.Background(), "sess-1", "worktree_create", "branch-x @ main", "")
	if err != nil {
		t.Fatalf("RecordAction() = %v, want nil", err)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(mc.posts))
	}
	if mc.posts[0].Activity.Type != linear.ActivityAction {
		t.Errorf("Activity.Type = %v, want %v", mc.posts[0].Activity.Type, linear.ActivityAction)
	}
	if mc.posts[0].Activity.Action != "worktree_create" {
		t.Errorf("Activity.Action = %q, want %q", mc.posts[0].Activity.Action, "worktree_create")
	}
	if mc.posts[0].Activity.Parameter != "branch-x @ main" {
		t.Errorf("Activity.Parameter = %q, want %q", mc.posts[0].Activity.Parameter, "branch-x @ main")
	}
}

func TestPostActivityWithRetry_Success(t *testing.T) {
	calls := 0
	fp := &funcPoster{
		fn: func(ctx context.Context, sessionID string, a linear.AgentActivity) error {
			calls++
			if calls < 4 {
				return assertErr{}
			}
			return nil
		},
	}

	err := postActivityWithRetry(context.Background(), fp, "sess-1", linear.Response("test"))
	if err != nil {
		t.Fatalf("postActivityWithRetry() = %v, want nil", err)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want 4", calls)
	}
}

type funcPoster struct {
	fn func(ctx context.Context, sessionID string, a linear.AgentActivity) error
}

func (f *funcPoster) PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error {
	return f.fn(ctx, sessionID, a)
}

// assertErr is a error type for testing retries.
type assertErr struct{}

func (e assertErr) Error() string { return "assertion failed" }