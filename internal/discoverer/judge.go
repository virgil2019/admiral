package discoverer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/claudeclient"
	"github.com/georgehuang/admiral/internal/linear"
)

// Verdict is the parsed output of the judge step.
type Verdict struct {
	Decision string `json:"verdict"`
	Reason   string `json:"reason"`
}

// claudeJudge implements the judger interface by spawning `claude -p`
// and parsing a JSON verdict from stdout.
type claudeJudge struct {
	claudeBin string
	timeout   time.Duration
	logger    *slog.Logger
}

// timeoutSeconds returns j.timeout in whole seconds for passing to
// claudeclient.Run (which takes int seconds, not a time.Duration).
// Returns the 30s fallback when j.timeout is unset / non-positive so a
// caller that forgot to wire the config still gets a usable default.
func (j *claudeJudge) timeoutSeconds() int {
	if j.timeout <= 0 {
		return 30
	}
	return int(j.timeout / time.Second)
}

func (j *claudeJudge) Judge(ctx context.Context, iss linear.Issue) (Verdict, error) {
	if strings.TrimSpace(j.claudeBin) == "" {
		return Verdict{}, fmt.Errorf("claude_bin not set")
	}
	if j.timeout <= 0 {
		return Verdict{}, fmt.Errorf("judge timeout not set")
	}
	out, err := claudeclient.Run(ctx, j.claudeBin, j.timeoutSeconds(),
		"", nil, buildJudgePrompt(iss),
		[]string{"--output-format", "text", "--dangerously-skip-permissions"},
		"", j.logger)
	if err != nil {
		return Verdict{}, err
	}
	v, err := parseVerdict(out)
	if err != nil {
		return Verdict{}, fmt.Errorf("parse verdict (raw=%q): %w", truncateOutput(out, 200), err)
	}
	if v.Decision != "yes" && v.Decision != "no" {
		return Verdict{}, fmt.Errorf("unexpected verdict %q", v.Decision)
	}
	return v, nil
}

const judgePromptTemplate = `You are evaluating whether an AI coding agent can autonomously complete a Linear issue without human guidance.

Issue identifier: %s
Title: %s
Labels: %s
Description:
%s

Decide:
- "yes" — the issue describes a clear, bounded, code-only task with enough detail to act on
- "no" — the issue requires product judgment, design, external coordination, or is ambiguous

Output ONLY one line of strict JSON in this exact shape, with no prose, no markdown, no code fences:
{"verdict":"yes","reason":"<one short sentence>"}
or
{"verdict":"no","reason":"<one short sentence>"}`

func buildJudgePrompt(iss linear.Issue) string {
	labels := strings.Join(iss.Labels, ", ")
	if labels == "" {
		labels = "(none)"
	}
	desc := strings.TrimSpace(iss.Description)
	if desc == "" {
		desc = "(no description)"
	}
	return fmt.Sprintf(judgePromptTemplate, iss.Identifier, iss.Title, labels, desc)
}

// verdictJSONRe matches the first JSON object on a line that contains
// the "verdict" key. Greedy on a single line; tolerant of leading prose
// or trailing whitespace.
var verdictJSONRe = regexp.MustCompile(`\{[^{}\n]*"verdict"[^{}\n]*\}`)

func parseVerdict(s string) (Verdict, error) {
	s = strings.TrimSpace(s)
	var v Verdict
	if err := json.Unmarshal([]byte(s), &v); err == nil && v.Decision != "" {
		return v, nil
	}
	m := verdictJSONRe.FindString(s)
	if m == "" {
		return Verdict{}, fmt.Errorf("no JSON object containing verdict found")
	}
	if err := json.Unmarshal([]byte(m), &v); err != nil {
		return Verdict{}, fmt.Errorf("unmarshal: %w", err)
	}
	return v, nil
}

func truncateOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
