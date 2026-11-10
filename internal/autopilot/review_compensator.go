package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

// reviewCompensationInterval bounds how long review feedback lost to a
// webhook delivery failure can sit unprocessed.
var reviewCompensationInterval = 5 * time.Minute

// ghReview / ghComment are the fields compensateReviews needs from `gh api`
// responses.
type ghReview struct {
	ID    int    `json:"id"`
	State string `json:"state"`
	Body  string `json:"body"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

type ghComment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// StartReviewCompensation re-enqueues review feedback that GitHub delivered
// while admiral could not receive it: events past the delivery retry window
// never reached events_inbox, so nothing else recovers them — the reviewer's
// feedback would sit on the PR with admiral silent forever. Every attempt
// carries a deterministic webhook ID (gh-recon-<kind>-<id>); events_inbox's
// unique index absorbs re-enqueues of already-delivered feedback, so the
// scan keeps no delivery bookkeeping of its own.
func (o *Orchestrator) StartReviewCompensation(ctx context.Context) {
	t := time.NewTicker(reviewCompensationInterval)
	defer t.Stop()
	o.compensateReviews()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.compensateReviews()
		}
	}
}

// compensateReviews scans each open-PR task's submitted reviews and inline
// review comments — the two feedback shapes the review fix loop consumes —
// and re-enqueues the ones missing from events_inbox. Ordinary PR comments
// (issue_comment) are deliberately not scanned: that is the /fix command
// channel, where a lost event is visible to the sender, who will simply
// resend.
func (o *Orchestrator) compensateReviews() {
	rows, err := o.db.ListWatchableCITasks()
	if err != nil {
		o.logger.Error("review_compensation_list_failed", "err", err)
		return
	}
	for _, row := range rows {
		repo, ok := o.resolveRepoForRow(row)
		if !ok {
			continue
		}
		env, err := o.ghEnvForAccount(context.Background(), repo.GhUser)
		if err != nil {
			o.logger.Warn("review_compensation_gh_env_failed",
				"issue", row.IssueIdentifier, "err", err)
			continue
		}
		reviews, comments, err := o.fetchRecentFeedback(context.Background(), repo.RepoDir, row.PRURL, env)
		if err != nil {
			o.logger.Warn("review_compensation_fetch_failed",
				"pr", row.PRURL, "err", err)
			continue
		}
		o.processFeedback(row, repo.GhUser, reviews, comments)
	}
}

// processFeedback filters one row's fetched feedback (pending reviews and
// admiral's own — the loop guard, same as the webhook's bot filter) and
// re-enqueues the rest.
func (o *Orchestrator) processFeedback(row store.AdmiralTask, ghUser string, reviews []ghReview, comments []ghComment) {
	for _, r := range reviews {
		if r.State == "PENDING" || strings.EqualFold(r.User.Login, ghUser) {
			continue
		}
		o.enqueueLostFeedback(row, "submitted", "review",
			fmt.Sprintf("gh-recon-review-%d", r.ID), "",
			map[string]any{"review": map[string]any{"body": r.Body, "state": strings.ToLower(r.State)}})
	}
	for _, c := range comments {
		if strings.EqualFold(c.User.Login, ghUser) {
			continue
		}
		o.enqueueLostFeedback(row, "pull_request_review_comment.created", "comment",
			fmt.Sprintf("gh-recon-comment-%d", c.ID), fmt.Sprint(c.ID),
			map[string]any{"comment": map[string]any{"body": c.Body}})
	}
}

// enqueueLostFeedback attempts the re-enqueue and logs when it was fresh —
// a duplicate (feedback the webhook did deliver) is absorbed by
// events_inbox's unique index and reported only at Debug level.
func (o *Orchestrator) enqueueLostFeedback(row store.AdmiralTask, action, kind, webhookID, commentID string, payload any) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		o.logger.Warn("review_compensation_marshal_failed", "err", err)
		return
	}
	fresh, err := o.db.EnqueueEventWithSource("github", webhookID, action, row.PRURL, "", string(payloadJSON), commentID)
	if err != nil {
		o.logger.Warn("review_compensation_enqueue_failed", "webhook_id", webhookID, "err", err)
		return
	}
	if fresh {
		o.logger.Info("review_compensation_enqueued",
			"issue", row.IssueIdentifier, "pr", row.PRURL, "kind", kind, "webhook_id", webhookID)
	} else {
		o.logger.Debug("review_compensation_duplicate", "webhook_id", webhookID)
	}
}

// ghEnvForAccount resolves the gh account env for a repo's registered user.
// Wrapper over accounts.EnvFor with a nil-accounts guard for test fixtures.
func (o *Orchestrator) ghEnvForAccount(ctx context.Context, ghUser string) ([]string, error) {
	if o.accounts == nil {
		return nil, fmt.Errorf("gh accounts resolver not wired")
	}
	return o.accounts.EnvFor(ctx, strings.TrimSpace(ghUser))
}

// fetchRecentFeedback pulls the newest submitted reviews and inline review
// comments for prURL. Lost feedback is by definition recent, so only the
// newest per_page entries are consulted.
func (o *Orchestrator) fetchRecentFeedback(ctx context.Context, repoDir, prURL string, env []string) ([]ghReview, []ghComment, error) {
	owner, repo, err := parseOwnerRepo(prURL)
	if err != nil {
		return nil, nil, err
	}
	num, err := extractPRNumberFromURL(prURL)
	if err != nil {
		return nil, nil, err
	}
	reviewsOut, err := captureCmd(ctx, repoDir, "gh", "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews?per_page=10", owner, repo, num))
	if err != nil {
		return nil, nil, fmt.Errorf("fetch reviews: %w (output: %s)", err, truncate(reviewsOut, 400))
	}
	var reviews []ghReview
	if err := json.Unmarshal([]byte(reviewsOut), &reviews); err != nil {
		return nil, nil, fmt.Errorf("parse reviews: %w", err)
	}
	commentsOut, err := captureCmd(ctx, repoDir, "gh", "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/comments?per_page=10", owner, repo, num))
	if err != nil {
		return nil, nil, fmt.Errorf("fetch comments: %w (output: %s)", err, truncate(commentsOut, 400))
	}
	var comments []ghComment
	if err := json.Unmarshal([]byte(commentsOut), &comments); err != nil {
		return nil, nil, fmt.Errorf("parse comments: %w", err)
	}
	return reviews, comments, nil
}

// parseOwnerRepo extracts owner and repo from a GitHub PR URL
// (https://github.com/<owner>/<repo>/pull/<n>).
func parseOwnerRepo(prURL string) (string, string, error) {
	parts := strings.Split(strings.TrimPrefix(prURL, "https://github.com/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("unrecognized PR URL: %s", prURL)
	}
	return parts[0], parts[1], nil
}
