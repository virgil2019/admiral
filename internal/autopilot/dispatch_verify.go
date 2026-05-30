package autopilot

import (
	"encoding/json"
	"fmt"
	"strings"
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
// actually shipped for each sub-issue.
type verifyMaterial struct {
	ParentIdentifier string
	PRD              string
	Subs             []verifySubMaterial
}

type verifySubMaterial struct {
	Identifier string
	Title      string
	PRURL      string
	Diff       string
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
