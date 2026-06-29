package autopilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/georgehuang/admiral/internal/cascade"
	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// verifyVerdict is the structured judgment a headless agent returns for an
// L2 verification: does the shipped work, taken together, satisfy the task's
// original requirements? admiral parses this and acts on it (close the task,
// or file the gaps as follow-up sub-issues) — the agent itself takes no
// action, mirroring the review-dispatch split (agent judges, admiral does I/O).
type verifyVerdict struct {
	Complete bool        `json:"complete"`
	Summary  string      `json:"summary"`
	Gaps     []verifyGap `json:"gaps"`
}

// verifyGap is one shortfall the agent found. Title + Description + criteria
// become a follow-up sub-issue: title is the issue title, description+criteria
// its body (the criteria is the standard a fix's PR is later judged against).
type verifyGap struct {
	Title              string `json:"title"`
	Description        string `json:"description"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
}

// verifyMaterial is the input to buildVerifyPrompt: the task's PRD (the
// parent issue's description, the ground-truth requirements) plus what was
// actually shipped for each sub-issue. Optionally also the result of running
// the repo's configured verify command — fed to the judge as a hard input so
// build/compile errors cannot land complete=true via diff-only judgment.
type verifyMaterial struct {
	ParentIdentifier string
	PRD              string
	Subs             []verifySubMaterial
	BuildResult      *buildResultMaterial
}

type verifySubMaterial struct {
	Identifier string
	Title      string
	PRURL      string
	Diff       string
}

// buildResultMaterial describes the result of running the repo's verify_cmd
// at L2 verify time. Always present when a verify_cmd is configured; nil
// when the repo has no verify_cmd (verify falls back to diff-only LLM
// judgment for that repo — see warnReposMissingVerifyCmd in
// cmd/admiral-autopilot/main.go).
type buildResultMaterial struct {
	// Command is the shell string admiral ran (echoed into the prompt so
	// the judge can name it in any "Build/test failure" gap).
	Command string
	// ExitCode is the process exit code. 0 = build/test passed.
	ExitCode int
	// Output is the (capped, tail-truncated) combined stdout+stderr.
	Output string
	// TimedOut is true when the verify command was killed by the timeout
	// rather than exiting on its own.
	TimedOut bool
	// LaunchErr is non-empty when the verify command could not be launched
	// at all (sh missing, IO error). ExitCode is then not meaningful.
	LaunchErr string
}

// passed reports whether the build/test result counts as a clean pass.
// Anything other than exit code 0 (timeout, launch error, non-zero exit)
// is a fail.
func (b *buildResultMaterial) passed() bool {
	if b == nil {
		return false
	}
	return b.LaunchErr == "" && !b.TimedOut && b.ExitCode == 0
}

// buildVerifyPrompt renders the L2 verification prompt. It instructs the
// agent to judge the shipped work against the PRD and reply with ONLY the
// verifyVerdict JSON — parseVerifyVerdict tolerates stray prose/fences, but a
// clean object keeps the parse unambiguous.
func buildVerifyPrompt(m verifyMaterial) string {
	var b strings.Builder
	b.WriteString("You are verifying whether a completed software task fully satisfies its original requirements.\n\n")
	b.WriteString("ORIGINAL REQUIREMENTS (the task's PRD):\n")
	b.WriteString(strings.TrimSpace(m.PRD))
	b.WriteString("\n\nThe task was decomposed into sub-tasks, each shipped as a merged PR. Here is what was built:\n")
	if len(m.Subs) == 0 {
		b.WriteString("\n(no sub-task diffs available)\n")
	}
	for _, s := range m.Subs {
		fmt.Fprintf(&b, "\n### %s: %s\n", s.Identifier, s.Title)
		if s.PRURL != "" {
			fmt.Fprintf(&b, "PR: %s\n", s.PRURL)
		}
		diff := strings.TrimSpace(s.Diff)
		if diff == "" {
			b.WriteString("(diff unavailable)\n")
		} else {
			b.WriteString("```diff\n")
			b.WriteString(diff)
			b.WriteString("\n```\n")
		}
	}
	if br := m.BuildResult; br != nil {
		b.WriteString("\n## Project build / test result (from admiral, hard input)\n\n")
		fmt.Fprintf(&b, "admiral ran the configured verify command BEFORE asking you to judge.\n")
		fmt.Fprintf(&b, "Command: `%s`\n", br.Command)
		switch {
		case br.LaunchErr != "":
			fmt.Fprintf(&b, "Launch error: %s (the command could NOT be run; treat as a non-passing build)\n", br.LaunchErr)
		case br.TimedOut:
			b.WriteString("Result: TIMED OUT (treat as a non-passing build)\n")
		default:
			fmt.Fprintf(&b, "Exit code: %d\n", br.ExitCode)
		}
		if out := strings.TrimSpace(br.Output); out != "" {
			b.WriteString("Output tail (combined stdout+stderr, capped):\n```\n")
			b.WriteString(out)
			b.WriteString("\n```\n")
		} else {
			b.WriteString("(no output captured)\n")
		}
		b.WriteString(`
**Hard rule on the build result:** if the result above is anything other than a clean exit code 0 (i.e. non-zero exit, launch error, or timeout), you MUST set "complete": false and include one gap whose title starts with "Build/test failure" describing the error from the output above. Do NOT mark the task complete while the build is broken — admiral will reject an inconsistent verdict.
`)
	}
	b.WriteString(`
Judge whether the shipped work, taken together, fully satisfies the ORIGINAL REQUIREMENTS.

Respond with ONLY a JSON object — no prose, no markdown fences — in exactly this shape:
{
  "complete": true,
  "summary": "<one-line judgment>",
  "gaps": [
    {"title": "<short issue title>", "description": "<what is missing and why>", "acceptance_criteria": "<concrete, verifiable conditions a fix's PR must meet>"}
  ]
}

Rules:
- If the work fully satisfies the requirements, set "complete": true and "gaps": [].
- Otherwise set "complete": false and list one gap per missing/incorrect piece. Each gap must be independently shippable as its own sub-task.
- Be strict but fair: only flag gaps that are genuine shortfalls against the stated requirements, not nice-to-haves.`)
	return b.String()
}

