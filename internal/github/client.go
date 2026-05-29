package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PRClient is the outbound API admiral uses to talk to a PR on GitHub.
// PR #4 (review-followup dispatcher) depends on this surface; defining
// it here lets the dispatcher mock the client without taking on a gh
// CLI dependency in its own tests.
type PRClient interface {
	// PostComment posts a top-level conversation comment on the PR.
	// Used for the worker's "fixed in <sha>" / "won't change because X"
	// final reply path. Does not resolve review threads or reply inside
	// a specific review — those operations live on a future extension.
	PostComment(ctx context.Context, prURL, body string) error

	// GetPRState returns the lifecycle state of the PR as gh reports it:
	// "OPEN", "MERGED", "CLOSED", or "" when gh can't resolve the URL.
	// Errors are reserved for unexpected failures (network, gh missing,
	// auth broken); a missing PR is not an error.
	GetPRState(ctx context.Context, prURL string) (string, error)

	// GetPRStatus returns the lifecycle snapshot admiral-discoverer
	// needs to drive the Linear-state advancement: state plus merge
	// timestamp plus whether any latest review has approved. Returns
	// (PRStatus{}, nil) when gh can't resolve the PR.
	GetPRStatus(ctx context.Context, prURL string) (PRStatus, error)

	// GetDiff returns the unified diff for the PR as a single string.
	// The worker uses it to anchor inline review comments (path:line)
	// against the hunks it actually owns.
	GetDiff(ctx context.Context, prURL string) (string, error)
}

// PRStatus is the discoverer's snapshot of a PR's lifecycle.
type PRStatus struct {
	// State is "OPEN" / "MERGED" / "CLOSED" (empty when PR not found).
	State string
	// MergedAt is the merge timestamp (RFC3339) when state=="MERGED",
	// empty otherwise.
	MergedAt string
	// HasApprovedReview is true when the PR has at least one current
	// approval review (latestReviews .state == "APPROVED").
	HasApprovedReview bool
}

// runner is the indirection that lets tests substitute the gh subprocess
// without monkey-patching exec.Command. extraEnv augments the parent
// process's environment for this invocation only — e.g. GH_TOKEN to pin
// auth to admiral's bot identity, independent of any pre-existing gh
// auth state on the host.
type runner func(ctx context.Context, extraEnv []string, name string, args ...string) (string, error)

// Client talks to GitHub by shelling out to the `gh` CLI. Mirrors the
// pattern in internal/autopilot/ghprobe.go (the read-only probe used
// during orchestrator pre-flight). admiral's deployment always has gh
// available because claude itself depends on it.
type Client struct {
	bin    string
	token  string
	runCmd runner
}

// NewClient builds a Client that authenticates via the given personal
// access token. token may be empty to inherit whatever auth `gh` finds
// on the host (mainly useful for tests and one-off CLI smoke runs).
// In production, pass a non-empty token so admiral's identity is
// explicit and independent of host gh auth state — see admiral's
// gh-accounts multi-account discipline for why.
func NewClient(token string) *Client {
	return &Client{
		bin:    "gh",
		token:  token,
		runCmd: realRunCmd,
	}
}

// realRunCmd is the production runner. It composes the child's
// environment as os.Environ() + extraEnv so a passed GH_TOKEN overrides
// any inherited one, and captures combined stdout/stderr because gh
// emits API errors on stderr.
func realRunCmd(ctx context.Context, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *Client) gh(ctx context.Context, args ...string) (string, error) {
	var extraEnv []string
	if c.token != "" {
		extraEnv = []string{"GH_TOKEN=" + c.token}
	}
	return c.runCmd(ctx, extraEnv, c.bin, args...)
}

