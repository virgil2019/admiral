package autopilot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// productVerifyMaterial is the input to buildProductVerifyPrompt: the product
// it judges (a Linear project), the repo-relative path to the product
// documentation (the ground-truth spec living in the repo alongside the
// code), and the project's existing top-level "feature" issues with their
// shipped status. The judge runs read-only in the repo working directory, so
// it reads the product doc itself — admiral only points it at the path (or
// tells it to discover one when none is configured, mirroring verify_cmd's
// degradation).
type productVerifyMaterial struct {
	ProjectName    string
	ProductDocPath string // repo-relative; "" => judge discovers the doc
	Features       []productFeature
}

// productFeature is one existing top-level issue (a "feature") under the
// project, with enough context for the judge to tell whether the capability
// it represents is already shipped.
type productFeature struct {
	Identifier string
	Title      string
	StateName  string
	Shipped    bool // Linear state type == "completed"
}

// buildProductVerifyPrompt renders the product-level verification prompt. It
// instructs the agent (read-only, cwd = repo root) to read the product doc,
// compare it against the shipped features, and reply with ONLY the
// verifyVerdict JSON — the same shape L2 verify uses (parseVerifyVerdict is
// shared). "complete" means the product fully realises its documentation;
// each gap is a missing/incomplete capability to be filed as a NEW top-level
// feature issue (later decomposed and shipped).
func buildProductVerifyPrompt(m productVerifyMaterial) string {
	var b strings.Builder
	b.WriteString("You are verifying whether a software PRODUCT, taken as a whole, fully realises its product documentation.\n\n")
	fmt.Fprintf(&b, "Product (Linear project): %s\n\n", m.ProjectName)

	b.WriteString("## The product documentation (ground truth)\n\n")
	if p := strings.TrimSpace(m.ProductDocPath); p != "" {
		fmt.Fprintf(&b, "The complete product documentation lives in this repository at `%s` (relative to the repo root, your current working directory). Read it now.\n", p)
	} else {
		b.WriteString("No documentation path is configured. Discover the complete product documentation yourself by exploring the repository (look for `README`, a `docs/` directory, `PRD`/`SPEC`/`PRODUCT` files, etc.) and read it.\n")
	}

	b.WriteString("\n## Features already planned / shipped\n\n")
	b.WriteString("Each item below is a top-level \"feature\" issue under this product. \"shipped\" means its Linear state is a completed type.\n")
	if len(m.Features) == 0 {
		b.WriteString("\n(no top-level feature issues exist yet)\n")
	}
	for _, f := range m.Features {
		status := "NOT shipped"
		if f.Shipped {
			status = "shipped"
		}
		fmt.Fprintf(&b, "- %s: %s [state: %s — %s]\n", f.Identifier, f.Title, f.StateName, status)
	}

	b.WriteString(`
Judge whether the SHIPPED features, taken together, fully realise the product documentation.

Respond with ONLY a JSON object — no prose, no markdown fences — in exactly this shape:
{
  "complete": true,
  "summary": "<one-line judgment>",
  "gaps": [
    {"title": "<short feature title>", "description": "<the capability the product doc requires that is missing or unfinished>", "acceptance_criteria": "<concrete, verifiable conditions this feature must meet>"}
  ]
}

Rules:
- If the shipped features fully realise the product documentation, set "complete": true and "gaps": [].
- Otherwise set "complete": false and list one gap per missing/incomplete capability. Each gap becomes a NEW top-level feature issue that will be decomposed into shippable tasks — so make each gap a coherent, self-contained feature, not a one-line tweak.
- Judge only against what the product documentation actually requires, not nice-to-haves. A capability that exists as a NOT-shipped feature is still a gap (it isn't realised yet) — but do NOT duplicate it; reference the existing identifier in the description instead of inventing a new feature for the same capability.`)
	return b.String()
}