// parseVerifyVerdict extracts the verdict JSON from an agent's raw stdout.
// The agent is told to emit only the object, but models sometimes wrap it in
// prose or ```json fences — so we slice from the first '{' to the last '}'
// and parse that. Returns an error when no object is found or it is malformed,
// or when the verdict is internally inconsistent (complete with gaps, or not
// complete with none) — an ambiguous verdict must not silently drive actions.
func parseVerifyVerdict(raw string) (*verifyVerdict, error) {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("no JSON object found in verdict output")
	}
	var v verifyVerdict
	if err := json.Unmarshal([]byte(raw[start:end+1]), &v); err != nil {
		return nil, fmt.Errorf("parse verdict JSON: %w", err)
	}
	if v.Complete && len(v.Gaps) > 0 {
		return nil, fmt.Errorf("inconsistent verdict: complete=true but %d gaps listed", len(v.Gaps))
	}
	if !v.Complete && len(v.Gaps) == 0 {
		return nil, fmt.Errorf("inconsistent verdict: complete=false but no gaps listed")
	}
	for i, g := range v.Gaps {
		if strings.TrimSpace(g.Title) == "" {
			return nil, fmt.Errorf("gap %d has an empty title", i)
		}
	}
	return &v, nil
}

// HandleVerifyEvent guards and launches one round of autonomous L2
// verification for a task (a parent Linear issue). parentID is the parent
// issue's Linear ID, carried on the events_inbox row's session_id by the
// discoverer's verify enqueue.
//
// The guard runs synchronously (terminal-status short-circuit, then bump +
// round cap) so round accounting is deterministic and the worker's serial
// drain can't double-count. The heavy work — gather PRD + sub diffs, run the
// headless judge, apply the verdict — is dispatched to a background goroutine
// gated by the same runSlots semaphore as the other claude runs, so a 30-min
// judge does not block the worker's drain loop. Returns once the goroutine is
// launched, mirroring HandleReviewEvent.
//
// The round is consumed at bump time regardless of how the launched run ends:
// a persistently failing judge therefore escalates after the cap instead of
// looping forever. A run that fails for transient reasons leaves the
// verification 'active', so the discoverer re-enqueues on the next merge.
func (o *Orchestrator) HandleVerifyEvent(ctx context.Context, parentID string) {
	if parentID == "" {
		o.logger.Warn("verify_dispatch_empty_parent")
		return
	}

	tv, err := o.db.GetTaskVerification(parentID)
	if err != nil {
		o.logger.Error("verify_get_task_verification_failed", "parent", parentID, "err", err)
		return
	}
	if tv != nil && tv.Status != store.TaskVerifyActive {
		o.logger.Info("verify_skip_terminal_status",
			"parent", parentID, "status", tv.Status, "rounds", tv.Rounds)
		return
	}
	bumped, err := o.db.BumpTaskVerificationRound(parentID)
	if err != nil {
		o.logger.Error("verify_bump_round_failed", "parent", parentID, "err", err)
		return
	}
	maxRounds := o.cfg.VerifyMaxRounds
	if maxRounds <= 0 {
		maxRounds = config.DefaultVerifyMaxRounds // defensive: config defaulting normally pins this
	}
	if bumped.Rounds > maxRounds {
		o.escalateVerify(ctx, parentID, bumped.Rounds, maxRounds)
		return
	}

	o.logger.Info("verify_round_starting",
		"parent", parentID, "round", bumped.Rounds, "cap", maxRounds)
	go o.runVerify(parentID)
}

