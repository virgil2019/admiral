package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// toolHandler is the function signature every MCP tool implements.
// args is the raw `arguments` field from the tools/call params —
// each handler is responsible for unmarshalling into its own typed
// struct. Returning (any, nil) on success encodes to a JSON content
// block; returning (_, err) becomes an MCP isError result the host
// agent surfaces to the LLM.
type toolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// toolDef bundles the MCP descriptor (visible to the host agent via
// tools/list) with its Go handler. Held in the Server's registry
// keyed on Name.
type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     toolHandler
}

// issueRef returns the most readable identifier for an admiral_task:
// the human "GEO-50" identifier when populated, falling back to the
// internal UUID. Used in error messages so callers can grep / search
// in Linear without guessing.
func issueRef(task *store.AdmiralTask) string {
	if task == nil {
		return ""
	}
	if task.IssueIdentifier != "" {
		return task.IssueIdentifier
	}
	return task.IssueID
}

// PRDiffer is the read half of the GitHub dependency — fetches the
// unified diff for a PR. Kept separate so tests of read-only tools
// don't need to satisfy the write half.
type PRDiffer interface {
	GetDiff(ctx context.Context, prURL string) (string, error)
}

// PRReviewer is the write half — actually posts a verdict to GitHub
// via `gh pr review`. The concrete implementation in production is
// *github.Client; tests inject a stub.
type PRReviewer interface {
	PostReview(ctx context.Context, prURL, verdict, body string) error
}

// GitHubClient bundles both halves. cmd/admiral-planner-mcp passes
// a *github.Client which satisfies both, but tools.go takes the
// composite so individual tools can pin to just the slice they need
// (pr_get_materials wants only PRDiffer, pr_verify_submit wants
// PRReviewer + idempotency lookup against the planner store).
type GitHubClient interface {
	PRDiffer
	PRReviewer
}

// LinearClient is the Linear write path the planner needs for L2
// follow-up issue creation. *linear.Client satisfies it directly (same
// arrangement as *github.Client and GitHubClient). May be nil when the
// server booted without a Linear OAuth token in the DB — feature_followup_submit
// returns a tool-level error in that case rather than crashing.
type LinearClient interface {
	GetProjectTeamID(ctx context.Context, projectID string) (string, error)
	IssueCreate(ctx context.Context, in linear.IssueCreateInput) (*linear.Issue, error)
	GetTeamLabelID(ctx context.Context, teamID, name string) (string, error)
	GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error)
}

// PickupRules mirror the discoverer's require_label + state_types so the
// issues the planner creates satisfy the discoverer's pickup gates and get
// shipped automatically. Loaded from admiral's config by the main package
// (kept as a planner-local struct so this package doesn't depend on config).
// A zero value (nil StateTypes) means "not configured" — the planner then
// creates issues without a label or forced state, as it did before pickup
// support, and the operator must label / move them manually.
type PickupRules struct {
	RequireLabel string
	StateTypes   []string
}

// BuildTools wires every tool the planner exposes, closing over the
// Store + PRDiffer dependencies. cmd/admiral-planner-mcp calls this
// once at startup and passes the result to NewServer. Adding a tool
// means adding one entry here plus its handler below.
//
// gh / lc may be nil — tools that need them (pr_get_materials /
// pr_verify_submit need gh; feature_followup_submit needs lc) return an
// MCP error in that case rather than crashing the process. This lets the
// server boot in environments where ADMIRAL_GH_TOKEN or the Linear OAuth
// token is not configured (e.g. read-only inspection of planner state).
func BuildTools(db *store.Store, gh GitHubClient, lc LinearClient, pickup PickupRules) map[string]*toolDef {
	return map[string]*toolDef{
		"feature_start":           featureStartTool(db),
		"issue_set_acceptance":    issueSetAcceptanceTool(db),
		"feature_get_materials":   featureGetMaterialsTool(db),
		"issue_list_by_feature":   issueListByFeatureTool(db),
		"pr_get_materials":        prGetMaterialsTool(db, gh),
		"pr_verify_submit":        prVerifySubmitTool(db, gh),
		"feature_followup_submit": featureFollowupSubmitTool(db, lc, pickup),
		"feature_close":           featureCloseTool(db),
	}
}

