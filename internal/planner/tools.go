package planner

import (
	"context"
	"encoding/json"
	"fmt"

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

// BuildTools wires every tool the planner exposes, closing over the
// Store dependency. cmd/admiral-planner-mcp calls this once at
// startup and passes the result to NewServer. Adding a tool means
// adding one entry here plus its handler below.
func BuildTools(db *store.Store) map[string]*toolDef {
	return map[string]*toolDef{
		"feature_get_materials": featureGetMaterialsTool(db),
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