// runVerify is the background goroutine for one verification round: gather
// materials → headless judge → apply the verdict. It owns its own run slot
// and timeout ctx (same budget as the review/autopilot claude runs). Every
// failure logs and returns, leaving the verification 'active' for a later
// retry.
func (o *Orchestrator) runVerify(parentID string) {
	defer func() {
		if r := recover(); r != nil {
			o.logger.Error("verify_run_panic", "parent", parentID, "panic", r)
		}
	}()

	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	mat, repoDir, teamID, projectID, verifyCmd, err := o.gatherVerifyMaterial(ctx, parentID)
	if err != nil {
		o.logger.Error("verify_gather_failed", "parent", parentID, "err", err)
		return
	}

	// Hard gate: when a verify_cmd is configured for this repo, admiral runs
	// it BEFORE the judge and threads the result into the prompt. The judge
	// is told to treat a non-zero exit as a forced complete=false; admiral
	// then enforces that decision after parse (see overrideVerdictOnBuildFail
	// below) so a slip-up in the LLM cannot land a "complete" task with a
	// broken build. Repos without verify_cmd skip this step — verify falls
	// back to the original diff-only behavior (warned about at boot).
	if verifyCmd != "" {
		br := runRepoVerify(ctx, repoDir, verifyCmd, o.cfg.MaxRunSeconds)
		mat.BuildResult = &buildResultMaterial{
			Command:  verifyCmd,
			ExitCode: br.ExitCode,
			Output:   br.Output,
			TimedOut: br.TimedOut,
		}
		if br.Err != nil && !br.TimedOut {
			// Launch / IO failure (NOT a non-zero process exit). Surface so the
			// judge can see something went wrong, but keep going — the prompt
			// will still let the judge file ordinary gaps if it can spot any.
			mat.BuildResult.LaunchErr = br.Err.Error()
			o.logger.Warn("verify_build_launch_failed",
				"parent", parentID, "err", br.Err)
		} else if br.TimedOut {
			o.logger.Warn("verify_build_timed_out",
				"parent", parentID, "timeout_sec", o.cfg.MaxRunSeconds)
		} else {
			o.logger.Info("verify_build_run",
				"parent", parentID, "exit_code", br.ExitCode, "output_bytes", len(br.Output))
		}
	}

	raw, err := o.verifyRunner(ctx, repoDir, buildVerifyPrompt(mat))
	if err != nil {
		o.logger.Error("verify_claude_failed", "parent", parentID, "err", err)
		return
	}
	verdict, err := parseVerifyVerdict(raw)
	if err != nil {
		o.logger.Error("verify_parse_verdict_failed", "parent", parentID, "err", err)
		return
	}

	// Defense in depth: when admiral observed a non-passing build, the parent
	// task is NOT complete regardless of what the judge said. Override and
	// synthesize a Build/test failure gap so the verify loop re-spins on a fix
	// PR. The judge prompt already requires this on its own; this enforces it.
	overrideVerdictOnBuildFail(verdict, mat.BuildResult)

	o.applyVerifyVerdict(ctx, parentID, mat.ParentIdentifier, teamID, projectID, verdict)
}

