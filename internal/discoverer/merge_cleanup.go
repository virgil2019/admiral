package discoverer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

// cleanupMergedTask is the merge-time residue sweep: the task's worktree is
// archived (recoverable copy under <repoDir>/.worktrees-archive — same
// layout and copy-before-remove order as autopilot's archiveWorktree, GEO-37
// Bug B) and its local + remote branches deleted. A merged PR's branch has
// no future, and without this sweep merged-task worktrees accumulate
// forever. Best-effort: every failure is a warning, because the Linear/task
// state transitions have already succeeded and must not be rolled back by a
// cleanup hiccup.
func (s *Service) cleanupMergedTask(ctx context.Context, t *store.AdmiralTask, repoDir string) {
	archiveID := t.IssueID
	if archiveID == "" {
		archiveID = t.IssueIdentifier
	}
	archivePath := filepath.Join(repoDir, ".worktrees-archive",
		fmt.Sprintf("%s-%d", strings.ReplaceAll(archiveID, "/", "-"), time.Now().Unix()))

	if t.WorktreePath != "" {
		// Copy first, so the worktree content survives even if git's
		// cooperative removal fails halfway.
		if err := copyDirForArchive(t.WorktreePath, archivePath); err != nil {
			s.logger.Warn("merge_cleanup_archive_copy_failed",
				"issue", t.IssueIdentifier, "err", err)
		}
		if out, err := runGitCmd(ctx, repoDir, "worktree", "remove", "--force", t.WorktreePath); err != nil {
			s.logger.Warn("merge_cleanup_worktree_remove_failed",
				"issue", t.IssueIdentifier, "err", err, "out", out,
				"action", "falling back to os.RemoveAll + git worktree prune")
			if rmErr := os.RemoveAll(t.WorktreePath); rmErr != nil {
				s.logger.Error("merge_cleanup_force_remove_failed",
					"issue", t.IssueIdentifier, "err", rmErr,
					"action", "manual cleanup required — directory will leak")
			} else if _, perr := runGitCmd(ctx, repoDir, "worktree", "prune"); perr != nil {
				s.logger.Warn("merge_cleanup_prune_failed", "issue", t.IssueIdentifier, "err", perr)
			}
		}
		if archived, statErr := os.Stat(archivePath); statErr == nil && archived.IsDir() {
			s.logger.Info("merge_cleanup_worktree_archived",
				"issue", t.IssueIdentifier, "path", archivePath)
		}
	}

	// Branches: local -D, remote --delete. The remote side may already be
	// gone (repos with auto-delete head branches) — that failure is benign.
	if t.Branch != "" {
		if out, err := runGitCmd(ctx, repoDir, "branch", "-D", t.Branch); err != nil {
			s.logger.Warn("merge_cleanup_branch_delete_failed",
				"issue", t.IssueIdentifier, "branch", t.Branch, "err", err, "out", out)
		}
		if out, err := runGitCmd(ctx, repoDir, "push", "origin", "--delete", t.Branch); err != nil {
			s.logger.Info("merge_cleanup_remote_branch_delete_skipped",
				"issue", t.IssueIdentifier, "branch", t.Branch, "out", out)
		}
	}
	s.logger.Info("merge_cleanup_done", "issue", t.IssueIdentifier)
}

// runGitCmd runs one git command in dir and returns its combined output.
func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// copyDirForArchive copies src to dst recursively in pure Go.
func copyDirForArchive(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
