package discoverer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
)

// fakeViewer is a ViewerLookup that fails the first failCount calls then
// returns viewer. Tracks total call count for assertions.
type fakeViewer struct {
	failCount int
	err       error
	viewer    linear.Viewer
	calls     int
}

func (f *fakeViewer) GetViewer(_ context.Context) (*linear.Viewer, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, f.err
	}
	v := f.viewer
	return &v, nil
}

// memKV is an in-memory KVStore for tests.
type memKV struct {
	m     map[string]string
	reads int
	wrote string
}

func newMemKV() *memKV { return &memKV{m: map[string]string{}} }

func (k *memKV) KVGet(key string) (string, error) {
	k.reads++
	return k.m[key], nil
}

func (k *memKV) KVSet(key, value string) error {
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[key] = value
	k.wrote = value
	return nil
}

// shrinkBackoffs replaces resolveBackoffs for the duration of the test so
// retries don't add wall-clock seconds.
func shrinkBackoffs(t *testing.T) {
	t.Helper()
	prev := resolveBackoffs
	resolveBackoffs = []time.Duration{0, 1 * time.Millisecond, 1 * time.Millisecond}
	t.Cleanup(func() { resolveBackoffs = prev })
}

func TestResolveAdmiralUserID_ConfigWins(t *testing.T) {
	shrinkBackoffs(t)
	v := &fakeViewer{viewer: linear.Viewer{ID: "u-from-api"}}
	kv := newMemKV()
	kv.m[AdmiralUserIDKVKey] = "u-from-cache"
	id, err := ResolveAdmiralUserID(context.Background(), "u-config", v, kv, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "u-config" {
		t.Errorf("id = %q, want u-config (config overrides both cache and API)", id)
	}
	if v.calls != 0 {
		t.Errorf("config-explicit must skip API; got %d calls", v.calls)
	}
	if kv.reads != 0 {
		t.Errorf("config-explicit must skip cache read; got %d reads", kv.reads)
	}
}

func TestResolveAdmiralUserID_DBCacheHit(t *testing.T) {
	shrinkBackoffs(t)
	v := &fakeViewer{viewer: linear.Viewer{ID: "u-from-api"}}
	kv := newMemKV()
	kv.m[AdmiralUserIDKVKey] = "u-cached"
	id, err := ResolveAdmiralUserID(context.Background(), "", v, kv, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "u-cached" {
		t.Errorf("id = %q, want u-cached", id)
	}
	if v.calls != 0 {
		t.Errorf("cache hit must skip API; got %d calls", v.calls)
	}
}

func TestResolveAdmiralUserID_APIFirstSuccessCachesToDB(t *testing.T) {
	shrinkBackoffs(t)
	v := &fakeViewer{viewer: linear.Viewer{ID: "u-fresh", Name: "admiral"}}
	kv := newMemKV()
	id, err := ResolveAdmiralUserID(context.Background(), "", v, kv, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "u-fresh" {
		t.Errorf("id = %q, want u-fresh", id)
	}
	if v.calls != 1 {
		t.Errorf("expected 1 API call, got %d", v.calls)
	}
	if kv.m[AdmiralUserIDKVKey] != "u-fresh" {
		t.Errorf("KV not populated; got %q, want u-fresh", kv.m[AdmiralUserIDKVKey])
	}
}

func TestResolveAdmiralUserID_APIRecoverAfter2Fails(t *testing.T) {
	shrinkBackoffs(t)
	v := &fakeViewer{
		failCount: 2,
		err:       errors.New("TLS handshake timeout"),
		viewer:    linear.Viewer{ID: "u-recovered"},
	}
	kv := newMemKV()
	id, err := ResolveAdmiralUserID(context.Background(), "", v, kv, nil)
	if err != nil {
		t.Fatalf("err: %v (should have recovered on third attempt)", err)
	}
	if id != "u-recovered" {
		t.Errorf("id = %q, want u-recovered", id)
	}
	if v.calls != 3 {
		t.Errorf("expected 3 API calls (2 fails + 1 success); got %d", v.calls)
	}
	if kv.m[AdmiralUserIDKVKey] != "u-recovered" {
		t.Errorf("recovery should still cache; KV has %q", kv.m[AdmiralUserIDKVKey])
	}
}

func TestResolveAdmiralUserID_APIExhausted(t *testing.T) {
	shrinkBackoffs(t)
	apiErr := errors.New("TLS handshake timeout")
	v := &fakeViewer{
		failCount: 99, // larger than maxAttempts
		err:       apiErr,
	}
	kv := newMemKV()
	id, err := ResolveAdmiralUserID(context.Background(), "", v, kv, nil)
	if err == nil {
		t.Fatalf("expected error after exhausting retries; got id=%q", id)
	}
	if !errors.Is(err, apiErr) {
		t.Errorf("err should wrap the underlying API error; got %v", err)
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("err message should mention attempt count; got %q", err.Error())
	}
	if v.calls != len(resolveBackoffs) {
		t.Errorf("expected %d API calls, got %d", len(resolveBackoffs), v.calls)
	}
	if _, ok := kv.m[AdmiralUserIDKVKey]; ok {
		t.Errorf("exhausted lookup must NOT write to cache; KV has %q", kv.m[AdmiralUserIDKVKey])
	}
}

func TestResolveAdmiralUserID_ContextCancelledDuringBackoff(t *testing.T) {
	// Use the production-ish backoff so the first failure forces a sleep
	// long enough to observe ctx cancellation, but bounded so the test
	// doesn't actually wait 5s.
	prev := resolveBackoffs
	resolveBackoffs = []time.Duration{0, 200 * time.Millisecond, 200 * time.Millisecond}
	t.Cleanup(func() { resolveBackoffs = prev })

	v := &fakeViewer{failCount: 99, err: errors.New("transient")}
	kv := newMemKV()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ResolveAdmiralUserID(ctx, "", v, kv, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded propagation, got %v", err)
	}
	// We at least did the first attempt (which uses ctx) and then bailed
	// during the first backoff window.
	if v.calls < 1 {
		t.Errorf("expected at least 1 attempt before ctx cancel kicked in; got %d", v.calls)
	}
}

func TestResolveAdmiralUserID_NilLinearClient_NoCache_NoConfig(t *testing.T) {
	// Defensive: misconfigured caller (nil lc, nil/empty cache, no config)
	// should surface a clear programmer-error message, not panic.
	id, err := ResolveAdmiralUserID(context.Background(), "", nil, newMemKV(), nil)
	if err == nil {
		t.Fatalf("expected error on nil linear client with no cache, got id=%q", id)
	}
}

func TestResolveAdmiralUserID_NilLinearClient_WithCache(t *testing.T) {
	// nil lc is fine when the cache has the value — no API call needed.
	kv := newMemKV()
	kv.m[AdmiralUserIDKVKey] = "u-cached"
	id, err := ResolveAdmiralUserID(context.Background(), "", nil, kv, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "u-cached" {
		t.Errorf("id = %q, want u-cached", id)
	}
}