// overrideVerdictOnBuildFail enforces the "non-passing build ⇒ not complete"
// invariant on the parsed verdict. Mutates v in place: forces Complete=false
// and ensures at least one gap describes the build failure when no existing
// gap mentions it. The judge prompt asks for this behavior; this is admiral
// guaranteeing it.
func overrideVerdictOnBuildFail(v *verifyVerdict, br *buildResultMaterial) {
	if br == nil || br.passed() {
		return
	}
	v.Complete = false
	for _, g := range v.Gaps {
		if strings.Contains(strings.ToLower(g.Title), "build") ||
			strings.Contains(strings.ToLower(g.Title), "test failure") ||
			strings.Contains(strings.ToLower(g.Title), "compile") {
			return // judge already filed a build/test/compile gap; don't duplicate
		}
	}
	v.Gaps = append(v.Gaps, verifyGap{
		Title:              "Build/test failure",
		Description:        renderBuildFailureGapBody(br),
		AcceptanceCriteria: fmt.Sprintf("The configured verify command (`%s`) exits 0.", br.Command),
	})
}

// renderBuildFailureGapBody is the description body of an admiral-synthesized
// Build/test failure gap. Names the command admiral ran, the exit / timeout /
// launch outcome, and the captured tail output so the fix-PR agent has the
// error in hand without re-running the build itself.
func renderBuildFailureGapBody(br *buildResultMaterial) string {
	var b strings.Builder
	fmt.Fprintf(&b, "admiral ran the configured verify command (`%s`) before judging this task and observed:\n\n",
		br.Command)
	switch {
	case br.LaunchErr != "":
		fmt.Fprintf(&b, "**Could not launch the verify command**: %s\n\n", br.LaunchErr)
	case br.TimedOut:
		b.WriteString("**The verify command timed out.** Output captured so far is below.\n\n")
	default:
		fmt.Fprintf(&b, "**Exit code: %d**\n\n", br.ExitCode)
	}
	if out := strings.TrimSpace(br.Output); out != "" {
		b.WriteString("Output tail:\n\n```\n")
		b.WriteString(out)
		b.WriteString("\n```\n")
	} else {
		b.WriteString("(no output captured)\n")
	}
	return b.String()
}

// gatherVerifyMaterial assembles the verify prompt's inputs: the parent
// issue's description (the PRD / ground-truth requirements) plus, per
// sub-issue, its title, PR url, and merged diff. PR url comes from the
// admiral_tasks row; the diff from GitHub. Both are best-effort per sub — a
// sub shipped by a human (no admiral task) still appears in the prompt, just
// without a diff. Returns the repo dir (claude's cwd), the parent's team and
// project ids (used by the apply step), and the repo's configured
// verify_cmd ("" when none is set — runVerify then skips the build hard gate
// and verify degrades to diff-only judgment).
func (o *Orchestrator) gatherVerifyMaterial(ctx context.Context, parentID string) (mat verifyMaterial, repoDir, teamID, projectID, verifyCmd string, err error) {
	parent, err := o.lc.GetIssue(ctx, parentID)
	if err != nil {
		return verifyMaterial{}, "", "", "", "", fmt.Errorf("get parent issue: %w", err)
	}
	repo, err := o.db.GetRepoByProjectID(parent.ProjectID)
	if err != nil {
		return verifyMaterial{}, "", "", "", "", fmt.Errorf("get repo for project %s: %w", parent.ProjectID, err)
	}
	if repo == nil {
		return verifyMaterial{}, "", "", "", "", fmt.Errorf("no repo configured for project %s", parent.ProjectID)
	}
	subs, err := o.lc.GetSubIssues(ctx, parentID)
	if err != nil {
		return verifyMaterial{}, "", "", "", "", fmt.Errorf("get sub-issues: %w", err)
	}

	mat = verifyMaterial{
		ParentIdentifier: parent.Identifier,
		PRD:              parent.Description,
	}
	for _, sub := range subs {
		sm := verifySubMaterial{Identifier: sub.Identifier}
		// SubIssue carries only id/identifier/state; fetch the title for a
		// readable prompt heading. Non-fatal — heading degrades to just the
		// identifier when the lookup fails.
		if iss, gerr := o.lc.GetIssue(ctx, sub.ID); gerr == nil {
			sm.Title = iss.Title
		} else {
			o.logger.Warn("verify_get_sub_issue_failed", "sub", sub.Identifier, "err", gerr)
		}
		if task, terr := o.db.GetAdmiralTaskByIssue(sub.ID); terr == nil && task != nil && task.PRURL != "" {
			sm.PRURL = task.PRURL
			if diff, derr := o.prClient.GetDiff(ctx, task.PRURL); derr == nil {
				sm.Diff = diff
			} else {
				o.logger.Warn("verify_get_diff_failed",
					"sub", sub.Identifier, "pr", task.PRURL, "err", derr)
			}
		}
		mat.Subs = append(mat.Subs, sm)
	}
	return mat, repo.RepoDir, parent.TeamID, parent.ProjectID, repo.VerifyCmd, nil
}

