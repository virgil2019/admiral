// Package autopilot is the orchestrator: pick up a Linear AgentSessionEvent,
// create a worktree, run `claude -p`, ensure a PR was opened, post agent
// activities back into the Linear agent thread.
package autopilot

import (
	"context"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
)

// AgentSessionReplier is the semantic reply layer for Linear AgentSession
// threads. It maps business intent (Ack, Reply, Fail, Progress,
// RecordAction) to the correct Linear AgentActivityType, wrapping the HTTP
// call and retry behavior so callers don't need to know protocol details.
//
// Session terminality
//
//   - Response and ErrorActivity are TERMINAL: they mark the session as
//     completed or errored. Once posted, no further non-ephemeral activities
//     should be sent on the same session.
//   - Thought, Action, and Elicitation are NON-TERMINAL: they keep the
//     session alive. Use these for progress updates, acknowledgements, and
//     tool-use records during an in-flight flow.
//
// Intent → type mapping
//
//   - Ack         → Thought (non-terminal; session stays active)
//   - Reply       → Response (terminal-success; session marked completed)
//   - Fail        → ErrorActivity (terminal-failure; session marked errored)
//   - Progress    → Thought (non-terminal; optionally ephemeral)
//   - RecordAction → Action (non-terminal; structured step event)
type AgentSessionReplier interface {
	// Ack sends a non-terminal acknowledgement. Use for: "noted, still
	// working", bare @mention on busy task, rejection-class responses
	// during an in-flight task.
	Ack(ctx context.Context, sessionID, body string) error

	// Reply sends a terminal-success response. Use for: task finished,
	// posting the final answer, final status message.
	Reply(ctx context.Context, sessionID, body string) error

	// Fail sends a terminal-failure error activity. Use for: task aborted
	// with error, or rejection on a path with no live session to protect
	// (assignFirstHelp, terminal-state /fix rejections).
	Fail(ctx context.Context, sessionID, body string) error

	// Progress sends a non-terminal status update. Use for: streaming
	// "Reading issue context...", "Resuming...". Set ephemeral=true for
	// transient UI updates that shouldn't persist in the thread.
	Progress(ctx context.Context, sessionID, body string, ephemeral bool) error

	// RecordAction records a structured tool-use or step event.
	RecordAction(ctx context.Context, sessionID, action, parameter, result string) error
}

// linearClientPoster is the subset of linear.Client needed by the replier.
type linearClientPoster interface {
	PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error
}

// agentSessionReplier is the concrete implementation of AgentSessionReplier.
type agentSessionReplier struct {
	lc linearClientPoster
}

// NewAgentSessionReplier creates a replier backed by the given linear client.
func NewAgentSessionReplier(lc linearClientPoster) *agentSessionReplier {
	return &agentSessionReplier{lc: lc}
}

// replyCtx is a short context for single reply calls that don't need the
// full parent deadline. Callers doing multi-reply sequences (e.g. progress
// then Reply) should manage their own ctx.
func replyCtx() (context.Context, func()) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// The caller's ctx is intentionally not used here — replyCtx() bounds
// each reply at its own 15s deadline regardless of the caller's parent
// deadline (see replyCtx doc above). The interface keeps `ctx` in the
// signature so a future implementation can opt into the parent deadline
// without breaking callers.

func (r *agentSessionReplier) Ack(_ context.Context, sessionID, body string) error {
	ctx, cancel := replyCtx()
	defer cancel()
	return r.lc.PostAgentActivity(ctx, sessionID, linear.Thought(body, false))
}

func (r *agentSessionReplier) Reply(_ context.Context, sessionID, body string) error {
	ctx, cancel := replyCtx()
	defer cancel()
	return r.lc.PostAgentActivity(ctx, sessionID, linear.Response(body))
}

func (r *agentSessionReplier) Fail(_ context.Context, sessionID, body string) error {
	ctx, cancel := replyCtx()
	defer cancel()
	return r.lc.PostAgentActivity(ctx, sessionID, linear.ErrorActivity(body))
}

func (r *agentSessionReplier) Progress(_ context.Context, sessionID, body string, ephemeral bool) error {
	ctx, cancel := replyCtx()
	defer cancel()
	return r.lc.PostAgentActivity(ctx, sessionID, linear.Thought(body, ephemeral))
}

func (r *agentSessionReplier) RecordAction(_ context.Context, sessionID, action, parameter, result string) error {
	ctx, cancel := replyCtx()
	defer cancel()
	return r.lc.PostAgentActivity(ctx, sessionID, linear.Action(action, parameter, result))
}

// postActivityWithRetry posts the activity with up to 3 retries using exponential
// backoff. If all attempts fail, it returns the last error.
func postActivityWithRetry(ctx context.Context, lc linearClientPoster, sessionID string, a linear.AgentActivity) error {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		err := lc.PostAgentActivity(ctx, sessionID, a)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < len(delays) {
			time.Sleep(delays[attempt])
		}
	}
	return lastErr
}
