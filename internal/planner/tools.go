package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

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

// PRDiffer fetches the unified diff for a pull request. The concrete
// implementation in production is *github.Client; tests inject a stub.
// Kept as a narrow interface so the planner package does not depend on
// the full GitHub client surface (which carries cobra / gh-CLI shell-
// out machinery the MCP server has no business needing).
type PRDiffer interface {
	GetDiff(ctx context.Context, prURL string) (string, error)
}

// BuildTools wires every tool the planner exposes, closing over the
// Store + PRDiffer dependencies. cmd/admiral-planner-mcp calls this
// once at startup and passes the result to NewServer. Adding a tool
// means adding one entry here plus its handler below.
//
// gh may be nil — tools that need it (pr_get_materials) return an MCP
// error in that case rather than crashing the process. This lets the
// server boot in environments where ADMIRAL_GH_TOKEN is not configured
// (e.g. read-only inspection of planner state).
func BuildTools(db *store.Store, gh PRDiffer) map[string]*toolDef {
	return map[string]*toolDef{
		"feature_start":          featureStartTool(db),
		"issue_set_acceptance":   issueSetAcceptanceTool(db),
		"feature_get_materials":  featureGetMaterialsTool(db),
		"issue_list_by_feature":  issueListByFeatureTool(db),
		"pr_get_materials":       prGetMaterialsTool(db, gh),
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
			if feat == nil {
				return nil, fmt.Errorf("issue %s is not part of any planner feature", task.IssueIdentifier)
			}
			fi, err := db.GetFeatureIssue(feat.ID, task.IssueID)
			if err != nil {
				return nil, fmt.Errorf("get acceptance criteria: %w", err)
			}
			if fi == nil {
				return nil, fmt.Errorf("no acceptance criteria recorded for issue %s", task.IssueIdentifier)
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
