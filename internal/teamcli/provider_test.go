package teamcli

import "testing"

func TestLaunchSpec_CommandArgs(t *testing.T) {
	s := LaunchSpec{WorkerCount: 3, AgentType: "codex", Task: "bridge session"}
	got := s.CommandArgs()
	want := []string{"team", "3:codex", "bridge session"}
	if len(got) != len(want) {
		t.Fatalf("argv len: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestLaunchSpec_CommandArgs_Claude(t *testing.T) {
	s := LaunchSpec{WorkerCount: 5, AgentType: "claude", Task: "  spaced task  "}
	got := s.CommandArgs()
	want := []string{"team", "5:claude", "  spaced task  "}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}
