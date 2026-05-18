// admiral-jobs is an admin CLI for inspecting and cleaning up stale
// AWAITING_INPUT jobs — tasks where admiral asked the user a question via
// Linear and no reply was received.
//
// Usage:
//
//	admiral-jobs [--config <path>] list [--older-than <Nd>]
//	admiral-jobs [--config <path>] abort <ISSUE>
//	admiral-jobs [--config <path>] abort-stale --older-than <Nd> --confirm
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", config.DefaultConfigPath(), "path to config.yaml")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		args = []string{"list"}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatalf("config: %v", err)
	}

	db, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		fatalf("open db: %v", err)
	}
	defer db.Close()

	switch args[0] {
	case "list":
		runList(db, args[1:])
	case "abort":
		runAbort(db, cfg, args[1:])
	case "abort-stale":
		runAbortStale(db, cfg, args[1:])
	default:
		fatalf("unknown subcommand %q\n\nUsage:\n  admiral-jobs list [--older-than 30d]\n  admiral-jobs abort <ISSUE>\n  admiral-jobs abort-stale --older-than 60d --confirm", args[0])
	}
}

// --- list ---

func runList(db *store.Store, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	olderThan := fs.String("older-than", "", "filter to jobs older than N days, e.g. 30d")
	_ = fs.Parse(args)

	dur, err := parseDays(*olderThan)
	if err != nil {
		fatalf("--older-than: %v", err)
	}

	jobs, err := db.ListAwaitingInputJobs(dur)
	if err != nil {
		fatalf("list jobs: %v", err)
	}
	if len(jobs) == 0 {
		fmt.Println("no awaiting-input jobs found")
		return
	}

	now := time.Now().UTC()
	fmt.Printf("%-12s  %-8s  %-55s  %-50s\n",
		"ISSUE", "AGE", "WORKTREE", "QUESTION_PREVIEW")
	fmt.Println(strings.Repeat("-", 135))
	for _, j := range jobs {
		age := "unknown"
		if j.PendingCreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, j.PendingCreatedAt); err == nil {
				age = formatAge(now.Sub(t))
			}
		}
		issue := j.IssueIdentifier
		if issue == "" {
			issue = j.IssueID
		}
		q := j.PendingQuestion
		if len(q) > 48 {
			q = q[:48] + "…"
		}
		wt := j.WorktreePath
		if len(wt) > 53 {
			wt = "…" + wt[len(wt)-52:]
		}
		fmt.Printf("%-12s  %-8s  %-55s  %-50s\n", issue, age, wt, q)
	}
}

// --- abort ---

func runAbort(db *store.Store, cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("abort", flag.ExitOnError)
	_ = fs.Parse(args)

	if fs.NArg() == 0 {
		fatalf("usage: admiral-jobs abort <ISSUE>  (e.g. GEO-12)")
	}
	target := strings.ToUpper(strings.TrimSpace(fs.Arg(0)))
	abortOne(db, cfg, target, true)
}

// --- abort-stale ---

func runAbortStale(db *store.Store, cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("abort-stale", flag.ExitOnError)
	olderThan := fs.String("older-than", "60d", "abort jobs older than N days")
	confirm := fs.Bool("confirm", false, "required to actually perform the abort")
	_ = fs.Parse(args)

	dur, err := parseDays(*olderThan)
	if err != nil {
		fatalf("--older-than: %v", err)
	}
	if dur == 0 {
		fatalf("--older-than: must be a positive duration like 60d")
	}

	jobs, err := db.ListAwaitingInputJobs(dur)
	if err != nil {
		fatalf("list jobs: %v", err)
	}
	if len(jobs) == 0 {
		fmt.Printf("no awaiting-input jobs older than %s\n", *olderThan)
		return
	}

	fmt.Printf("found %d job(s) to abort:\n", len(jobs))
	for _, j := range jobs {
		id := j.IssueIdentifier
		if id == "" {
			id = j.IssueID
		}
		fmt.Printf("  %s  %s\n", id, j.WorktreePath)
	}

	if !*confirm {
		fmt.Println("\npass --confirm to execute")
		return
	}

	for _, j := range jobs {
		id := j.IssueIdentifier
		if id == "" {
			id = j.IssueID
		}
		abortOne(db, cfg, id, false)
	}
}

