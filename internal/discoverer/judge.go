package discoverer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

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

func (j *claudeJudge) Judge(ctx context.Context, iss linear.Issue) (Verdict, error) {
	if strings.TrimSpace(j.claudeBin) == "" {
		return Verdict{}, fmt.Errorf("claude_bin not set")
	}
	if j.timeout <= 0 {
		return Verdict{}, fmt.Errorf("judge timeout not set")
	}
	out, err := j.runClaude(ctx, buildJudgePrompt(iss))
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

func (j *claudeJudge) runClaude(ctx context.Context, prompt string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, j.claudeBin,
		"-p", prompt,
		"--dangerously-skip-permissions",
	)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}

	var sb strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteByte('\n')
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			j.logger.Warn("judge_stderr", "line", sc.Text())
		}
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if cctx.Err() != nil {
			return "", fmt.Errorf("claude exit %w (timeout)", err)
		}
		return "", fmt.Errorf("claude exit: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
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