// --- feature_get_materials ---

// featureGetMaterialsArgs is the typed view of the JSON arguments
// passed by the host agent. Mirrors the inputSchema below.
type featureGetMaterialsArgs struct {
	FeatureID string `json:"feature_id"`
}

// featureGetMaterialsResult is what the host agent reads back to do
// L2 acceptance: the feature's original requirements (ground truth
// from the user) plus every issue's acceptance criteria. PR diff /
// PR-state fields are deliberately absent from this PR — they come
// in once the GitHub / Linear integration layer lands. The host
// agent should treat a missing or empty `issues` list as a signal
// to re-decompose or to prompt the user, not as "everything passed".
type featureGetMaterialsResult struct {
	Feature *featurePayload      `json:"feature"`
	Issues  []featureIssuePayload `json:"issues"`
}

// featurePayload mirrors store.Feature but uses snake_case + omits
// internal fields the host agent has no business reading. Keeping
// the wire shape separate from the storage struct insulates the
// MCP contract from future store-side renames.
type featurePayload struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	LinearProjectID  string `json:"linear_project_id"`
	RequirementsText string `json:"requirements_text"`
	SourceAgent      string `json:"source_agent,omitempty"`
	CreatedAt        string `json:"created_at"`
	ClosedAt         string `json:"closed_at,omitempty"`
}

type featureIssuePayload struct {
	LinearIssueID       string `json:"linear_issue_id"`
	AcceptanceCriteria  string `json:"acceptance_criteria"`
	CreatedAt           string `json:"created_at"`
}

// --- issue_list_by_feature ---

type issueListByFeatureArgs struct {
	FeatureID string `json:"feature_id"`
}

// issueRowPayload bundles the planner's per-issue spec with the
// admiral_tasks state for that same Linear issue, so the host agent
// sees "what we asked for" and "what admiral has produced" in one
// shot — that's the join it would otherwise have to do via two
// separate tool calls.
type issueRowPayload struct {
	LinearIssueID      string `json:"linear_issue_id"`
	IssueIdentifier    string `json:"issue_identifier,omitempty"` // "GEO-50"
	AcceptanceCriteria string `json:"acceptance_criteria"`
	State              string `json:"state,omitempty"`            // admiral_tasks.state, or "" when admiral hasn't started yet
	PRURL              string `json:"pr_url,omitempty"`
}

type issueListByFeatureResult struct {
	FeatureID string            `json:"feature_id"`
	Issues    []issueRowPayload `json:"issues"`
}

func issueListByFeatureTool(db *store.Store) *toolDef {
	return &toolDef{
		Name: "issue_list_by_feature",
		Description: "List every issue belonging to a feature with its acceptance " +
			"criteria and current admiral_tasks state (RECEIVED / EXECUTING / DONE / " +
			"DONE_MERGED / ...) plus PR URL when one exists. Use to find which PRs " +
			"are ready for L1 verification.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"feature_id": map[string]any{
					"type":        "string",
					"description": "The feature ID returned by feature_start.",
				},
			},
			"required": []string{"feature_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args issueListByFeatureArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.FeatureID == "" {
				return nil, fmt.Errorf("feature_id is required")
			}
			// Verify the feature exists so the caller sees "unknown feature"
			// rather than an empty list (which could mean "no issues yet").
			f, err := db.GetFeature(args.FeatureID)
			if err != nil {
				return nil, fmt.Errorf("get feature: %w", err)
			}
			if f == nil {
				return nil, fmt.Errorf("feature %q not found", args.FeatureID)
			}
			rawIssues, err := db.ListFeatureIssues(args.FeatureID)
			if err != nil {
				return nil, fmt.Errorf("list feature issues: %w", err)
			}
			out := make([]issueRowPayload, 0, len(rawIssues))
			for _, fi := range rawIssues {
				row := issueRowPayload{
					LinearIssueID:      fi.LinearIssueID,
					AcceptanceCriteria: fi.AcceptanceCriteria,
				}
				// admiral_tasks may not yet exist (issue not started). N+1
				// query is acceptable: a feature rarely has more than a
				// handful of issues and the DB is single-conn anyway.
				task, terr := db.GetAdmiralTaskByIssue(fi.LinearIssueID)
				if terr != nil {
					return nil, fmt.Errorf("get task for issue %s: %w", fi.LinearIssueID, terr)
				}
				if task != nil {
					row.IssueIdentifier = task.IssueIdentifier
					row.State = task.State
					row.PRURL = task.PRURL
				}
				out = append(out, row)
			}
			return issueListByFeatureResult{
				FeatureID: args.FeatureID,
				Issues:    out,
			}, nil
		},
	}
}

