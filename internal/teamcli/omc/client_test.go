package omc

import (
	"context"
	"errors"
	"testing"

	"github.com/georgehuang/admiral/internal/teamcli"
)

func TestCaps_AllFalse(t *testing.T) {
	c := New("/bin/true", "/tmp", "t")
	caps := c.Caps()
	if caps.SupportsAwaitEvent || caps.SupportsIdleState || caps.SupportsStallState {
		t.Fatalf("omc should not support await/idle/stall, got %+v", caps)
	}
}

func TestUnsupported_AwaitEvent(t *testing.T) {
	c := New("/bin/true", "/tmp", "t")
	_, err := c.AwaitEvent(context.Background(), "", 1000)
	if !errors.Is(err, teamcli.ErrUnsupported) {
		t.Fatalf("AwaitEvent: want ErrUnsupported, got %v", err)
	}
	if _, err := c.ReadIdleState(context.Background()); !errors.Is(err, teamcli.ErrUnsupported) {
		t.Fatalf("ReadIdleState: want ErrUnsupported, got %v", err)
	}
	if _, err := c.ReadStallState(context.Background()); !errors.Is(err, teamcli.ErrUnsupported) {
		t.Fatalf("ReadStallState: want ErrUnsupported, got %v", err)
	}
}

func TestParseEnvelope_OK(t *testing.T) {
	stdout := []byte(`{"ok":true,"operation":"get-summary","data":{}}`)
	env, err := ParseEnvelope(stdout, nil, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !env.OK || env.Operation != "get-summary" {
		t.Fatalf("bad parse: %+v", env)
	}
}

func TestFilterNoise_DropsCanonicalizedLine(t *testing.T) {
	in := []byte("[team] canonicalized duplicate worker entries: claude-1\nreal error here\n[team] canonicalized duplicate worker entries: codex-2")
	out := filterNoise(in)
	want := "real error here"
	if string(out) != want {
		t.Fatalf("filterNoise:\n got=%q\nwant=%q", out, want)
	}
}

func TestFilterNoise_AllNoise(t *testing.T) {
	in := []byte("[team] canonicalized duplicate worker entries: a\n[team] canonicalized duplicate worker entries: b\n")
	out := filterNoise(in)
	if len(out) != 0 {
		t.Fatalf("filterNoise: want empty, got %q", out)
	}
}

func TestFilterNoise_Empty(t *testing.T) {
	if got := filterNoise(nil); len(got) != 0 {
		t.Fatalf("filterNoise(nil): want empty, got %q", got)
	}
}

func TestLaunchSpec_ArgvSharedShape(t *testing.T) {
	// Sanity: omc and omx share the same argv shape via teamcli.LaunchSpec.
	s := teamcli.LaunchSpec{WorkerCount: 2, AgentType: "claude", Task: "vibe omc test"}
	got := s.CommandArgs()
	want := []string{"team", "2:claude", "vibe omc test"}
	if len(got) != len(want) {
		t.Fatalf("argv len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}