// HandleProductVerifyEvent guards and launches one round of autonomous
// product-level verification for a product (a Linear project). projectID is
// carried on the events_inbox row's session_id by whatever triggered the
// verify (MVP: the admin manual-trigger endpoint).
//
// Mirrors HandleVerifyEvent: the guard runs synchronously (terminal-status
// short-circuit, then bump + round cap) so round accounting is deterministic;
// the heavy judge run is dispatched to a background goroutine gated by the
// same runSlots semaphore. The round is consumed at bump time regardless of
// how the run ends, so a persistently non-converging product escalates after
// the cap instead of looping forever.
func (o *Orchestrator) HandleProductVerifyEvent(ctx context.Context, projectID string) {
	if projectID == "" {
		o.logger.Warn("product_verify_dispatch_empty_project")
		return
	}

	pv, err := o.db.GetProductVerification(projectID)
	if err != nil {
		o.logger.Error("product_verify_get_failed", "project", projectID, "err", err)
		return
	}
	if pv != nil && pv.Status != store.TaskVerifyActive {
		o.logger.Info("product_verify_skip_terminal_status",
			"project", projectID, "status", pv.Status, "rounds", pv.Rounds)
		return
	}
	bumped, err := o.db.BumpProductVerificationRound(projectID)
	if err != nil {
		o.logger.Error("product_verify_bump_round_failed", "project", projectID, "err", err)
		return
	}
	maxRounds := o.cfg.VerifyMaxRounds
	if maxRounds <= 0 {
		maxRounds = config.DefaultVerifyMaxRounds // defensive: config defaulting normally pins this
	}
	if bumped.Rounds > maxRounds {
		o.escalateProductVerify(projectID, bumped.Rounds, maxRounds)
		return
	}

	o.logger.Info("product_verify_round_starting",
		"project", projectID, "round", bumped.Rounds, "cap", maxRounds)
	go o.runProductVerify(projectID)
}

// runProductVerify is the background goroutine for one product-verification
// round: gather materials → headless judge (read-only, in the repo) → apply
// the verdict. Owns its own run slot and timeout ctx (same budget as the
// L2 verify / autopilot claude runs). Every failure logs and returns, leaving
// the verification 'active' for a later (manual) re-trigger.
func (o *Orchestrator) runProductVerify(projectID string) {
	defer func() {
		if r := recover(); r != nil {
			o.logger.Error("product_verify_run_panic", "project", projectID, "panic", r)
		}
	}()

	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	mat, repoDir, teamID, err := o.gatherProductVerifyMaterial(ctx, projectID)
	if err != nil {
		o.logger.Error("product_verify_gather_failed", "project", projectID, "err", err)
		return
	}

	raw, err := o.productVerifyRunner(ctx, repoDir, buildProductVerifyPrompt(mat))
	if err != nil {
		o.logger.Error("product_verify_claude_failed", "project", projectID, "err", err)
		return
	}
	verdict, err := parseVerifyVerdict(raw)
	if err != nil {
		o.logger.Error("product_verify_parse_verdict_failed", "project", projectID, "err", err)
		return
	}

	o.applyProductVerifyVerdict(ctx, projectID, teamID, verdict)
}