// --- pr_get_materials ---

type prGetMaterialsArgs struct {
	PRURL string `json:"pr_url"`
}

// prGetMaterialsResult is what the host agent reads to do L1
// acceptance on a single PR: the criteria the issue was decomposed
// against (planner-side ground truth) and the unified diff (the work
// to judge). Base branch is included as context for the agent's
// reasoning ("did the diff stay within the intended scope of this
// branch?"); empty when admiral_tasks didn't record one.
type prGetMaterialsResult struct {
	FeatureID          string `json:"feature_id"`
	FeatureName        string `json:"feature_name"`
	LinearIssueID      string `json:"linear_issue_id"`
	IssueIdentifier    string `json:"issue_identifier,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Branch             string `json:"branch,omitempty"`
	Diff               string `json:"diff"`
}

func prGetMaterialsTool(db *store.Store, gh PRDiffer) *toolDef {
	return &toolDef{
		Name: "pr_get_materials",
		Description: "Read everything needed to judge a single PR against its L1 " +
			"acceptance criteria: the criteria recorded for the underlying Linear " +
			"issue, plus the unified diff fetched live from GitHub. Use before " +
			"calling pr_verify_submit. Errors if the PR is not tracked by admiral " +
			"or not linked to any planner feature.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pr_url": map[string]any{
					"type":        "string",
					"description": "GitHub PR URL (https://github.com/owner/repo/pull/N).",
				},
			},
			"required": []string{"pr_url"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args prGetMaterialsArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.PRURL == "" {
				return nil, fmt.Errorf("pr_url is required")
			}
			if gh == nil {
				return nil, fmt.Errorf("GitHub client not configured (set ADMIRAL_GH_TOKEN)")
			}
			task, err := db.GetAdmiralTaskByPRURL(args.PRURL)
			if err != nil {
				return nil, fmt.Errorf("lookup admiral_task: %w", err)
			}
			if task == nil {
				return nil, fmt.Errorf("no admiral task tracks PR %s", args.PRURL)
			}
			feat, err := db.FindFeatureByIssue(task.IssueID)
			if err != nil {
				return nil, fmt.Errorf("find feature for issue %s: %w", task.IssueID, err)
			}
			// admiral_tasks.issue_identifier is nullable — fall back to
			// IssueID so the error string isn't `issue "" is not part
			// of...`, which would force the caller to guess what failed.
			issueRef := issueRef(task)
			if feat == nil {
				return nil, fmt.Errorf("issue %s is not part of any planner feature", issueRef)
			}
			fi, err := db.GetFeatureIssue(feat.ID, task.IssueID)
			if err != nil {
				return nil, fmt.Errorf("get acceptance criteria: %w", err)
			}
			if fi == nil {
				return nil, fmt.Errorf("no acceptance criteria recorded for issue %s", issueRef)
			}
			diff, err := gh.GetDiff(ctx, args.PRURL)
			if err != nil {
				return nil, fmt.Errorf("fetch PR diff: %w", err)
			}
			return prGetMaterialsResult{
				FeatureID:          feat.ID,
				FeatureName:        feat.Name,
				LinearIssueID:      task.IssueID,
				IssueIdentifier:    task.IssueIdentifier,
				AcceptanceCriteria: fi.AcceptanceCriteria,
				Branch:             task.Branch,
				Diff:               diff,
			}, nil
		},
	}
}

