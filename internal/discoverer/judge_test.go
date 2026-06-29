package discoverer

import (
	"context"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/linear"
)

func TestParseVerdictPlainJSON(t *testing.T) {
	v, err := parseVerdict(`{"verdict":"yes","reason":"clear task"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "yes" || v.Reason != "clear task" {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdictWrappedInProse(t *testing.T) {
	// claude -p sometimes prefixes with a short acknowledgement.
	raw := "Sure, here is the verdict:\n{\"verdict\":\"no\",\"reason\":\"too vague\"}\n"
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "no" || !strings.Contains(v.Reason, "vague") {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdictMarkdownCodeFence(t *testing.T) {
	raw := "```json\n{\"verdict\":\"yes\",\"reason\":\"bounded\"}\n```"
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "yes" || !strings.Contains(v.Reason, "bounded") {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdictMissingJSON(t *testing.T) {
	_, err := parseVerdict("I think yes.")
	if err == nil {
		t.Fatal("expected error when no JSON present")
	}
}

func TestParseVerdictWrongFormat(t *testing.T) {
	_, err := parseVerdict(`{"verdict":}`)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestBuildJudgePromptIncludesAllFields(t *testing.T) {
	iss := linear.Issue{
		Identifier:  "GEO-77",
		Title:       "Fix flaky retry",
		Description: "The retry loop drops the err",
		Labels:      []string{"bug", "agent-ready"},
	}
	p := buildJudgePrompt(iss)
	for _, want := range []string{"GEO-77", "Fix flaky retry", "bug, agent-ready", "The retry loop"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, p)
		}
	}
}

func TestBuildJudgePromptEmptyFieldsFallback(t *testing.T) {
	iss := linear.Issue{Identifier: "GEO-78", Title: "x"}
	p := buildJudgePrompt(iss)
	if !strings.Contains(p, "(none)") || !strings.Contains(p, "(no description)") {
		t.Errorf("expected fallback placeholders, got:\n%s", p)
	}
}

func TestClaudeJudgeRejectsEmptyBin(t *testing.T) {
	j := &claudeJudge{}
	_, err := j.Judge(context.TODO(), linear.Issue{ID: "x"})
	if err == nil {
		t.Fatal("expected error on empty claude_bin")
	}
}
