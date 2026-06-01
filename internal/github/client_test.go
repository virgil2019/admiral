package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRunner captures one invocation's args + env so tests can assert
// on the exact `gh` command Client composes, and returns scripted
// stdout/err so each test exercises a specific code path.
type fakeRunner struct {
	gotName string
	gotArgs []string
	gotEnv  []string
	stdout  string
	err     error
}

func (f *fakeRunner) run(_ context.Context, extraEnv []string, name string, args ...string) (string, error) {
	f.gotName = name
	f.gotArgs = append([]string(nil), args...)
	f.gotEnv = append([]string(nil), extraEnv...)
	return f.stdout, f.err
}

func newTestClient(token string, fr *fakeRunner) *Client {
	return &Client{
		bin:    "gh",
		token:  token,
		runCmd: fr.run,
	}
}

func TestPostComment_Composition(t *testing.T) {
	fr := &fakeRunner{stdout: "https://github.com/x/y/pull/1#issuecomment-42\n"}
	c := newTestClient("tok-abc", fr)

	err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", "looks good")
	if err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if fr.gotName != "gh" {
		t.Errorf("name: got %q, want gh", fr.gotName)
	}
	wantBody := selfCommentSentinel + "\nlooks good"
	want := []string{"pr", "comment", "https://github.com/x/y/pull/1", "-b", wantBody}
	if !equalSlice(fr.gotArgs, want) {
		t.Errorf("args mismatch:\n got %v\nwant %v", fr.gotArgs, want)
	}
	if len(fr.gotEnv) != 1 || fr.gotEnv[0] != "GH_TOKEN=tok-abc" {
		t.Errorf("env: got %v, want [GH_TOKEN=tok-abc]", fr.gotEnv)
	}
}

func TestPostComment_PrependsSentinelOnce(t *testing.T) {
	// Calling PostComment with a body that already starts with the
	// sentinel must not double-prefix — that would leave a visible
	// stray sentinel in the rendered comment.
	fr := &fakeRunner{stdout: "ok"}
	c := newTestClient("tok", fr)

	pre := selfCommentSentinel + "\nalready prefixed"
	if err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", pre); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if len(fr.gotArgs) != 5 {
		t.Fatalf("args length: got %d, want 5", len(fr.gotArgs))
	}
	gotBody := fr.gotArgs[4]
	if gotBody != pre {
		t.Errorf("body should pass through verbatim when already sentinel-prefixed:\n got %q\nwant %q", gotBody, pre)
	}
	if strings.Count(gotBody, selfCommentSentinel) != 1 {
		t.Errorf("sentinel must appear exactly once, got %d in %q",
			strings.Count(gotBody, selfCommentSentinel), gotBody)
	}
}

func TestPostComment_PrependsSentinelDespiteLeadingWhitespace(t *testing.T) {
	// A body with leading whitespace then the sentinel is also
	// considered already-prefixed (TrimLeft on whitespace before
	// HasPrefix). This guards against accidental double-prefix when
	// callers compose multi-line bodies.
	fr := &fakeRunner{stdout: "ok"}
	c := newTestClient("tok", fr)

	pre := "  \n" + selfCommentSentinel + "\nbody"
	if err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", pre); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	gotBody := fr.gotArgs[4]
	if strings.Count(gotBody, selfCommentSentinel) != 1 {
		t.Errorf("sentinel must appear exactly once, got %d in %q",
			strings.Count(gotBody, selfCommentSentinel), gotBody)
	}
}

func TestPostComment_NoTokenSkipsEnv(t *testing.T) {
	fr := &fakeRunner{stdout: "ok"}
	c := newTestClient("", fr)

	if err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", "hi"); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if len(fr.gotEnv) != 0 {
		t.Errorf("env should be empty when token empty, got %v", fr.gotEnv)
	}
}

func TestPostComment_EmptyPRURLRejected(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	if err := c.PostComment(context.Background(), "", "body"); err == nil {
		t.Error("expected error on empty prURL")
	}
	if fr.gotName != "" {
		t.Errorf("gh must not be invoked on validation failure, got name %q", fr.gotName)
	}
}

func TestPostComment_EmptyBodyRejected(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	// Both "" and whitespace-only must fail — gh would otherwise post a
	// literally empty comment on the PR, which is always a caller mistake.
	for _, body := range []string{"", "   ", "\t\n"} {
		if err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", body); err == nil {
			t.Errorf("expected error for empty body %q", body)
		}
	}
	if fr.gotName != "" {
		t.Errorf("gh must not be invoked on validation failure, got name %q", fr.gotName)
	}
}