// gatherProductVerifyMaterial assembles the prompt inputs: the project's repo
// (for the judge's cwd + the configured product-doc path) and its top-level
// feature issues. Returns the repo dir (claude's cwd) and the project's team
// id (used by the apply step to file gap features).
func (o *Orchestrator) gatherProductVerifyMaterial(ctx context.Context, projectID string) (mat productVerifyMaterial, repoDir, teamID string, err error) {
	repo, err := o.db.GetRepoByProjectID(projectID)
	if err != nil {
		return productVerifyMaterial{}, "", "", fmt.Errorf("get repo for project %s: %w", projectID, err)
	}
	if repo == nil {
		return productVerifyMaterial{}, "", "", fmt.Errorf("no repo configured for project %s", projectID)
	}
	teamID, err = o.lc.GetProjectTeamID(ctx, projectID)
	if err != nil {
		return productVerifyMaterial{}, "", "", fmt.Errorf("get project team: %w", err)
	}
	issues, err := o.lc.ListProjectTopLevelIssues(ctx, projectID)
	if err != nil {
		return productVerifyMaterial{}, "", "", fmt.Errorf("list top-level issues: %w", err)
	}
	// ListProjectTopLevelIssues caps at 250 (its search ceiling). Hitting the
	// cap means enumeration was truncated — the judge then can't see the
	// dropped features and would re-file them as gaps. Surface it loudly
	// rather than silently miscount; pagination is a follow-up.
	if len(issues) == 250 {
		o.logger.Warn("product_verify_toplevel_issues_capped",
			"project", projectID, "count", len(issues),
			"note", "enumeration hit the 250 cap; newest features may be missing and could be re-filed as gaps")
	}

	mat = productVerifyMaterial{
		ProjectName:    repo.ProjectName,
		ProductDocPath: repo.ProductDocPath,
	}
	for _, iss := range issues {
		mat.Features = append(mat.Features, productFeature{
			Identifier: iss.Identifier,
			Title:      iss.Title,
			StateName:  iss.StateName,
			Shipped:    iss.StateType == "completed",
		})
	}
	return mat, repo.RepoDir, teamID, nil
}

// applyProductVerifyVerdict acts on the judge's verdict. complete → mark the
// product verification closed (no further triggers act on it). gaps → file
// each as a NEW top-level feature issue (no parent, no pickup label, no
// state) so it surfaces in Linear for the human to decompose + activate —
// product-level gaps are whole features, deliberately NOT auto-shipped (the
// human gate). The verification stays 'active' so a later re-trigger (after
// the gap features ship) can re-judge and converge.
func (o *Orchestrator) applyProductVerifyVerdict(ctx context.Context, projectID, teamID string, v *verifyVerdict) {
	if v.Complete {
		if err := o.db.SetProductVerificationStatus(projectID, store.TaskVerifyClosed); err != nil {
			o.logger.Error("product_verify_complete_set_status_failed", "project", projectID, "err", err)
			return
		}
		o.logger.Info("product_verify_complete", "project", projectID, "summary", v.Summary)
		return
	}

	created := 0
	for _, g := range v.Gaps {
		issue, err := o.lc.IssueCreate(ctx, linear.IssueCreateInput{
			TeamID:      teamID,
			ProjectID:   projectID,
			Title:       g.Title,
			Description: gapBody(g),
			// No LabelIDs (unlabeled → not auto-picked), no ParentID
			// (top-level feature), no StateID (lands in the team's default
			// state) — the human decomposes + activates it.
		})
		if err != nil {
			o.logger.Error("product_verify_gap_issue_create_failed",
				"project", projectID, "title", g.Title, "err", err)
			continue
		}
		created++
		o.logger.Info("product_verify_gap_filed",
			"project", projectID, "gap", issue.Identifier, "title", g.Title)
	}
	o.logger.Info("product_verify_gaps_done",
		"project", projectID, "gaps", len(v.Gaps), "created", created, "summary", v.Summary)
}

// escalateProductVerify hands a non-converging product (round cap reached)
// off to a human. Unlike L2 verify, a project has no agent-session thread to
// comment on, so escalation is recorded via status + a loud log only; the
// already-filed gap feature issues remain the human-visible signal in Linear.
func (o *Orchestrator) escalateProductVerify(projectID string, rounds, maxRounds int) {
	if err := o.db.SetProductVerificationStatus(projectID, store.TaskVerifyEscalated); err != nil {
		o.logger.Error("product_verify_escalate_set_status_failed", "project", projectID, "err", err)
	}
	o.logger.Warn("product_verify_escalated",
		"project", projectID, "rounds", rounds, "cap", maxRounds)
}
