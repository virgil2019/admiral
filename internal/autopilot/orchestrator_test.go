package autopilot

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
)

func TestSanitizeForPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"TST-1", "tst-1"},
		{"  weird @id! ", "weird-id"},
		{"../../etc/passwd", "etc-passwd"},
		{"linear/abc", "linear-abc"},
		{"---abc---", "abc"},
	}
	for _, c := range cases {
		if got := sanitizeForPath(c.in); got != c.want {
			t.Errorf("sanitizeForPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBranchName(t *testing.T) {
	cases := []struct {
		in   linear.Issue
		want string
	}{
		{linear.Issue{Identifier: "TST-12"}, "linear/tst-12"},
		{linear.Issue{Identifier: "", ID: "abc-123"}, "linear/abc-123"},
	}
	for _, c := range cases {
		if got := branchName(&c.in); got != c.want {
			t.Errorf("branchName(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildPrompt_AssignNoContext(t *testing.T) {
	p := buildPrompt("", &linear.Issue{
		Identifier:  "TST-1",
		Title:       "do the thing",
		Description: "details",
		StateName:   "Todo",
		Labels:      []string{"backend"},
	}, linear.AgentEvent{Action: linear.ActionCreated}, "linear/tst-1", "main")
	if !strings.Contains(p, "TST-1: do the thing") {
		t.Errorf("missing title line:\n%s", p)
	}
	if !strings.Contains(p, "State: Todo") {
		t.Errorf("missing state:\n%s", p)
	}
	if !strings.Contains(p, "Labels: backend") {
		t.Errorf("missing labels:\n%s", p)
	}
	if !strings.Contains(p, "(assigned, no explicit prompt") {
		t.Errorf("missing assign placeholder:\n%s", p)
	}
	if !strings.Contains(p, "git push -u origin \"linear/tst-1\"") &&
		!strings.Contains(p, "git push -u origin linear/tst-1") {
		t.Errorf("missing branch in operating procedure:\n%s", p)
	}
	if !strings.Contains(p, "--base \"main\"") &&
		!strings.Contains(p, "--base main") {
		t.Errorf("missing base branch in operating procedure:\n%s", p)
	}
}

func TestBuildPrompt_MentionWithContext(t *testing.T) {
	p := buildPrompt("autopilot", &linear.Issue{
		Identifier: "TST-2", Title: "x", Description: "d",
	}, linear.AgentEvent{
		Action:        linear.ActionCreated,
		PromptContext: "please refactor the auth module",
	}, "linear/tst-2", "main")
	if !strings.HasPrefix(p, "/autopilot\n\n") {
		t.Errorf("skill not prefixed:\n%s", p)
	}
	if !strings.Contains(p, "please refactor the auth module") {
		t.Errorf("missing prompt context:\n%s", p)
	}
}

func TestBuildPrompt_Comments(t *testing.T) {
	p := buildPrompt("", &linear.Issue{
		Identifier: "TST-3", Title: "x",
		Comments: []linear.Comment{
			{UserName: "alice", Body: "first comment"},
			{UserName: "bob", Body: "second comment"},
		},
	}, linear.AgentEvent{Action: linear.ActionCreated}, "linear/tst-3", "main")
	if !strings.Contains(p, "- alice: first comment") {
		t.Errorf("missing alice comment:\n%s", p)
	}
	if !strings.Contains(p, "- bob: second comment") {
		t.Errorf("missing bob comment:\n%s", p)
	}
}

func TestExtractFirstURL(t *testing.T) {
	in := "Some preamble\nhttps://github.com/x/y/pull/42\nfooter"
	if got := extractFirstURL(in); got != "https://github.com/x/y/pull/42" {
		t.Errorf("got %q", got)
	}
}

func TestOpenStreamFile(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "test-session-abc"
	expectedPath := filepath.Join(tmp, sessionID+".jsonl")

	// Simulate flow with a mock orchestrator and open stream file.
	o := &Orchestrator{cfg: &config.Autopilot{JobStreamsDir: tmp}, logger: slog.Default()}
	f := &flow{o: o, ev: linear.AgentEvent{SessionID: sessionID}}

	if err := f.openStreamFile(); err != nil {
		t.Fatalf("openStreamFile failed: %v", err)
	}
	defer f.closeStreamFile()

	if f.streamFile == nil {
		t.Fatal("streamFile is nil after openStreamFile")
	}
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("stream file not created at expected path: %v", err)
	}
}

func TestDrainStreamJSON_WritesRawLines(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "test-session-def"
	streamPath := filepath.Join(tmp, sessionID+".jsonl")

	o := &Orchestrator{cfg: &config.Autopilot{JobStreamsDir: tmp}, logger: slog.Default()}
	f := &flow{o: o, ev: linear.AgentEvent{SessionID: sessionID}}

	if err := f.openStreamFile(); err != nil {
		t.Fatalf("openStreamFile failed: %v", err)
	}
	defer f.closeStreamFile()

	// Simulate N lines of stream-json output.
	lines := []string{
		`{"type":"system_init","subtype":"version"}`,
		`{"type":"tool_use","name":"Bash","input":{}}`,
		`{"type":"result","content":"ok"}`,
	}
	r := strings.NewReader(strings.Join(lines, "\n")+"\n")
	f.drainStreamJSON(r)

	data, err := os.ReadFile(streamPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	readLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(readLines) != len(lines) {
		t.Fatalf("expected %d lines, got %d", len(lines), len(readLines))
	}
	for i := range lines {
		if readLines[i] != lines[i] {
			t.Errorf("line %d: got %q, want %q", i, readLines[i], lines[i])
		}
	}
}
