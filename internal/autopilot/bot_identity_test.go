package autopilot

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/config"
)

func TestAppendBotIdentityEnv_Unset(t *testing.T) {
	base := []string{"FOO=bar", "BAZ=qux"}
	got := appendBotIdentityEnv(base, config.BotIdentity{})
	if len(got) != len(base) {
		t.Errorf("expected no-op on unset identity, got %d entries (base had %d)", len(got), len(base))
	}
	got = appendBotIdentityEnv(base, config.BotIdentity{Name: "only-name"})
	if len(got) != len(base) {
		t.Errorf("partial identity (name only) should be no-op, got %d entries", len(got))
	}
	got = appendBotIdentityEnv(base, config.BotIdentity{Email: "only@email"})
	if len(got) != len(base) {
		t.Errorf("partial identity (email only) should be no-op, got %d entries", len(got))
	}
}

func TestAppendBotIdentityEnv_Set(t *testing.T) {
	base := []string{"FOO=bar"}
	id := config.BotIdentity{Name: "admiral-bot", Email: "bot@example.com"}
	got := appendBotIdentityEnv(base, id)
	want := map[string]string{
		"GIT_AUTHOR_NAME":     "admiral-bot",
		"GIT_AUTHOR_EMAIL":    "bot@example.com",
		"GIT_COMMITTER_NAME":  "admiral-bot",
		"GIT_COMMITTER_EMAIL": "bot@example.com",
	}
	for k, v := range want {
		needle := k + "=" + v
		found := false
		for _, e := range got {
			if e == needle {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("env missing %q; got %v", needle, got)
		}
	}
	// Base env must be preserved verbatim — bot env is appended, not replaced.
	if got[0] != "FOO=bar" {
		t.Errorf("base env was clobbered; got[0] = %q, want %q", got[0], "FOO=bar")
	}
}

func TestApplyBotIdentityToWorktree_Unset(t *testing.T) {
	dir := initEmptyGitRepo(t)
	// Should not error and should not leave behind any user.name / user.email.
	if err := applyBotIdentityToWorktree(context.Background(), dir, config.BotIdentity{}); err != nil {
		t.Fatalf("unset identity should be no-op, got err: %v", err)
	}
	if name := gitConfig(t, dir, "user.name"); name != "" {
		t.Errorf("expected no user.name; got %q", name)
	}
	if email := gitConfig(t, dir, "user.email"); email != "" {
		t.Errorf("expected no user.email; got %q", email)
	}
}

func TestApplyBotIdentityToWorktree_Set(t *testing.T) {
	dir := initEmptyGitRepo(t)
	id := config.BotIdentity{Name: "admiral-bot", Email: "bot@example.com"}
	if err := applyBotIdentityToWorktree(context.Background(), dir, id); err != nil {
		t.Fatalf("applyBotIdentityToWorktree: %v", err)
	}
	if got := gitConfig(t, dir, "user.name"); got != "admiral-bot" {
		t.Errorf("user.name = %q, want admiral-bot", got)
	}
	if got := gitConfig(t, dir, "user.email"); got != "bot@example.com" {
		t.Errorf("user.email = %q, want bot@example.com", got)
	}
}

func TestApplyBotIdentityToWorktree_TrimsWhitespace(t *testing.T) {
	dir := initEmptyGitRepo(t)
	id := config.BotIdentity{Name: "  admiral-bot  ", Email: "\tbot@example.com\n"}
	if err := applyBotIdentityToWorktree(context.Background(), dir, id); err != nil {
		t.Fatalf("applyBotIdentityToWorktree: %v", err)
	}
	if got := gitConfig(t, dir, "user.name"); got != "admiral-bot" {
		t.Errorf("user.name = %q, want admiral-bot (no surrounding whitespace)", got)
	}
	if got := gitConfig(t, dir, "user.email"); got != "bot@example.com" {
		t.Errorf("user.email = %q, want bot@example.com (no surrounding whitespace)", got)
	}
}

func TestAppendBotIdentityEnv_TrimsWhitespace(t *testing.T) {
	id := config.BotIdentity{Name: "  admiral-bot  ", Email: "\tbot@example.com\n"}
	got := appendBotIdentityEnv(nil, id)
	for _, e := range got {
		if strings.ContainsAny(e[strings.IndexByte(e, '=')+1:], " \t\n") {
			t.Errorf("env entry %q contains untrimmed whitespace in value", e)
		}
	}
	expected := []string{
		"GIT_AUTHOR_NAME=admiral-bot",
		"GIT_AUTHOR_EMAIL=bot@example.com",
		"GIT_COMMITTER_NAME=admiral-bot",
		"GIT_COMMITTER_EMAIL=bot@example.com",
	}
	for _, want := range expected {
		found := false
		for _, e := range got {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("env missing %q (whitespace not trimmed correctly); got %v", want, got)
		}
	}
}

func TestApplyBotIdentityToWorktree_Idempotent(t *testing.T) {
	dir := initEmptyGitRepo(t)
	id := config.BotIdentity{Name: "first", Email: "first@example.com"}
	if err := applyBotIdentityToWorktree(context.Background(), dir, id); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	id2 := config.BotIdentity{Name: "second", Email: "second@example.com"}
	if err := applyBotIdentityToWorktree(context.Background(), dir, id2); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	// Last write wins (single value per key — `git config` rewrites by default).
	if got := gitConfig(t, dir, "user.name"); got != "second" {
		t.Errorf("user.name = %q, want second", got)
	}
}

// --- helpers ---

func initEmptyGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	return dir
}

func gitConfig(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "--get", key)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Exit code 1 from `git config --get` on a missing key is expected,
		// not a test failure.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return ""
		}
		t.Fatalf("git config --get %s: %v (%s)", key, err, out)
	}
	return strings.TrimSpace(string(out))
}
