package discoverer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
)

// AdmiralUserIDKVKey is the kv-table key under which admiral's resolved
// Linear user UUID is cached after a successful viewer lookup. Exported so
// operators can locate (and, if it ever goes stale, manually delete) the
// row.
const AdmiralUserIDKVKey = "discoverer.admiral_user_id"

// ViewerLookup is the narrow subset of linear.Client that ResolveAdmiralUserID
// depends on — kept small so tests can drop in a fake without satisfying the
// full Linear client surface.
type ViewerLookup interface {
	GetViewer(ctx context.Context) (*linear.Viewer, error)
}

// KVStore is the kv subset of *store.Store the resolver uses for caching.
type KVStore interface {
	KVGet(key string) (string, error)
	KVSet(key, value string) error
}

// resolveBackoffs is the per-attempt delay used by ResolveAdmiralUserID:
// entry 0 is the pre-delay for the first attempt (always zero — try
// immediately), subsequent entries are how long to wait BEFORE the next
// attempt on failure. With 3 entries [0, 5s, 10s] we get 3 attempts total,
// spaced ~5s and ~10s apart — ~15s of wall-clock retry budget, which covers
// the typical TLS-handshake / DNS / transient-5xx class of failure without
// pinning ops on a hard crash loop.
//
// var (not const) so tests can shrink to near-zero.
var resolveBackoffs = []time.Duration{0, 5 * time.Second, 10 * time.Second}

// ResolveAdmiralUserID returns admiral's Linear user UUID using the following
// precedence:
//
//  1. configuredID — explicit YAML config wins unconditionally (operator
//     override; no API call, no DB read).
//  2. kvStore cache — written on a prior successful API lookup; lets the
//     discoverer boot cold even when Linear is unreachable, as long as the
//     identity was resolved at least once in the past.
//  3. lc.GetViewer with retry/backoff — last resort. On the first successful
//     attempt the value is written back to the cache so the next boot can
//     short-circuit at path (2).
//
// Returns the error from the final API attempt (wrapped) once retries are
// exhausted. The caller (admiral-discoverer's main) treats this as fatal.
//
// logger may be nil for tests; production callers should always pass one.
func ResolveAdmiralUserID(ctx context.Context, configuredID string, lc ViewerLookup, kvStore KVStore, logger *slog.Logger) (string, error) {
	if configuredID != "" {
		return configuredID, nil
	}

	if kvStore != nil {
		cached, err := kvStore.KVGet(AdmiralUserIDKVKey)
		if err != nil && logger != nil {
			// Non-fatal: fall through to API path. The cache is an
			// optimization, not the source of truth.
			logger.Warn("admiral_user_id_cache_read_failed", "err", err)
		}
		if cached != "" {
			if logger != nil {
				logger.Info("admiral_user_id_from_cache", "user_id", cached, "kv_key", AdmiralUserIDKVKey)
			}
			return cached, nil
		}
	}

	if lc == nil {
		return "", errors.New("ResolveAdmiralUserID: linear client is nil and no cached/configured id")
	}

	var lastErr error
	for attempt, backoff := range resolveBackoffs {
		if backoff > 0 {
			if logger != nil {
				logger.Warn("admiral_user_id_lookup_retry",
					"attempt", attempt+1, "max", len(resolveBackoffs),
					"backoff", backoff, "prev_err", lastErr)
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		v, err := lc.GetViewer(ctx)
		if err == nil {
			if kvStore != nil {
				if setErr := kvStore.KVSet(AdmiralUserIDKVKey, v.ID); setErr != nil && logger != nil {
					// Cache-write failure is non-fatal — discoverer can still
					// run this boot using the freshly-resolved id; just won't
					// benefit from the cache on the next boot.
					logger.Warn("admiral_user_id_cache_write_failed", "err", setErr)
				}
			}
			if logger != nil {
				logger.Info("admiral_user_resolved", "user_id", v.ID, "name", v.Name, "attempt", attempt+1)
			}
			return v.ID, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("viewer lookup failed after %d attempts: %w", len(resolveBackoffs), lastErr)
}