// ghReadRetryDelays is the backoff schedule for transient `gh` read
// failures. Matched to internal/linear's retryHTTP shape so admiral's
// two upstream APIs (GitHub via gh, Linear via direct HTTP) recover
// from blips on the same cadence. Exposed as a var so tests can
// override to zero-duration without sleeping the test suite.
var ghReadRetryDelays = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// ghReadRetry wraps gh() with bounded backoff retry for transient
// GitHub failures (TCP reset on a pooled keep-alive, brief 5xx, etc).
// Safe ONLY for idempotent GET-style operations — never call from
// `gh pr comment` and friends: POSTs that fail with `unexpected EOF`
// may have already succeeded server-side, and retrying would
// double-create the resource.
func (c *Client) ghReadRetry(ctx context.Context, args ...string) (string, error) {
	var (
		out     string
		lastErr error
	)
	for attempt := 0; attempt <= len(ghReadRetryDelays); attempt++ {
		out, lastErr = c.gh(ctx, args...)
		if lastErr == nil {
			return out, nil
		}
		if !isTransientGhFailure(out, lastErr) || attempt == len(ghReadRetryDelays) {
			return out, lastErr
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(ghReadRetryDelays[attempt]):
		}
	}
	return out, lastErr
}

// isTransientGhFailure inspects gh's combined output for patterns
// that suggest a retry is worthwhile. gh itself does not classify
// these — it just exits non-zero and prints the upstream error
// verbatim, so we pattern-match on the substring shapes admiral has
// actually seen in production (or that GitHub documents as transient).
//
// Patterns covered (case-insensitive substring on combined output):
//   - "unexpected EOF"     — server-side TCP reset on pooled keep-alive
//     (Go HTTP transport: mid-response body cut)
//   - `": EOF"`            — plain io.EOF, the connection was closed
//     before the response started. Go formats this as
//     `Get "<URL>": EOF`. Anchored on the closing quote + colon to
//     avoid false-positives on the literal word "EOF" appearing
//     elsewhere in gh output (e.g. in commit messages).
//   - "connection reset"   — RST mid-stream
//   - "i/o timeout"        — request deadline exceeded at transport
//   - "503"/"502"/"504"    — gateway/availability hiccups
//   - "rate limit"         — API rate limit (backoff helps modestly)
//
// Deliberately NOT covered: HTTP 401/403/404 and gh's
// "could not resolve" — those are permanent and a retry just wastes
// time. The PR-not-resolvable path is already handled at the call
// site (isPRNotResolvable), so we don't need to also filter it here.
func isTransientGhFailure(output string, err error) bool {
	if err == nil {
		return false
	}
	hay := strings.ToLower(output)
	for _, needle := range []string{
		"unexpected eof",
		`": eof`,
		"connection reset",
		"i/o timeout",
		"503",
		"502",
		"504",
		"rate limit",
	} {
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}

// PostComment shells out to `gh pr comment <url> -b <body>`. Both
// arguments must be non-empty (whitespace-only counts as empty) — an
// empty body would post a literally-empty comment on the PR, which is
// always a caller mistake.
//
// Every posted body is prefixed with the self-comment sentinel (a
// hidden HTML comment) so the inbound webhook can drop self-triggered
// events deterministically — see webhook.go. The sentinel is
// invisible in the rendered GitHub UI. Bodies already carrying the
// sentinel are not re-prefixed.
func (c *Client) PostComment(ctx context.Context, prURL, body string) error {
	if strings.TrimSpace(prURL) == "" {
		return fmt.Errorf("github: PostComment: empty prURL")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("github: PostComment: empty body")
	}
	if !hasSelfCommentSentinel(body) {
		body = selfCommentSentinel + "\n" + body
	}
	out, err := c.gh(ctx, "pr", "comment", prURL, "-b", body)
	if err != nil {
		return fmt.Errorf("gh pr comment %s: %w (output: %s)", prURL, err, truncate(out, 200))
	}
	return nil
}

// PostReview shells out to `gh pr review <url> --<verdict> [--body <body>]`.
// verdict must be one of "approve", "request_changes", "comment" — these
// map to the corresponding gh CLI flags. body is required for
// request_changes and comment (gh enforces this); for approve it is
// optional. Used by admiral-planner-mcp's pr_verify_submit tool to
// finalize an L1 verification on GitHub.
//
// Uses c.gh (not c.ghReadRetry) on purpose: review submission is a
// write that GitHub does not deduplicate, so an automatic retry on
// transient failure could double-post a review. Matches PostComment's
// stance. Callers that want resilience layer it themselves —
// pr_verify_submit checks the latest stored verdict before re-issuing,
// which lets the host agent retry safely with the same arguments.
func (c *Client) PostReview(ctx context.Context, prURL, verdict, body string) error {
	if strings.TrimSpace(prURL) == "" {
		return fmt.Errorf("github: PostReview: empty prURL")
	}
	var flag string
	switch verdict {
	case "approve":
		flag = "--approve"
	case "request_changes":
		flag = "--request-changes"
	case "comment":
		flag = "--comment"
	default:
		return fmt.Errorf("github: PostReview: unknown verdict %q (want approve | request_changes | comment)", verdict)
	}
	if (verdict == "request_changes" || verdict == "comment") && strings.TrimSpace(body) == "" {
		return fmt.Errorf("github: PostReview: body required for verdict %q", verdict)
	}
	args := []string{"pr", "review", prURL, flag}
	if body != "" {
		args = append(args, "--body", body)
	}
	out, err := c.gh(ctx, args...)
	if err != nil {
		return fmt.Errorf("gh pr review %s: %w (output: %s)", prURL, err, truncate(out, 200))
	}
	return nil
}

// GetPRState shells out to `gh pr view <url> --json state` and returns
// the State field. When gh can't resolve the PR (deleted / wrong repo /
// permissions), gh exits non-zero with a "could not resolve" message —
// treat that as state="" so the caller can fall through to a "PR not
// available" reply rather than a hard error.
func (c *Client) GetPRState(ctx context.Context, prURL string) (string, error) {
	if strings.TrimSpace(prURL) == "" {
		return "", fmt.Errorf("github: GetPRState: empty prURL")
	}
	out, err := c.ghReadRetry(ctx, "pr", "view", prURL, "--json", "state")
	if err != nil {
		if isPRNotResolvable(out) {
			return "", nil
		}
		return "", fmt.Errorf("gh pr view %s: %w (output: %s)", prURL, err, truncate(out, 200))
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parse gh pr view output: %w (raw: %s)", err, truncate(out, 200))
	}
	return v.State, nil
}

// GetPRStatus shells out to `gh pr view <url> --json state,mergedAt,latestReviews`
// and assembles a snapshot useful for Linear-state advancement.
// Returns (PRStatus{}, nil) when gh cannot resolve the PR (mirrors
// GetPRState's contract).
func (c *Client) GetPRStatus(ctx context.Context, prURL string) (PRStatus, error) {
	if strings.TrimSpace(prURL) == "" {
		return PRStatus{}, fmt.Errorf("github: GetPRStatus: empty prURL")
	}
	out, err := c.ghReadRetry(ctx, "pr", "view", prURL, "--json", "state,mergedAt,latestReviews")
	if err != nil {
		if isPRNotResolvable(out) {
			return PRStatus{}, nil
		}
		return PRStatus{}, fmt.Errorf("gh pr view %s: %w (output: %s)", prURL, err, truncate(out, 200))
	}
	var v struct {
		State         string `json:"state"`
		MergedAt      string `json:"mergedAt"`
		LatestReviews []struct {
			State string `json:"state"`
		} `json:"latestReviews"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return PRStatus{}, fmt.Errorf("parse gh pr view output: %w (raw: %s)", err, truncate(out, 200))
	}
	status := PRStatus{State: v.State, MergedAt: v.MergedAt}
	for _, r := range v.LatestReviews {
		if strings.EqualFold(r.State, "APPROVED") {
			status.HasApprovedReview = true
			break
		}
	}
	return status, nil
}

// GetDiff shells out to `gh pr diff <url>` and returns the raw unified
// diff. gh prints the diff to stdout on success and a short error
// message on failure; we return the full output verbatim so the caller
// can show it to the agent prompt without re-running.
func (c *Client) GetDiff(ctx context.Context, prURL string) (string, error) {
	if strings.TrimSpace(prURL) == "" {
		return "", fmt.Errorf("github: GetDiff: empty prURL")
	}
	out, err := c.ghReadRetry(ctx, "pr", "diff", prURL)
	if err != nil {
		return "", fmt.Errorf("gh pr diff %s: %w (output: %s)", prURL, err, truncate(out, 200))
	}
	return out, nil
}

// isPRNotResolvable inspects a gh failure body for the canonical
// "could not resolve" signatures gh prints for `pr view` against a
// deleted or wrong-repo PR, so a missing PR gets normalized to
// (state="", err=nil) rather than a hard error.
func isPRNotResolvable(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "could not resolve to a pullrequest") ||
		strings.Contains(low, "graphql: could not resolve")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
