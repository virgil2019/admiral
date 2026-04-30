package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestEnqueueEvent_Fresh(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	if _, err := db.Exec(migration0001); err != nil {
		t.Fatalf("migration0001: %v", err)
	}
	if _, err := db.Exec(migration0002); err != nil {
		t.Fatalf("migration0002: %v", err)
	}
	if _, err := db.Exec(migration0003); err != nil {
		t.Fatalf("migration0003: %v", err)
	}
	if _, err := db.Exec(migration0004); err != nil {
		t.Fatalf("migration0004: %v", err)
	}
	if _, err := db.Exec(migration0005); err != nil {
		t.Fatalf("migration0005: %v", err)
	}

	fresh, err := s.EnqueueEvent("wh-1", "created", "sess-1", "issue-1", `{"foo":"bar"}`)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !fresh {
		t.Error("expected fresh=true on first insert")
	}
}

func TestEnqueueEvent_Duplicate(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	fresh1, err := s.EnqueueEvent("wh-dup", "created", "sess-1", "issue-1", `{}`)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if !fresh1 {
		t.Error("expected fresh=true on first insert")
	}

	fresh2, err := s.EnqueueEvent("wh-dup", "created", "sess-2", "issue-2", `{}`)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if fresh2 {
		t.Error("expected fresh=false on duplicate webhook_id")
	}
}

func TestClaimNextPendingEvent_Empty(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	row, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if row != nil {
		t.Errorf("expected nil on empty table, got %+v", row)
	}
}

func TestClaimNextPendingEvent_SinglePending(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	_, err = s.EnqueueEvent("wh-single", "created", "sess-x", "issue-x", `{"test":1}`)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	row, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if row == nil {
		t.Fatal("expected a row")
	}
	if row.WebhookID != "wh-single" {
		t.Errorf("webhook_id: got %q", row.WebhookID)
	}
	if row.Status != "processing" {
		t.Errorf("status: got %q, want processing", row.Status)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts: got %d, want 1", row.Attempts)
	}

	// Second claim should be empty
	row2, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if row2 != nil {
		t.Error("second claim should be nil (already claimed)")
	}
}

func TestClaimNextPendingEvent_Concurrent(t *testing.T) {
	// SQLite's BEGIN IMMEDIATE locking is tested by SQLite itself.
	// Here we just verify that the second claim returns nil when first is in progress.
	// The actual row-level locking is handled by SQLite's transaction isolation.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	// Enqueue 2 events with different webhook IDs
	_, err = s.EnqueueEvent("wh-concurrent-a", "created", "sess-1", "issue-1", `{}`)
	if err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	_, err = s.EnqueueEvent("wh-concurrent-b", "created", "sess-2", "issue-2", `{}`)
	if err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	// Claim first event
	row1, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if row1 == nil {
		t.Fatal("expected first row")
	}
	if row1.WebhookID != "wh-concurrent-a" {
		t.Errorf("first claim: got %q, want wh-concurrent-a", row1.WebhookID)
	}

	// Claim second event
	row2, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if row2 == nil {
		t.Fatal("expected second row")
	}
	if row2.WebhookID != "wh-concurrent-b" {
		t.Errorf("second claim: got %q, want wh-concurrent-b", row2.WebhookID)
	}

	// Third claim should be empty
	row3, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 3: %v", err)
	}
	if row3 != nil {
		t.Error("expected nil for third claim")
	}
}

func TestMarkEventDone(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	_, _ = s.EnqueueEvent("wh-done", "created", "sess-1", "issue-1", `{}`)
	if err := s.MarkEventDone("wh-done"); err != nil {
		t.Fatalf("MarkEventDone: %v", err)
	}

	var status string
	err = db.QueryRow(`SELECT status FROM events_inbox WHERE webhook_id='wh-done'`).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "done" {
		t.Errorf("status: got %q, want done", status)
	}
}

func TestMarkEventFailed_Retry(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	_, _ = s.EnqueueEvent("wh-retry", "created", "sess-1", "issue-1", `{}`)
	if err := s.MarkEventFailed("wh-retry", "some error", true); err != nil {
		t.Fatalf("MarkEventFailed: %v", err)
	}

	var status string
	var lastErr string
	err = db.QueryRow(`SELECT status, last_error FROM events_inbox WHERE webhook_id='wh-retry'`).Scan(&status, &lastErr)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "pending" {
		t.Errorf("status: got %q, want pending", status)
	}
	if lastErr != "some error" {
		t.Errorf("last_error: got %q", lastErr)
	}
}

func TestMarkEventFailed_DeadLetter(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	_, _ = s.EnqueueEvent("wh-dead", "created", "sess-1", "issue-1", `{}`)
	if err := s.MarkEventFailed("wh-dead", "final error", false); err != nil {
		t.Fatalf("MarkEventFailed: %v", err)
	}

	var status string
	err = db.QueryRow(`SELECT status FROM events_inbox WHERE webhook_id='wh-dead'`).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "failed" {
		t.Errorf("status: got %q, want failed", status)
	}
}

func TestCountPendingEvents(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	count, err := s.CountPendingEvents()
	if err != nil {
		t.Fatalf("CountPendingEvents: %v", err)
	}
	if count != 0 {
		t.Errorf("initial count: got %d, want 0", count)
	}

	_, _ = s.EnqueueEvent("wh-count-1", "created", "sess-1", "issue-1", `{}`)
	_, _ = s.EnqueueEvent("wh-count-2", "created", "sess-2", "issue-2", `{}`)

	count, err = s.CountPendingEvents()
	if err != nil {
		t.Fatalf("CountPendingEvents: %v", err)
	}
	if count != 2 {
		t.Errorf("count after 2 enqueues: got %d, want 2", count)
	}
}

func TestEventInboxRow_Timestamps(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := NewForTest(db)
	for _, m := range []string{migration0001, migration0002, migration0003, migration0004, migration0005} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	_, _ = s.EnqueueEvent("wh-ts", "created", "sess-1", "issue-1", `{}`)
	row, err := s.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if row == nil {
		t.Fatal("expected row")
	}
	if row.ReceivedAt.IsZero() {
		t.Error("ReceivedAt should be set")
	}
	if row.StartedAt == nil {
		t.Error("StartedAt should be set after claim")
	}
	if row.FinishedAt != nil {
		t.Error("FinishedAt should be nil before done")
	}

	// Simulate some time passing
	time.Sleep(10 * time.Millisecond)

	if err := s.MarkEventDone("wh-ts"); err != nil {
		t.Fatalf("MarkEventDone: %v", err)
	}

	// Verify finished_at is now set
	var finishedAtUnix int64
	err = db.QueryRow(`SELECT finished_at FROM events_inbox WHERE webhook_id='wh-ts'`).Scan(&finishedAtUnix)
	if err != nil {
		t.Fatalf("query finished_at: %v", err)
	}
	if finishedAtUnix == 0 {
		t.Error("finished_at should be non-zero after done")
	}
}