// --- feature_start ---

type featureStartArgs struct {
	Name             string `json:"name"`
	RequirementsText string `json:"requirements_text"`
	LinearProjectID  string `json:"linear_project_id"`
	SourceAgent      string `json:"source_agent,omitempty"`
}

type featureStartResult struct {
	FeatureID       string `json:"feature_id"`
	LinearProjectID string `json:"linear_project_id"`
}

func featureStartTool(db *store.Store) *toolDef {
	return &toolDef{
		Name: "feature_start",
		Description: "Open a new planner feature: bind a Linear project to the original " +
			"user requirements (the ground-truth text used at L2 acceptance). " +
			"Returns a feature_id the host agent passes to subsequent tools. " +
			"Errors with a UNIQUE-style message if linear_project_id is already " +
			"bound to another open feature — call feature_get_materials with " +
			"that project's existing feature_id instead.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Short slug for the feature (used in logs / search; need not be unique).",
				},
				"requirements_text": map[string]any{
					"type":        "string",
					"description": "Verbatim original requirements from the user. Use the raw conversation text, not a paraphrase, so L2 acceptance can judge against true intent.",
				},
				"linear_project_id": map[string]any{
					"type":        "string",
					"description": "The Linear project UUID this feature is scoped to. The host agent must create the project (or pick an existing one) before calling feature_start.",
				},
				"source_agent": map[string]any{
					"type":        "string",
					"description": "Optional telemetry tag (e.g. 'claude', 'codex').",
				},
			},
			"required": []string{"name", "requirements_text", "linear_project_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args featureStartArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.Name == "" || args.RequirementsText == "" || args.LinearProjectID == "" {
				return nil, fmt.Errorf("name, requirements_text, linear_project_id all required")
			}
			// uuid7 would be nicer (sortable) but google/uuid in this repo
			// is on v1.6.0 which doesn't expose v7. v4 (random) is fine.
			id := "f-" + uuid.NewString()
			f := store.Feature{
				ID:               id,
				Name:             args.Name,
				LinearProjectID:  args.LinearProjectID,
				RequirementsText: args.RequirementsText,
				SourceAgent:      args.SourceAgent,
			}
			if err := db.InsertFeature(f); err != nil {
				// UNIQUE on linear_project_id is the most likely failure;
				// surface it with the existing feature_id when we can find
				// it so the host agent can switch over instead of retrying.
				if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint") {
					if existing, _ := db.GetFeatureByLinearProject(args.LinearProjectID); existing != nil {
						return nil, fmt.Errorf("linear_project_id %s is already bound to feature %s (%q)",
							args.LinearProjectID, existing.ID, existing.Name)
					}
				}
				return nil, fmt.Errorf("insert feature: %w", err)
			}
			return featureStartResult{
				FeatureID:       id,
				LinearProjectID: args.LinearProjectID,
			}, nil
		},
	}
}

// --- issue_set_acceptance ---