func TestPostComment_PropagatesRunnerError(t *testing.T) {
	fr := &fakeRunner{stdout: "HTTP 404: Not Found", err: errors.New("exit status 1")}
	c := newTestClient("tok", fr)
	err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", "body")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error must include gh output: %v", err)
	}
}

func TestGetPRState_ParsesJSON(t *testing.T) {
	fr := &fakeRunner{stdout: `{"state":"OPEN"}`}
	c := newTestClient("tok", fr)
	state, err := c.GetPRState(context.Background(), "https://github.com/x/y/pull/1")
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if state != "OPEN" {
		t.Errorf("state: got %q, want OPEN", state)
	}
	want := []string{"pr", "view", "https://github.com/x/y/pull/1", "--json", "state"}
	if !equalSlice(fr.gotArgs, want) {
		t.Errorf("args mismatch:\n got %v\nwant %v", fr.gotArgs, want)
	}
}

func TestGetPRState_NormalizesNotFound(t *testing.T) {
	fr := &fakeRunner{
		stdout: "graphql: Could not resolve to a PullRequest with the number of 999.",
		err:    errors.New("exit status 1"),
	}
	c := newTestClient("tok", fr)
	state, err := c.GetPRState(context.Background(), "https://github.com/x/y/pull/999")
	if err != nil {
		t.Fatalf("unresolvable PR should not error: %v", err)
	}
	if state != "" {
		t.Errorf("unresolvable PR state: got %q, want empty", state)
	}
}

func TestGetPRState_PropagatesOtherErrors(t *testing.T) {
	fr := &fakeRunner{
		stdout: "error connecting to github.com: timeout",
		err:    errors.New("exit status 1"),
	}
	c := newTestClient("tok", fr)
	if _, err := c.GetPRState(context.Background(), "https://github.com/x/y/pull/1"); err == nil {
		t.Error("expected error on network failure")
	}
}

func TestGetPRState_RejectsMalformedJSON(t *testing.T) {
	fr := &fakeRunner{stdout: `{state:OPEN}`}
	c := newTestClient("tok", fr)
	if _, err := c.GetPRState(context.Background(), "https://github.com/x/y/pull/1"); err == nil {
		t.Error("expected error on bad JSON")
	}
}

func TestGetPRState_EmptyPRURLRejected(t *testing.T) {
	c := newTestClient("tok", &fakeRunner{})
	if _, err := c.GetPRState(context.Background(), ""); err == nil {
		t.Error("expected error on empty prURL")
	}
}

func TestGetDiff_ReturnsRawOutput(t *testing.T) {
	raw := "diff --git a/foo.go b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	fr := &fakeRunner{stdout: raw}
	c := newTestClient("tok", fr)
	got, err := c.GetDiff(context.Background(), "https://github.com/x/y/pull/1")
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if got != raw {
		t.Errorf("diff output not passed through verbatim:\n got %q\nwant %q", got, raw)
	}
	want := []string{"pr", "diff", "https://github.com/x/y/pull/1"}
	if !equalSlice(fr.gotArgs, want) {
		t.Errorf("args mismatch:\n got %v\nwant %v", fr.gotArgs, want)
	}
}

func TestGetDiff_PropagatesError(t *testing.T) {
	fr := &fakeRunner{
		stdout: "no diff available",
		err:    errors.New("exit status 1"),
	}
	c := newTestClient("tok", fr)
	if _, err := c.GetDiff(context.Background(), "https://github.com/x/y/pull/1"); err == nil {
		t.Error("expected error")
	}
}

func TestGetDiff_EmptyPRURLRejected(t *testing.T) {
	c := newTestClient("tok", &fakeRunner{})
	if _, err := c.GetDiff(context.Background(), ""); err == nil {
		t.Error("expected error on empty prURL")
	}
}

