package bridge

import "testing"

func TestWhitelist_Allowed(t *testing.T) {
	w := NewWhitelist([]int64{1, 2, 3}, 42)
	if got := w.Check(2, 42); got != GateAllowed {
		t.Fatalf("expected allowed, got %v", got)
	}
}

func TestWhitelist_UnknownUser(t *testing.T) {
	w := NewWhitelist([]int64{1}, 42)
	if got := w.Check(999, 42); got != GateRejectUser {
		t.Fatalf("expected reject-user, got %v", got)
	}
}

func TestWhitelist_WrongChat(t *testing.T) {
	w := NewWhitelist([]int64{1}, 42)
	if got := w.Check(1, 77); got != GateRejectChat {
		t.Fatalf("expected reject-chat, got %v", got)
	}
}

func TestWhitelist_EmptyIDs(t *testing.T) {
	w := NewWhitelist(nil, 42)
	if got := w.Check(1, 42); got != GateRejectUser {
		t.Fatalf("expected reject-user for empty whitelist, got %v", got)
	}
}