type issueSetAcceptanceArgs struct {
	FeatureID          string `json:"feature_id"`
	LinearIssueID      string `json:"linear_issue_id"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
}

type issueSetAcceptanceResult struct {
	OK bool `json:"ok"`
}

func issueSetAcceptanceTool(db *store.Store) *toolDef {
	return &toolDef{
		Name: "issue_set_acceptance",
		Description: "Record the L1 acceptance criteria for a Linear issue inside a " +
			"feature. Idempotent: re-calling with the same (feature_id, " +
			"linear_issue_id) overwrites the criteria (used during decomposition " +
			"refinement). The host agent should call this for every issue it " +
			"creates after feature_start — without criteria, pr_verify_submit " +
			"has no standard to judge against.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"feature_id": map[string]any{
					"type":        "string",
					"description": "The feature ID returned by feature_start.",
				},
				"linear_issue_id": map[string]any{
					"type":        "string",
					"description": "The Linear issue UUID this criterion applies to.",
				},
				"acceptance_criteria": map[string]any{
					"type":        "string",
					"description": "Concrete, verifiable conditions a PR for this issue must meet. Be specific — vague criteria produce vague verdicts.",
				},
			},
			"required": []string{"feature_id", "linear_issue_id", "acceptance_criteria"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args issueSetAcceptanceArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.FeatureID == "" || args.LinearIssueID == "" || args.AcceptanceCriteria == "" {
				return nil, fmt.Errorf("feature_id, linear_issue_id, acceptance_criteria all required")
			}
			// Validate parent exists so a typo'd feature_id surfaces as a
			// crisp tool-level error instead of a FK constraint message.
			f, err := db.GetFeature(args.FeatureID)
			if err != nil {
				return nil, fmt.Errorf("get feature: %w", err)
			}
			if f == nil {
				return nil, fmt.Errorf("feature %q not found", args.FeatureID)
			}
			if err := db.UpsertFeatureIssue(store.FeatureIssue{
				FeatureID:          args.FeatureID,
				LinearIssueID:      args.LinearIssueID,
				AcceptanceCriteria: args.AcceptanceCriteria,
			}); err != nil {
				return nil, fmt.Errorf("upsert feature_issue: %w", err)
			}
			return issueSetAcceptanceResult{OK: true}, nil
		},
	}
}

// --- feature_get_materials ---

func featureGetMaterialsTool(db *store.Store) *toolDef {
	return &toolDef{
		Name: "feature_get_materials",
		Description: "Read back the planner's record of a feature for L2 acceptance: " +
			"the original user requirements text (ground truth) and the acceptance " +
			"criteria written for every issue in the feature. Use before judging " +
			"whether the feature as a whole matches user intent. Returns an error " +
			"if the feature_id is unknown.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"feature_id": map[string]any{
					"type":        "string",
					"description": "The feature ID returned by feature_start.",
				},
			},
			"required": []string{"feature_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args featureGetMaterialsArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.FeatureID == "" {
				return nil, fmt.Errorf("feature_id is required")
			}
			f, err := db.GetFeature(args.FeatureID)
			if err != nil {
				return nil, fmt.Errorf("get feature: %w", err)
			}
			if f == nil {
				return nil, fmt.Errorf("feature %q not found", args.FeatureID)
			}
			rawIssues, err := db.ListFeatureIssues(args.FeatureID)
			if err != nil {
				return nil, fmt.Errorf("list feature issues: %w", err)
			}
			issues := make([]featureIssuePayload, 0, len(rawIssues))
			for _, fi := range rawIssues {
				issues = append(issues, featureIssuePayload{
					LinearIssueID:      fi.LinearIssueID,
					AcceptanceCriteria: fi.AcceptanceCriteria,
					CreatedAt:          fi.CreatedAt,
				})
			}
			return featureGetMaterialsResult{
				Feature: &featurePayload{
					ID:               f.ID,
					Name:             f.Name,
					LinearProjectID:  f.LinearProjectID,
					RequirementsText: f.RequirementsText,
					SourceAgent:      f.SourceAgent,
					CreatedAt:        f.CreatedAt,
					ClosedAt:         f.ClosedAt,
				},
				Issues: issues,
			}, nil
		},
	}
}

// --- pr_verify_submit ---

type prVerifySubmitArgs struct {
	PRURL     string `json:"pr_url"`
	Verdict   string `json:"verdict"`
	Reasoning string `json:"reasoning"`
	Agent     string `json:"agent,omitempty"`
}

// prVerifySubmitResult records both what the planner decided and
// whether the side-effect (gh pr review) actually fired. submitted=
// false with verdict==prior verdict means idempotency caught it.
type prVerifySubmitResult struct {
	Submitted bool   `json:"submitted"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason,omitempty"`
}