// abortOne aborts a single AWAITING_INPUT job identified by issueIdentifier
// (or issueID). verbose controls whether progress is printed.
func abortOne(db *store.Store, cfg *config.Config, identifier string, verbose bool) {
	jobs, err := db.ListAwaitingInputJobs(0)
	if err != nil {
		fatalf("list jobs: %v", err)
	}

	var target *store.AwaitingInputJob
	for i := range jobs {
		j := &jobs[i]
		if strings.EqualFold(j.IssueIdentifier, identifier) ||
			strings.EqualFold(j.IssueID, identifier) {
			target = j
			break
		}
	}
	if target == nil {
		fatalf("no awaiting-input job found for %q", identifier)
	}

	issueLabel := target.IssueIdentifier
	if issueLabel == "" {
		issueLabel = target.IssueID
	}

	if verbose {
		fmt.Printf("aborting %s ...\n", issueLabel)
	}

	// 1. Archive worktree (best-effort; don't fail the abort if it errors).
	if target.WorktreePath != "" {
		if err := archiveWorktree(target.WorktreePath, issueLabel); err != nil {
			fmt.Fprintf(os.Stderr, "warn: archive worktree: %v (continuing)\n", err)
		} else if verbose {
			fmt.Printf("  worktree archived\n")
		}
	}

	// 2. Update DB.
	ok, err := db.AbortAdmiralTask(target.IssueID)
	if err != nil {
		fatalf("abort task in db: %v", err)
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "warn: %s was no longer in AWAITING_INPUT (skipped)\n", issueLabel)
		return
	}
	if verbose {
		fmt.Printf("  db state → ABORTED\n")
	}

	// 3. Post Linear comment.
	if target.LastEventSession != "" {
		postAbortComment(db, cfg, target.LastEventSession, issueLabel)
		if verbose {
			fmt.Printf("  Linear comment posted\n")
		}
	}

	if verbose {
		fmt.Printf("done: %s aborted\n", issueLabel)
	} else {
		fmt.Printf("aborted %s\n", issueLabel)
	}
}

// --- archive worktree ---

// archiveWorktree copies the worktree to .worktrees-archive/ adjacent to
// the main repo, then removes the git worktree registration.
func archiveWorktree(worktreePath, issueLabel string) error {
	repoDir, err := findRepoDir(worktreePath)
	if err != nil {
		return fmt.Errorf("find repo dir: %w", err)
	}

	archiveRoot := filepath.Join(repoDir, ".worktrees-archive")
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir archive root: %w", err)
	}

	dst := filepath.Join(archiveRoot,
		fmt.Sprintf("%s-%d", sanitizeLabel(issueLabel), time.Now().Unix()))
	if err := copyDir(worktreePath, dst); err != nil {
		return fmt.Errorf("copy worktree: %w", err)
	}

	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: git worktree remove: %v\n%s\n", err, out)
		// Fallback: hard remove + prune.
		if removeErr := os.RemoveAll(worktreePath); removeErr != nil {
			fmt.Fprintf(os.Stderr, "warn: os.RemoveAll worktree: %v\n", removeErr)
		} else {
			pruneCmd := exec.Command("git", "worktree", "prune")
			pruneCmd.Dir = repoDir
			if pruneOut, pruneErr := pruneCmd.CombinedOutput(); pruneErr != nil {
				fmt.Fprintf(os.Stderr, "warn: git worktree prune: %v\n%s\n", pruneErr, pruneOut)
			}
		}
	}
	return nil
}

// findRepoDir returns the main repository root for a linked git worktree.
func findRepoDir(worktreePath string) (string, error) {
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	commonDir := strings.TrimSpace(string(out))
	// commonDir is the main repo's .git dir, either absolute or relative to worktreePath.
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	return filepath.Dir(commonDir), nil
}

var reSanitize = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = reSanitize.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// --- Linear comment ---

func postAbortComment(db *store.Store, cfg *config.Config, sessionID, issueLabel string) {
	tok, err := db.GetLinearOAuthToken()
	if err != nil || tok == nil || tok.AccessToken == "" {
		fmt.Fprintf(os.Stderr, "warn: no Linear token in db, skipping comment\n")
		return
	}
	lc := linear.NewClient(cfg.Linear.APIBase, tok.AccessToken)
	body := fmt.Sprintf(
		"admiral: no reply received for an extended period — aborting session for %s. "+
			"Use /rerun to start a new run.", issueLabel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lc.PostAgentActivity(ctx, sessionID, linear.Action("aborted", body, "")); err != nil {
		fmt.Fprintf(os.Stderr, "warn: post Linear comment: %v\n", err)
	}
}

// --- helpers ---

// parseDays parses a duration string like "30d" or "" (zero). Returns an error
// for any other format.
func parseDays(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, "d") {
		return 0, fmt.Errorf("expected format Nd (e.g. 30d), got %q", s)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("expected positive integer before 'd', got %q", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

func formatAge(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("%dd", days)
	}
	hours := int(d.Hours())
	if hours >= 1 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "admiral-jobs: "+format+"\n", args...)
	os.Exit(1)
}
