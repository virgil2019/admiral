package autopilot

import (
	"strings"
	"testing"
)

func TestParseVerifyVerdict_CompleteCleanJSON(t *testing.T) {
	raw := `{"complete": true, "summary": "all good", "gaps": []}`
	v, err := parseVerifyVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Complete || len(v.Gaps) != 0 || v.Summary != "all good" {
		t.Fatalf("unexpected verdict: %+v", v)
	}
}

func TestParseVerifyVerdict_GapsWithProseAndFences(t *testing.T) {
	// Models often wrap the object in prose / ```json fences.
	raw := "Here is my verdict:\n```json\n" +
		`{"complete": false, "summary": "missing logout", "gaps": [` +
		`{"title": "Add logout", "description": "no logout endpoint", "acceptance_criteria": "DELETE /session clears cookie"}]}` +
		"\n```\nHope that helps!"
	v, err := parseVerifyVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Complete || len(v.Gaps) != 1 {
		t.Fatalf("unexpected verdict: %+v", v)
	}
	if v.Gaps[0].Title != "Add logout" || v.Gaps[0].AcceptanceCriteria == "" {
		t.Fatalf("gap not parsed: %+v", v.Gaps[0])
	}
}

func TestParseVerifyVerdict_NoJSON(t *testing.T) {
	if _, err := parseVerifyVerdict("I could not decide."); err == nil {
		t.Error("expected error when no JSON object present")
	}
}

func TestParseVerifyVerdict_Malformed(t *testing.T) {
	if _, err := parseVerifyVerdict(`{"complete": tru`); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

func TestParseVerifyVerdict_InconsistentCompleteWithGaps(t *testing.T) {
	raw := `{"complete": true, "gaps": [{"title": "x"}]}`
	if _, err := parseVerifyVerdict(raw); err == nil {
		t.Error("expected error: complete=true but gaps present")
	}
}

func TestParseVerifyVerdict_InconsistentIncompleteNoGaps(t *testing.T) {
	raw := `{"complete": false, "gaps": []}`
	if _, err := parseVerifyVerdict(raw); err == nil {
		t.Error("expected error: complete=false but no gaps")
	}
}

func TestParseVerifyVerdict_GapMissingTitle(t *testing.T) {
	raw := `{"complete": false, "gaps": [{"title": "  ", "description": "d"}]}`
	if _, err := parseVerifyVerdict(raw); err == nil {
		t.Error("expected error when a gap has an empty title")
	}
}

func TestBuildVerifyPrompt_IncludesPRDAndDiffs(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		ParentIdentifier: "GEO-10",
		PRD:              "Build a login feature with email + password.",
		Subs: []verifySubMaterial{
			{Identifier: "GEO-11", Title: "email form", PRURL: "https://gh/pr/1", Diff: "+ <input email>"},
			{Identifier: "GEO-12", Title: "session", Diff: ""},
		},
	})
	for _, want := range []string{
		"Build a login feature", "GEO-11: email form", "https://gh/pr/1",
		"+ <input email>", "GEO-12: session", "(diff unavailable)", `"complete"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildVerifyPrompt_NoSubs(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{PRD: "do things"})
	if !strings.Contains(p, "no sub-task diffs available") {
		t.Error("expected a marker when there are no subs")
	}
}