func TestIsPRNotResolvable(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"GraphQL: Could not resolve to a PullRequest with the number of 1.", true},
		{"graphql: Could not resolve to a PullRequest", true},
		// Non-matching errors must propagate as real errors, not be silently
		// normalized — including any speculative wording (e.g. "no pull
		// requests found") that gh does not actually emit for `pr view`.
		{"no pull requests found for branch foo", false},
		{"HTTP 404", false},
		{"network timeout", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isPRNotResolvable(tc.out); got != tc.want {
			t.Errorf("isPRNotResolvable(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// seqRunner returns a scripted (stdout, err) per call and counts
// invocations. Used to exercise ghReadRetry's backoff path without
// real subprocess spawns.
type seqRunner struct {
	calls   int
	outputs []seqOutput
}

type seqOutput struct {
	stdout string
	err    error
}

func (s *seqRunner) run(_ context.Context, _ []string, _ string, _ ...string) (string, error) {
	idx := s.calls
	s.calls++
	if idx >= len(s.outputs) {
		// Past the scripted set — return the last one, so an
		// always-transient script can be expressed by repeating one
		// entry once.
		return s.outputs[len(s.outputs)-1].stdout, s.outputs[len(s.outputs)-1].err
	}
	return s.outputs[idx].stdout, s.outputs[idx].err
}

func newClientWithSeq(sr *seqRunner) *Client {
	return &Client{bin: "gh", runCmd: sr.run}
}

// withZeroRetryDelays nukes the backoff schedule for one test so
// retries don't actually sleep. Restored on cleanup.
func withZeroRetryDelays(t *testing.T) {
	t.Helper()
	orig := ghReadRetryDelays
	ghReadRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { ghReadRetryDelays = orig })
}

func TestIsTransientGhFailure(t *testing.T) {
	bang := errors.New("exit status 1")
	cases := []struct {
		name   string
		output string
		err    error
		want   bool
	}{
		{"nil err", "any output", nil, false},
		{"empty output", "", bang, false},
		{"unexpected EOF", `Get "https://api.github.com/...": unexpected EOF`, bang, true},
		{"uppercase EOF", "Unexpected EOF", bang, true},
		// Plain io.EOF — Go formats as `Get "URL": EOF` when the
		// connection is closed before the response starts. Distinct
		// from "unexpected EOF" which is mid-response.
		{"plain EOF", `could not find pull request diff: Get "https://api.github.com/repos/x/y/pulls/5": EOF`, bang, true},
		// EOF in a non-URL-terminator context must NOT match. The
		// regex anchor `": EOF` requires the closing quote + colon
		// + space immediately before EOF.
		{"EOF in commit message", `feat: handle EOF in parser`, bang, false},
		{"EOF in PR title", "EOF detection", bang, false},
		{"connection reset", "read tcp ... connection reset by peer", bang, true},
		{"i/o timeout", "Get ...: i/o timeout", bang, true},
		{"503", "HTTP 503: Service Unavailable", bang, true},
		{"502", "HTTP 502: Bad Gateway", bang, true},
		{"504", "HTTP 504: Gateway Timeout", bang, true},
		{"rate limit", "API rate limit exceeded", bang, true},
		// Permanent failures must NOT be retried.
		{"could not resolve", "GraphQL: Could not resolve to a PullRequest", bang, false},
		{"404 not found", "HTTP 404: Not Found", bang, false},
		{"401 unauthorized", "HTTP 401: Unauthorized", bang, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransientGhFailure(c.output, c.err); got != c.want {
				t.Errorf("IsTransientGhFailure(%q, %v) = %v, want %v",
					c.output, c.err, got, c.want)
			}
		})
	}
}

func TestGhReadRetry_SucceedsFirstTry(t *testing.T) {
	withZeroRetryDelays(t)
	sr := &seqRunner{outputs: []seqOutput{{stdout: "ok"}}}
	c := newClientWithSeq(sr)
	out, err := c.ghReadRetry(context.Background(), "pr", "view")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out != "ok" {
		t.Errorf("stdout: got %q, want ok", out)
	}
	if sr.calls != 1 {
		t.Errorf("calls: got %d, want 1", sr.calls)
	}
}

func TestGhReadRetry_TransientThenSuccess(t *testing.T) {
	withZeroRetryDelays(t)
	sr := &seqRunner{outputs: []seqOutput{
		{stdout: `Get "...": unexpected EOF`, err: errors.New("exit status 1")},
		{stdout: `{"state":"OPEN"}`, err: nil},
	}}
	c := newClientWithSeq(sr)
	out, err := c.ghReadRetry(context.Background(), "pr", "view")
	if err != nil {
		t.Fatalf("unexpected err after retry: %v", err)
	}
	if out != `{"state":"OPEN"}` {
		t.Errorf("stdout: got %q, want JSON state", out)
	}
	if sr.calls != 2 {
		t.Errorf("calls: got %d, want 2 (1 transient + 1 success)", sr.calls)
	}
}

func TestGhReadRetry_AllTransientExhausts(t *testing.T) {
	withZeroRetryDelays(t)
	sr := &seqRunner{outputs: []seqOutput{
		{stdout: "unexpected EOF", err: errors.New("exit status 1")},
	}}
	c := newClientWithSeq(sr)
	_, err := c.ghReadRetry(context.Background(), "pr", "view")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// Initial + 3 retries = 4 invocations total (matches ghReadRetryDelays
	// having 3 entries).
	if sr.calls != 4 {
		t.Errorf("calls: got %d, want 4 (initial + 3 retries)", sr.calls)
	}
}

func TestGhReadRetry_NonTransientNoRetry(t *testing.T) {
	withZeroRetryDelays(t)
	sr := &seqRunner{outputs: []seqOutput{
		{stdout: "GraphQL: Could not resolve to a PullRequest", err: errors.New("exit status 1")},
	}}
	c := newClientWithSeq(sr)
	_, err := c.ghReadRetry(context.Background(), "pr", "view")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if sr.calls != 1 {
		t.Errorf("calls: got %d, want 1 (no retry for permanent failure)", sr.calls)
	}
}

func TestGhReadRetry_ContextCancelStopsBackoff(t *testing.T) {
	// Use a real (non-zero) backoff so cancel has something to interrupt.
	sr := &seqRunner{outputs: []seqOutput{
		{stdout: "unexpected EOF", err: errors.New("exit status 1")},
	}}
	c := newClientWithSeq(sr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before call
	_, err := c.ghReadRetry(ctx, "pr", "view")
	if err == nil {
		t.Fatal("expected ctx error")
	}
	// Should make at most 1 attempt before the cancel interrupts the
	// first backoff sleep.
	if sr.calls > 1 {
		t.Errorf("calls: got %d, want 1 (cancel must stop further attempts)", sr.calls)
	}
}

func TestPostComment_DoesNotRetryOnTransient(t *testing.T) {
	// POST is not idempotent — a comment that fails with "unexpected EOF"
	// after the request may have been created server-side. Retrying would
	// double-post. PostComment must call gh() directly, not ghReadRetry.
	sr := &seqRunner{outputs: []seqOutput{
		{stdout: "unexpected EOF", err: errors.New("exit status 1")},
	}}
	c := newClientWithSeq(sr)
	err := c.PostComment(context.Background(), "https://github.com/x/y/pull/1", "body")
	if err == nil {
		t.Fatal("expected error on transient PostComment failure")
	}
	if sr.calls != 1 {
		t.Errorf("calls: got %d, want 1 (PostComment must not retry POSTs)", sr.calls)
	}
}

func TestPostReview_ApproveCommand(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	if err := c.PostReview(context.Background(),
		"https://github.com/x/y/pull/1", "approve", "ok"); err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	want := []string{"pr", "review", "https://github.com/x/y/pull/1", "--approve", "--body", "ok"}
	if !equalSlice(fr.gotArgs, want) {
		t.Errorf("args:\n got %v\nwant %v", fr.gotArgs, want)
	}
}

func TestPostReview_RequestChangesRequiresBody(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	err := c.PostReview(context.Background(),
		"https://github.com/x/y/pull/1", "request_changes", "")
	if err == nil {
		t.Fatal("expected error when body empty for request_changes")
	}
	if fr.gotName != "" {
		t.Errorf("gh should not have been invoked, got args=%v", fr.gotArgs)
	}
}

func TestPostReview_ApproveAllowsEmptyBody(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	if err := c.PostReview(context.Background(),
		"https://github.com/x/y/pull/1", "approve", ""); err != nil {
		t.Fatalf("approve with empty body should work: %v", err)
	}
	want := []string{"pr", "review", "https://github.com/x/y/pull/1", "--approve"}
	if !equalSlice(fr.gotArgs, want) {
		t.Errorf("args:\n got %v\nwant %v", fr.gotArgs, want)
	}
}

func TestPostReview_UnknownVerdictRejected(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	err := c.PostReview(context.Background(),
		"https://github.com/x/y/pull/1", "wat", "body")
	if err == nil {
		t.Fatal("expected error for unknown verdict")
	}
	if fr.gotName != "" {
		t.Error("gh should not be invoked for unknown verdict")
	}
}

func TestPostReview_EmptyPRURLRejected(t *testing.T) {
	fr := &fakeRunner{}
	c := newTestClient("tok", fr)
	if err := c.PostReview(context.Background(), "  ", "approve", ""); err == nil {
		t.Fatal("expected error for empty prURL")
	}
}

func TestPostReview_PropagatesRunnerError(t *testing.T) {
	fr := &fakeRunner{err: errors.New("gh boom"), stdout: "permission denied"}
	c := newTestClient("tok", fr)
	err := c.PostReview(context.Background(),
		"https://github.com/x/y/pull/1", "approve", "ok")
	if err == nil {
		t.Fatal("expected error propagated from gh failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should include gh output, got: %v", err)
	}
}
