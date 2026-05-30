package store

import (
	"errors"
	"testing"
)

func TestGetTaskVerification_NoneYet(t *testing.T) {
	s := newTestStore(t)
	tv, err := s.GetTaskVerification("parent-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tv != nil {
		t.Fatalf("expected nil for unrecorded parent, got %+v", tv)
	}
}

func TestBumpTaskVerificationRound_InsertThenIncrement(t *testing.T) {
	s := newTestStore(t)

	tv, err := s.BumpTaskVerificationRound("parent-1")
	if err != nil {
		t.Fatalf("bump 1: %v", err)
	}
	if tv.Rounds != 1 || tv.Status != TaskVerifyActive {
		t.Fatalf("after first bump: %+v", tv)
	}
	if tv.UpdatedAt == "" {
		t.Fatal("UpdatedAt should be set")
	}

	tv, err = s.BumpTaskVerificationRound("parent-1")
	if err != nil {
		t.Fatalf("bump 2: %v", err)
	}
	if tv.Rounds != 2 {
		t.Fatalf("expected rounds=2, got %d", tv.Rounds)
	}
}

func TestBumpTaskVerificationRound_PreservesStatus(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.BumpTaskVerificationRound("parent-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := s.SetTaskVerificationStatus("parent-1", TaskVerifyEscalated); err != nil {
		t.Fatalf("set status: %v", err)
	}
	// A later bump must not silently reset an escalated task back to active.
	tv, err := s.BumpTaskVerificationRound("parent-1")
	if err != nil {
		t.Fatalf("bump after escalate: %v", err)
	}
	if tv.Status != TaskVerifyEscalated {
		t.Fatalf("status should be preserved across bump, got %q", tv.Status)
	}
	if tv.Rounds != 2 {
		t.Fatalf("expected rounds=2, got %d", tv.Rounds)
	}
}

func TestSetTaskVerificationStatus_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.BumpTaskVerificationRound("parent-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := s.SetTaskVerificationStatus("parent-1", TaskVerifyClosed); err != nil {
		t.Fatalf("set: %v", err)
	}
	tv, err := s.GetTaskVerification("parent-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tv.Status != TaskVerifyClosed {
		t.Fatalf("expected closed, got %q", tv.Status)
	}
}

func TestSetTaskVerificationStatus_NotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetTaskVerificationStatus("ghost", TaskVerifyClosed); err == nil {
		t.Error("expected error setting status on a non-existent row")
	}
}

func TestSetTaskVerificationStatus_RejectsBadValue(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.BumpTaskVerificationRound("parent-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	// Out-of-allowlist status is rejected with a typed error before it ever
	// reaches the DB CHECK constraint.
	err := s.SetTaskVerificationStatus("parent-1", "bogus")
	if !errors.Is(err, ErrInvalidTaskStatus) {
		t.Errorf("expected ErrInvalidTaskStatus, got %v", err)
	}
}

func TestBumpTaskVerificationRound_DoesNotResurrectClosed(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.BumpTaskVerificationRound("parent-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := s.SetTaskVerificationStatus("parent-1", TaskVerifyClosed); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Documented contract: Bump increments but does NOT reactivate a terminal
	// task — gating is the caller's job.
	tv, err := s.BumpTaskVerificationRound("parent-1")
	if err != nil {
		t.Fatalf("bump after close: %v", err)
	}
	if tv.Status != TaskVerifyClosed || tv.Rounds != 2 {
		t.Fatalf("bump after close should keep status closed + increment, got %+v", tv)
	}
}
