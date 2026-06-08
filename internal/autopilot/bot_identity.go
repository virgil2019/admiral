package autopilot

import (
	"context"
	"fmt"
	"strings"

	"github.com/georgehuang/admiral/internal/config"
)

// applyBotIdentityToWorktree writes worktree-local user.name / user.email so
// any commit produced inside the worktree (by claude or by admiral's own
// follow-up commits) is attributed to the configured bot. Idempotent — safe to
// call on both fresh and reused worktrees. No-op when the identity is not
// configured; admiral then falls through to the runtime user's git config
// (pre-#162 behavior).
func applyBotIdentityToWorktree(ctx context.Context, worktreePath string, id config.BotIdentity) error {
	if !id.IsSet() {
		return nil
	}
	name, email := strings.TrimSpace(id.Name), strings.TrimSpace(id.Email)
	if err := runCmd(ctx, worktreePath, "git", "config", "user.name", name); err != nil {
		return fmt.Errorf("set bot user.name in %s: %w", worktreePath, err)
	}
	if err := runCmd(ctx, worktreePath, "git", "config", "user.email", email); err != nil {
		return fmt.Errorf("set bot user.email in %s: %w", worktreePath, err)
	}
	return nil
}

// appendBotIdentityEnv adds GIT_AUTHOR_* / GIT_COMMITTER_* to env when the
// identity is configured. env vars beat worktree-local config in git's
// precedence, so this acts as a belt-and-braces guarantee: even if the
// worktree-config write was skipped on a particular code path, commits made
// by claude through admiral's env still carry the bot identity. Values are
// trimmed of surrounding whitespace before injection so a stray space in YAML
// config doesn't leak into the recorded author/committer.
func appendBotIdentityEnv(env []string, id config.BotIdentity) []string {
	if !id.IsSet() {
		return env
	}
	name, email := strings.TrimSpace(id.Name), strings.TrimSpace(id.Email)
	return append(env,
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
	)
}