// applyVerifyVerdict acts on the judge's verdict. complete → flip the parent
// to a completed-type Linear state, mark the verification closed (no further
// verify events act on it), and cascade one hop up (when this parent itself
// sits under another parent whose other children are all done, trigger the
// upper layer's verify). gaps → file each as a follow-up sub-issue in a
// pickable state with the discoverer's label, leaving the verification
// 'active' so the re-shipped fixes trigger another round.
//
// In both branches the judge's one-line summary is persisted on the
// task_verifications row before any other action. An upper-layer verify
// run reads it via gatherVerifyMaterial as the digest of THIS parent's
// shipped work (an intermediate parent has no single PR diff of its own —
// its "shipped work" is the union of its children, summarized here).
//
// Summary write order: BEFORE the Linear flip on purpose. If the flip
// fails the row stays 'active' and re-verifies next round, at which point
// the freshly-computed summary overwrites — so the column always reflects
// the latest judgment, never a stale earlier round. (The alternative —
// write summary after the flip — would leave a closed row with the prior
// round's summary visible to upper-layer cascade reads if the flip itself
// failed downstream.)
//
// parentIdentifier is the parent's human-readable Linear identifier
// (e.g. "GEO-12") used only for log narrative; the cascade walk hands
// it to the helper for clean enqueue log lines instead of UUIDs.
func (o *Orchestrator) applyVerifyVerdict(ctx context.Context, parentID, parentIdentifier, teamID, projectID string, v *verifyVerdict) {
	if err := o.db.SetTaskVerificationSummary(parentID, v.Summary); err != nil {
		// All paths to applyVerifyVerdict go through HandleVerifyEvent →
		// BumpTaskVerificationRound, which guarantees the row exists. A
		// failure here is a defensive-invariant violation (caller bypassed
		// the bump, or DB transport error). Surface loudly and abort so
		// the cascade does not fire against a row that can't carry the
		// digest the upper hop will read.
		o.logger.Error("verify_set_summary_failed", "parent", parentID, "err", err)
		return
	}

	if v.Complete {
		stateID, err := o.stateIDByType(ctx, teamID, "completed")
		if err != nil {
			o.logger.Error("verify_complete_state_lookup_failed", "parent", parentID, "err", err)
			return
		}
		if stateID == "" {
			o.logger.Error("verify_complete_no_completed_state", "parent", parentID, "team", teamID)
			return
		}
		if err := o.lc.IssueUpdate(ctx, parentID, stateID); err != nil {
			o.logger.Error("verify_complete_issue_update_failed", "parent", parentID, "err", err)
			return
		}
		if err := o.db.SetTaskVerificationStatus(parentID, store.TaskVerifyClosed); err != nil {
			// Parent is already closed in Linear; a stuck 'active' row would
			// only re-verify a completed task. Log loudly but the Linear
			// state is the user-visible source of truth.
			o.logger.Error("verify_complete_set_status_failed", "parent", parentID, "err", err)
		}
		o.logger.Info("verify_task_complete", "parent", parentID, "summary", v.Summary)

		// Cascade: this parent's verify just passed, so it is now the
		// "just-completed" child relative to ITS parent. The same helper
		// the discoverer uses for leaf merges walks one hop further up.
		// Top-of-tree (no parent) and incomplete-siblings cases short-
		// circuit inside the helper.
		cascade.MaybeEnqueueVerify(ctx, o.lc, o.db, o.logger, parentID, parentIdentifier)
		return
	}

	// Resolve the pickup gates once for the whole gap batch. A gate that is
	// CONFIGURED but cannot be RESOLVED is fatal for the loop: filing the
	// follow-ups anyway would create issues the discoverer can never re-ship
	// (wrong state / missing label), silently stalling self-convergence. So
	// escalate to a human instead of degrading. An UNconfigured gate (empty
	// label / no state types) is a valid setup and skipped, not an error.
	var pickStateID string
	if len(o.verifyStateTypes) > 0 {
		id, err := o.stateIDByType(ctx, teamID, o.verifyStateTypes[0])
		if err != nil || id == "" {
			o.logger.Error("verify_gap_state_unresolved",
				"parent", parentID, "type", o.verifyStateTypes[0], "id", id, "err", err)
			o.escalateVerifyWithReason(ctx, parentID, fmt.Sprintf(
				"admiral could not resolve a pickable Linear state (type %q) to file follow-up sub-issues — the verification loop cannot self-converge. Handing off for human review.",
				o.verifyStateTypes[0]))
			return
		}
		pickStateID = id
	}
	var labelIDs []string
	if o.verifyLabel != "" {
		id, err := o.lc.GetTeamLabelID(ctx, teamID, o.verifyLabel)
		if err != nil || id == "" {
			o.logger.Error("verify_gap_label_unresolved",
				"parent", parentID, "label", o.verifyLabel, "id", id, "err", err)
			o.escalateVerifyWithReason(ctx, parentID, fmt.Sprintf(
				"admiral could not resolve the pickup label %q to file follow-up sub-issues — the verification loop cannot self-converge. Handing off for human review.",
				o.verifyLabel))
			return
		}
		labelIDs = []string{id}
	}

	created := 0
	for _, g := range v.Gaps {
		issue, err := o.lc.IssueCreate(ctx, linear.IssueCreateInput{
			TeamID:      teamID,
			ProjectID:   projectID,
			Title:       g.Title,
			Description: gapBody(g),
			LabelIDs:    labelIDs,
			StateID:     pickStateID,
			ParentID:    parentID,
		})
		if err != nil {
			o.logger.Error("verify_gap_issue_create_failed",
				"parent", parentID, "title", g.Title, "err", err)
			continue
		}
		created++
		o.logger.Info("verify_gap_filed",
			"parent", parentID, "gap", issue.Identifier, "title", g.Title)
	}
	o.logger.Info("verify_gaps_done",
		"parent", parentID, "gaps", len(v.Gaps), "created", created, "summary", v.Summary)
}