// verdictToGHFlag maps planner verdicts to the gh-CLI argument
// PostReview accepts. needs_rebase folds into request_changes because
// admiral's dispatch_review handler treats request_changes as the
// trigger to spawn claude to address the feedback — perfect for a
// "rebase onto base" prompt.
func verdictToGHFlag(v string) string {
	switch v {
	case store.PRVerdictApprove:
		return "approve"
	case store.PRVerdictRequestChanges, store.PRVerdictNeedsRebase:
		return "request_changes"
	}
	return ""
}

// rebaseBodyPrefix is prepended to needs_rebase reasoning so the
// downstream review handler sees the intent before the planner's
// detailed comments.
const rebaseBodyPrefix = "Rebase onto base branch and resolve conflicts before re-review.\n\n"

func prVerifySubmitTool(db *store.Store, gh GitHubClient) *toolDef {
	return &toolDef{
		Name: "pr_verify_submit",
		Description: "Submit an L1 verdict on a PR. verdict must be one of approve, " +
			"request_changes, needs_rebase. Calls `gh pr review` and records an " +
			"audit row. Idempotent against the most recent verdict for the same " +
			"PR: if the latest recorded verdict already matches, the gh call is " +
			"skipped and submitted=false is returned. Use reasoning to explain " +
			"the decision; required for request_changes / needs_rebase (gh " +
			"enforces a body for those), optional for approve.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pr_url": map[string]any{
					"type":        "string",
					"description": "GitHub PR URL.",
				},
				"verdict": map[string]any{
					"type":        "string",
					"enum":        []string{store.PRVerdictApprove, store.PRVerdictRequestChanges, store.PRVerdictNeedsRebase},
					"description": "approve | request_changes | needs_rebase. needs_rebase is request_changes plus a rebase-themed body prefix.",
				},
				"reasoning": map[string]any{
					"type":        "string",
					"description": "Explanation surfaced to the reviewer / admiral autopilot. Required when verdict != approve.",
				},
				"agent": map[string]any{
					"type":        "string",
					"description": "Optional telemetry tag (e.g. 'claude').",
				},
			},
			"required": []string{"pr_url", "verdict"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args prVerifySubmitArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.PRURL == "" || args.Verdict == "" {
				return nil, fmt.Errorf("pr_url and verdict are required")
			}
			ghFlag := verdictToGHFlag(args.Verdict)
			if ghFlag == "" {
				return nil, fmt.Errorf("invalid verdict %q (want approve | request_changes | needs_rebase)", args.Verdict)
			}
			if (args.Verdict == store.PRVerdictRequestChanges || args.Verdict == store.PRVerdictNeedsRebase) &&
				strings.TrimSpace(args.Reasoning) == "" {
				return nil, fmt.Errorf("reasoning is required for verdict %q", args.Verdict)
			}
			if gh == nil {
				return nil, fmt.Errorf("GitHub client not configured (set ADMIRAL_GH_TOKEN)")
			}

			// Idempotency: skip when the latest recorded verdict already
			// matches. Don't even append an audit row — repeated calls
			// with the same conclusion don't carry new information.
			latest, err := db.GetLatestPRVerification(args.PRURL)
			if err != nil {
				return nil, fmt.Errorf("lookup prior verdict: %w", err)
			}
			if latest != nil && latest.Verdict == args.Verdict {
				return prVerifySubmitResult{
					Submitted: false,
					Verdict:   args.Verdict,
					Reason:    "latest recorded verdict already matches; gh call skipped",
				}, nil
			}

			body := args.Reasoning
			if args.Verdict == store.PRVerdictNeedsRebase {
				body = rebaseBodyPrefix + body
			}
			if err := gh.PostReview(ctx, args.PRURL, ghFlag, body); err != nil {
				return nil, fmt.Errorf("post review: %w", err)
			}
			// Audit only after gh succeeded — a failed PostReview that
			// still wrote an audit row would mislead future idempotency
			// checks into believing the verdict landed.
			if err := db.InsertPRVerification(store.PRVerification{
				PRURL:     args.PRURL,
				Verdict:   args.Verdict,
				Reasoning: args.Reasoning,
				Agent:     args.Agent,
			}); err != nil {
				return nil, fmt.Errorf("audit row: %w", err)
			}
			return prVerifySubmitResult{
				Submitted: true,
				Verdict:   args.Verdict,
			}, nil
		},
	}
}

