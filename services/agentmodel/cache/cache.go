// Package cache provides an optional response cache for the agentmodel gateway:
// a bounded in-memory LRU (L1) with an optional SQLite persistence layer (L2).
//
// It is a generic TTL'd key→bytes store — callers (the api layer) own key
// derivation and value (un)marshalling, so the same cache can back chat
// completions today and embeddings/messages later. It mirrors LiteLLM's
// response-cache concept (#48), but is SQLite-backed rather than Redis to match
// the stack's zero-external-dependency posture (single static binary).
package cache

import (
	"context"
	"errors"
	"strings"
	"time"
)

// defaultMaxItems bounds the in-memory LRU when MaxItems is unset. Each entry is
// a full response body, so this caps memory at roughly maxItems × response size.
const defaultMaxItems = 1000

// Cache is a TTL'd key→bytes store. Get reports whether a live (unexpired) entry
// exists; Set stores a value with the cache's configured TTL. All methods are
// safe for concurrent use, and returned/stored byte slices are copied so callers
// can mutate their buffers freely.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte)
	Close() error
}

// Config configures New.
type Config struct {
	// TTL is the lifetime of a cached entry. Zero means entries never expire and
	// are evicted only by L1 LRU pressure (and never from L2) — set a TTL when
	// persisting to SQLite so the file does not grow unbounded.
	TTL time.Duration
	// MaxItems is the L1 LRU capacity; <= 0 uses defaultMaxItems.
	MaxItems int
	// SQLitePath, when non-empty, adds a persistent L2 at that path. Empty keeps
	// the cache memory-only (lost on restart).
	SQLitePath string
	// Clock is injectable for tests; nil uses time.Now.
	Clock func() time.Time
}

// New builds a Cache from Config. With no SQLitePath it returns the in-memory
// LRU alone; otherwise it returns a tiered L1+L2 cache (memory in front of
// SQLite). It only errors when opening the SQLite L2 fails.
func New(cfg Config) (Cache, error) {
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	maxItems := cfg.MaxItems
	if maxItems <= 0 {
		maxItems = defaultMaxItems
	}
	mem := newMemory(maxItems, cfg.TTL, clock)
	if strings.TrimSpace(cfg.SQLitePath) == "" {
		return mem, nil
	}
	l2, err := newSQLite(cfg.SQLitePath, cfg.TTL, clock)
	if err != nil {
		return nil, err
	}
	return &tiered{l1: mem, l2: l2}, nil
}

// tiered fronts a persistent L2 with an in-memory L1. A read missing in L1 but
// found in L2 is promoted back into L1; a write lands in both.
type tiered struct {
	l1 Cache
	l2 Cache
}

func (t *tiered) Get(ctx context.Context, key string) ([]byte, bool) {
	if v, ok := t.l1.Get(ctx, key); ok {
		return v, true
	}
	if v, ok := t.l2.Get(ctx, key); ok {
		t.l1.Set(ctx, key, v) // promote so the next read skips SQLite
		return v, true
	}
	return nil, false
}

func (t *tiered) Set(ctx context.Context, key string, value []byte) {
	t.l1.Set(ctx, key, value)
	t.l2.Set(ctx, key, value)
}

func (t *tiered) Close() error {
	return errors.Join(t.l1.Close(), t.l2.Close())
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