// gapBody renders a follow-up sub-issue's description: the gap description
// followed by its acceptance criteria (the standard the fix's PR is later
// judged against), when present.
func gapBody(g verifyGap) string {
	body := strings.TrimSpace(g.Description)
	if ac := strings.TrimSpace(g.AcceptanceCriteria); ac != "" {
		if body != "" {
			body += "\n\n"
		}
		body += "## Acceptance Criteria\n" + ac
	}
	return body
}

// escalateVerify hands a non-converging task (round cap reached) off to a
// human.
func (o *Orchestrator) escalateVerify(ctx context.Context, parentID string, rounds, maxRounds int) {
	o.escalateVerifyWithReason(ctx, parentID, fmt.Sprintf(
		"admiral autonomous verification reached the round cap (%d) without converging — the task still has unmet requirements. Handing off for human review.",
		maxRounds))
	o.logger.Info("verify_escalated", "parent", parentID, "rounds", rounds, "cap", maxRounds)
}

// escalateVerifyWithReason posts a human-facing comment on the parent issue
// (it's human-created, not an agent session, so PostAgentActivity does not
// apply) and marks the verification 'escalated' so the loop stops. Used both
// for the round cap and for unrecoverable follow-up-filing failures.
func (o *Orchestrator) escalateVerifyWithReason(ctx context.Context, parentID, body string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := o.lc.CreateComment(ctx, parentID, body); err != nil {
		o.logger.Error("verify_escalate_comment_failed", "parent", parentID, "err", err)
	}
	if err := o.db.SetTaskVerificationStatus(parentID, store.TaskVerifyEscalated); err != nil {
		o.logger.Error("verify_escalate_set_status_failed", "parent", parentID, "err", err)
	}
}

// runClaudeForVerify runs the headless verify judge: `claude -p <prompt>
// --dangerously-skip-permissions` with cmd.Dir set to the repo (base branch,
// no worktree, no push — the agent only reads and judges) and captures stdout
// as the verdict text. Mirrors runClaudeForReview's process handling; kept
// separate because verify neither prepares a worktree nor pushes commits.
func runClaudeForVerify(ctx context.Context, claudeBin string, maxRunSeconds int, repoDir, prompt string, logger *slog.Logger) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(maxRunSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, claudeBin,
		"-p", prompt,
		"--dangerously-skip-permissions",
	)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = repoDir
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("claude stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude start: %w", err)
	}

	var sb strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteByte('\n')
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			logger.Warn("claude_verify_stderr", "line", sc.Text())
		}
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if cctx.Err() != nil {
			return "", fmt.Errorf("claude exit: %w: %w", err, context.DeadlineExceeded)
		}
		return "", fmt.Errorf("claude exit: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}
