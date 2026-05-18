package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

	// GetDiff returns the unified diff for the PR as a single string.
	// The worker uses it to anchor inline review comments (path:line)
	// against the hunks it actually owns.
	GetDiff(ctx context.Context, prURL string) (string, error)
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

// PostComment shells out to `gh pr comment <url> -b <body>`. Both
// arguments must be non-empty (whitespace-only counts as empty) — an
// empty body would post a literally-empty comment on the PR, which is
// always a caller mistake.
func (c *Client) PostComment(ctx context.Context, prURL, body string) error {
	if strings.TrimSpace(prURL) == "" {
		return fmt.Errorf("github: PostComment: empty prURL")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("github: PostComment: empty body")
	}
	out, err := c.gh(ctx, "pr", "comment", prURL, "-b", body)
	if err != nil {
		return fmt.Errorf("gh pr comment %s: %w (output: %s)", prURL, err, truncate(out, 200))
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
	out, err := c.gh(ctx, "pr", "view", prURL, "--json", "state")
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

// GetDiff shells out to `gh pr diff <url>` and returns the raw unified
// diff. gh prints the diff to stdout on success and a short error
// message on failure; we return the full output verbatim so the caller
// can show it to the agent prompt without re-running.
func (c *Client) GetDiff(ctx context.Context, prURL string) (string, error) {
	if strings.TrimSpace(prURL) == "" {
		return "", fmt.Errorf("github: GetDiff: empty prURL")
	}
	out, err := c.gh(ctx, "pr", "diff", prURL)
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
