package omx

import "testing"

func TestParseEnvelope_OK(t *testing.T) {
	stdout := []byte(`{"ok":true,"operation":"get-summary","data":{"team_name":"vibe","workers":[]}}`)
	env, err := ParseEnvelope(stdout, nil, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !env.OK {
		t.Fatal("expected ok=true")
	}
	if env.Operation != "get-summary" {
		t.Fatalf("wrong op: %s", env.Operation)
	}
	if len(env.Data) == 0 {
		t.Fatal("data missing")
	}
}

func TestParseEnvelope_Error(t *testing.T) {
	stdout := []byte(`{"ok":false,"operation":"send-message","error":{"code":"team_not_found","message":"no such team"}}`)
	env, err := ParseEnvelope(stdout, nil, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if env.OK {
		t.Fatal("expected ok=false")
	}
	if env.Error == nil || env.Error.Code != "team_not_found" {
		t.Fatalf("wrong error: %+v", env.Error)
	}
}

func TestParseEnvelope_EmptyStdout(t *testing.T) {
	_, err := ParseEnvelope(nil, []byte("boom"), nil)
	if err == nil {
		t.Fatal("expected error on empty stdout")
	}
}

func TestParseEnvelope_BadJSON(t *testing.T) {
	_, err := ParseEnvelope([]byte("not-json"), nil, nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLaunchSpec_OmxCommandArgs(t *testing.T) {
	s := LaunchSpec{WorkerCount: 3, AgentType: "codex", Task: "bridge session"}
	got := s.OmxCommandArgs()
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

func TestLaunchSpec_OmxCommandArgs_Claude(t *testing.T) {
	s := LaunchSpec{WorkerCount: 5, AgentType: "claude", Task: "  spaced task  "}
	got := s.OmxCommandArgs()
	want := []string{"team", "5:claude", "  spaced task  "}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}
