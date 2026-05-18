package github

import (
	"context"
	"errors"
	"strings"
	"testing"
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
	want := []string{"pr", "comment", "https://github.com/x/y/pull/1", "-b", "looks good"}
	if !equalSlice(fr.gotArgs, want) {
		t.Errorf("args mismatch:\n got %v\nwant %v", fr.gotArgs, want)
	}
	if len(fr.gotEnv) != 1 || fr.gotEnv[0] != "GH_TOKEN=tok-abc" {
		t.Errorf("env: got %v, want [GH_TOKEN=tok-abc]", fr.gotEnv)
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