// --- feature_followup_submit ---

type featureFollowupSubmitArgs struct {
	FeatureID          string `json:"feature_id"`
	Title              string `json:"title"`
	Description        string `json:"description,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
}

// featureFollowupSubmitResult returns the created Linear issue so the
// host agent can link it back to the user / surface it. The criteria is
// already registered against the issue in the planner, so a later
// pr_verify_submit on its PR has a standard to judge against.
type featureFollowupSubmitResult struct {
	LinearIssueID   string `json:"linear_issue_id"`
	IssueIdentifier string `json:"issue_identifier"`
	URL             string `json:"url,omitempty"`
}

// resolvePickup turns the discoverer's pickup rules into concrete Linear IDs
// for a team: the require_label's UUID (when a label is configured) and a
// workflow state whose type is in state_types (the lowest-position match, for
// determinism). Returns (nil, "", nil) when pickup isn't configured so the
// caller creates a plain issue. Errors loudly when a configured label or a
// pickable state can't be resolved — a silently un-pickable issue would
// defeat the whole purpose.
func resolvePickup(ctx context.Context, lc LinearClient, teamID string, pickup PickupRules) ([]string, string, error) {
	if len(pickup.StateTypes) == 0 {
		return nil, "", nil // not configured
	}
	var labelIDs []string
	if pickup.RequireLabel != "" {
		id, err := lc.GetTeamLabelID(ctx, teamID, pickup.RequireLabel)
		if err != nil {
			return nil, "", fmt.Errorf("resolve pickup label %q: %w", pickup.RequireLabel, err)
		}
		labelIDs = []string{id}
	}
	states, err := lc.GetWorkflowStates(ctx, teamID)
	if err != nil {
		return nil, "", fmt.Errorf("list workflow states for team %s: %w", teamID, err)
	}
	wanted := make(map[string]bool, len(pickup.StateTypes))
	for _, t := range pickup.StateTypes {
		wanted[t] = true
	}
	var pick *linear.WorkflowState
	for i := range states {
		s := &states[i]
		if wanted[s.Type] && (pick == nil || s.Position < pick.Position) {
			pick = s
		}
	}
	if pick == nil {
		return nil, "", fmt.Errorf("team %s has no workflow state of type %v (needed for discoverer pickup)", teamID, pickup.StateTypes)
	}
	return labelIDs, pick.ID, nil
}

func featureFollowupSubmitTool(db *store.Store, lc LinearClient, pickup PickupRules) *toolDef {
	return &toolDef{
		Name: "feature_followup_submit",
		Description: "Create a new Linear issue for an L2 follow-up gap and register its " +
			"acceptance criteria in the planner, in one call. Use after feature_get_materials " +
			"reveals the shipped PRs don't fully match user intent. The issue is created in " +
			"the feature's Linear project (and that project's team), labelled and stated so " +
			"admiral's discoverer auto-picks it; the criteria is recorded so a later " +
			"pr_verify_submit on the follow-up's PR has a standard to judge against. " +
			"Returns the created issue's id / identifier / url. Requires a Linear OAuth token " +
			"in the admiral DB.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"feature_id": map[string]any{
					"type":        "string",
					"description": "The feature ID returned by feature_start. The follow-up issue lands in this feature's Linear project.",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "Title for the new Linear issue. Should name the gap concisely.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Optional issue body — explain the gap and what a fix must do.",
				},
				"acceptance_criteria": map[string]any{
					"type":        "string",
					"description": "Concrete, verifiable conditions a PR for this follow-up must meet. Recorded as the issue's L1 criteria.",
				},
			},
			"required": []string{"feature_id", "title", "acceptance_criteria"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args featureFollowupSubmitArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.FeatureID == "" || args.Title == "" || args.AcceptanceCriteria == "" {
				return nil, fmt.Errorf("feature_id, title, acceptance_criteria all required")
			}
			if lc == nil {
				return nil, fmt.Errorf("Linear client not configured (no OAuth token in admiral DB)")
			}
			f, err := db.GetFeature(args.FeatureID)
			if err != nil {
				return nil, fmt.Errorf("get feature: %w", err)
			}
			if f == nil {
				return nil, fmt.Errorf("feature %q not found", args.FeatureID)
			}
			teamID, err := lc.GetProjectTeamID(ctx, f.LinearProjectID)
			if err != nil {
				return nil, fmt.Errorf("resolve team for project %s: %w", f.LinearProjectID, err)
			}
			// Resolve the discoverer's pickup label + a pickable state so the
			// issue is auto-discovered. Skipped entirely when pickup rules
			// aren't configured (planner launched without admiral's config).
			labelIDs, stateID, err := resolvePickup(ctx, lc, teamID, pickup)
			if err != nil {
				return nil, err
			}
			iss, err := lc.IssueCreate(ctx, linear.IssueCreateInput{
				TeamID:      teamID,
				ProjectID:   f.LinearProjectID,
				Title:       args.Title,
				Description: args.Description,
				LabelIDs:    labelIDs,
				StateID:     stateID,
			})
			if err != nil {
				return nil, fmt.Errorf("create linear issue: %w", err)
			}
			// Register criteria after the issue exists (we need its ID). If
			// this fails, the issue is created but has no criteria — the host
			// agent can recover with issue_set_acceptance. Surface the error
			// so it knows the registration step didn't land.
			if err := db.UpsertFeatureIssue(store.FeatureIssue{
				FeatureID:          args.FeatureID,
				LinearIssueID:      iss.ID,
				AcceptanceCriteria: args.AcceptanceCriteria,
			}); err != nil {
				return nil, fmt.Errorf("issue %s created but registering criteria failed: %w", iss.Identifier, err)
			}
			return featureFollowupSubmitResult{
				LinearIssueID:   iss.ID,
				IssueIdentifier: iss.Identifier,
				URL:             iss.URL,
			}, nil
		},
	}
}

// --- feature_close ---

type featureCloseArgs struct {
	FeatureID string `json:"feature_id"`
}

type featureCloseResult struct {
	Closed bool   `json:"closed"`
	Reason string `json:"reason,omitempty"`
}

func featureCloseTool(db *store.Store) *toolDef {
	return &toolDef{
		Name: "feature_close",
		Description: "Mark a feature closed after L2 acceptance has confirmed all " +
			"PRs match the user's intent and any follow-up issues have been " +
			"recorded. Idempotent: closing an already-closed feature returns " +
			"closed=false with a reason. Errors only when feature_id is unknown.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"feature_id": map[string]any{
					"type":        "string",
					"description": "The feature ID returned by feature_start.",
				},
			},
			"required": []string{"feature_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args featureCloseArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			if args.FeatureID == "" {
				return nil, fmt.Errorf("feature_id is required")
			}
			closed, err := db.CloseFeature(args.FeatureID)
			if err != nil {
				// ErrFeatureNotFound bubbles up here — surface it as a
				// readable tool error so the host agent sees "not found"
				// instead of the sentinel string.
				return nil, fmt.Errorf("close feature: %w", err)
			}
			if !closed {
				return featureCloseResult{
					Closed: false,
					Reason: "feature was already closed",
				}, nil
			}
			return featureCloseResult{Closed: true}, nil
		},
	}
}
