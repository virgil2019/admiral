package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ghProbe abstracts the read-only gh CLI calls used by the orchestrator's
// pre-flight short-circuits (already-merged detection, prior-PR state
// lookups). The default production implementation shells out to gh; tests
// inject fakes via fakeGhProbe in the _test files.
//
// "Found" semantics: callers distinguish "no such PR / branch exists but no
// PR" (found=false, err=nil) from "couldn't reach gh / network error"
// (err non-nil). Production shellouts coerce common gh exit codes to the
// found=false form so the orchestrator can fail open and continue.
type ghProbe interface {
	// FindMergedPRForBranch looks up the most recent merged PR whose head
	// branch matches the given name in the repo at repoDir. Returns the PR
	// url and the merge commit SHA when found.
	FindMergedPRForBranch(ctx context.Context, repoDir, branch string) (url, mergeSHA string, found bool, err error)

	// PRState returns the lifecycle state of a PR by URL: "OPEN", "MERGED",
	// "CLOSED", or "" when the PR can't be located. Errors are returned
	// only for unexpected failures.
	PRState(ctx context.Context, prURL string) (state string, err error)

	// FindOpenPRForBranch looks up the most recent open PR whose head branch
	// matches the given name. Returns the PR url, author login, and head
	// ref name when found.
	FindOpenPRForBranch(ctx context.Context, repoDir, branch string) (url, author string, found bool, err error)
}

// ghCLIProbe is the default ghProbe — a thin shell around `gh pr list` and
// `gh pr view` that JSON-decodes the responses.
type ghCLIProbe struct {
	bin string
}

func newGhCLIProbe(bin string) *ghCLIProbe {
	if bin == "" {
		bin = "gh"
	}
	return &ghCLIProbe{bin: bin}
}

// FindMergedPRForBranch shells out to:
//
//	gh pr list --head <branch> --state merged --json url,mergeCommit --limit 1
//
// gh prints `[]` when no PRs match, which we treat as found=false. Anything
// else (auth error, network error, gh not installed) bubbles up as err.
func (g *ghCLIProbe) FindMergedPRForBranch(ctx context.Context, repoDir, branch string) (string, string, bool, error) {
	if branch == "" {
		return "", "", false, nil
	}
	out, err := captureCmd(ctx, repoDir, g.bin,
		"pr", "list",
		"--head", branch,
		"--state", "merged",
		"--json", "url,mergeCommit",
		"--limit", "1",
	)
	if err != nil {
		return "", "", false, fmt.Errorf("gh pr list --head %s: %w (output: %s)", branch, err, truncate(out, 200))
	}
	var rows []struct {
		URL         string `json:"url"`
		MergeCommit struct {
			Oid string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return "", "", false, fmt.Errorf("parse gh pr list output: %w (raw: %s)", err, truncate(out, 200))
	}
	if len(rows) == 0 {
		return "", "", false, nil
	}
	return rows[0].URL, rows[0].MergeCommit.Oid, true, nil
}

// PRState shells out to:
//
//	gh pr view <url> --json state
//
// Returns "OPEN", "MERGED", "CLOSED", or "" if the PR is not locatable.
// Treats `gh: graphql: ... could not resolve to a PullRequest` as ""
// (unlocatable) rather than a hard error so the caller can fall back to
// fresh-flow.
func (g *ghCLIProbe) PRState(ctx context.Context, prURL string) (string, error) {
	if strings.TrimSpace(prURL) == "" {
		return "", nil
	}
	out, err := captureCmd(ctx, "", g.bin, "pr", "view", prURL, "--json", "state")
	if err != nil {
		// gh pr view returns non-zero when the PR can't be resolved (e.g.
		// stale URL, deleted repo). The orchestrator treats this as "we
		// don't know" and falls through to fresh flow rather than
		// surfacing it as an error — surface via empty state.
		if strings.Contains(out, "could not resolve") || strings.Contains(out, "no pull request") {
			return "", nil
		}
		return "", fmt.Errorf("gh pr view %s: %w (output: %s)", prURL, err, truncate(out, 200))
	}
	var resp struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return "", fmt.Errorf("parse gh pr view output: %w (raw: %s)", err, truncate(out, 200))
	}
	return resp.State, nil
}

// FindOpenPRForBranch shells out to:
//
//	gh pr list --head <branch> --state open --json url,author,headRefName --limit 1
//
// gh prints `[]` when no PRs match, which we treat as found=false. Anything
// else (auth error, network error, gh not installed) bubbles up as err.
func (g *ghCLIProbe) FindOpenPRForBranch(ctx context.Context, repoDir, branch string) (string, string, bool, error) {
	if branch == "" {
		return "", "", false, nil
	}
	out, err := captureCmd(ctx, repoDir, g.bin,
		"pr", "list",
		"--head", branch,
		"--state", "open",
		"--json", "url,author,headRefName",
		"--limit", "1",
	)
	if err != nil {
		return "", "", false, fmt.Errorf("gh pr list --head %s: %w (output: %s)", branch, err, truncate(out, 200))
	}
	var rows []struct {
		URL    string `json:"url"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return "", "", false, fmt.Errorf("parse gh pr list output: %w (raw: %s)", err, truncate(out, 200))
	}
	if len(rows) == 0 {
		return "", "", false, nil
	}
	return rows[0].URL, rows[0].Author.Login, true, nil
}
