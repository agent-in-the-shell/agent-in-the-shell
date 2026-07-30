package cache

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic TTL tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func mustGet(t *testing.T, c Cache, key string) []byte {
	t.Helper()
	v, ok := c.Get(context.Background(), key)
	if !ok {
		t.Fatalf("expected hit for %q, got miss", key)
	}
	return v
}

func mustMiss(t *testing.T, c Cache, key string) {
	t.Helper()
	if v, ok := c.Get(context.Background(), key); ok {
		t.Fatalf("expected miss for %q, got %q", key, v)
	}
}

func TestMemory_GetSetMiss(t *testing.T) {
	c := newMemory(8, 0, time.Now)
	mustMiss(t, c, "absent")
	c.Set(context.Background(), "k", []byte("v"))
	if got := string(mustGet(t, c, "k")); got != "v" {
		t.Fatalf("got %q, want v", got)
	}
}

func TestMemory_LRUEviction(t *testing.T) {
	c := newMemory(2, 0, time.Now)
	ctx := context.Background()
	c.Set(ctx, "a", []byte("1"))
	c.Set(ctx, "b", []byte("2"))
	_ = mustGet(t, c, "a") // touch a so b is the LRU victim
	c.Set(ctx, "c", []byte("3"))
	mustGet(t, c, "a")
	mustGet(t, c, "c")
	mustMiss(t, c, "b") // evicted
}

func TestMemory_TTLExpiry(t *testing.T) {
	clk := newFakeClock()
	c := newMemory(8, time.Minute, clk.Now)
	c.Set(context.Background(), "k", []byte("v"))
	mustGet(t, c, "k")
	clk.advance(61 * time.Second)
	mustMiss(t, c, "k")
}

func TestMemory_SetOverwritesAndRefreshesTTL(t *testing.T) {
	clk := newFakeClock()
	c := newMemory(8, time.Minute, clk.Now)
	ctx := context.Background()
	c.Set(ctx, "k", []byte("v1"))
	clk.advance(50 * time.Second)
	c.Set(ctx, "k", []byte("v2")) // refreshes expiry
	clk.advance(50 * time.Second)
	if got := string(mustGet(t, c, "k")); got != "v2" {
		t.Fatalf("got %q, want v2 (refreshed)", got)
	}
}

func TestMemory_DefensiveCopy(t *testing.T) {
	c := newMemory(8, 0, time.Now)
	in := []byte("orig")
	c.Set(context.Background(), "k", in)
	in[0] = 'X' // mutate caller's buffer after Set
	if got := string(mustGet(t, c, "k")); got != "orig" {
		t.Fatalf("cache aliased caller buffer: got %q", got)
	}
	out := mustGet(t, c, "k")
	out[0] = 'Y' // mutate returned buffer
	if got := string(mustGet(t, c, "k")); got != "orig" {
		t.Fatalf("cache returned aliased buffer: got %q", got)
	}
}

func TestSQLite_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	c1, err := newSQLite(path, 0, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	c1.Set(context.Background(), "k", []byte("v"))
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := newSQLite(path, 0, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := string(mustGet(t, c2, "k")); got != "v" {
		t.Fatalf("got %q, want v after reopen", got)
	}
}

func TestSQLite_TTLExpiryAndPurgeOnOpen(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "cache.db")
	c1, err := newSQLite(path, time.Minute, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	c1.Set(context.Background(), "k", []byte("v"))
	clk.advance(61 * time.Second)
	mustMiss(t, c1, "k") // lazy expiry on read
	_ = c1.Close()

	// Reopen past the deadline: the open-time sweep should have removed it.
	c2, err := newSQLite(path, time.Minute, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var n int
	if err := c2.db.QueryRow(`SELECT count(*) FROM response_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected expired row swept at open, found %d rows", n)
	}
}

func TestTiered_PromotesFromL2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	l2, err := newSQLite(path, 0, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	l1 := newMemory(8, 0, time.Now)
	tc := &tiered{l1: l1, l2: l2}
	ctx := context.Background()

	// Seed only L2, then read through the tier: it should hit L2 and promote.
	l2.Set(ctx, "k", []byte("v"))
	mustMiss(t, l1, "k")
	if got := string(mustGet(t, tc, "k")); got != "v" {
		t.Fatalf("tiered got %q, want v", got)
	}
	if got := string(mustGet(t, l1, "k")); got != "v" {
		t.Fatalf("expected L2 hit promoted into L1, L1 got %q", got)
	}
}

func TestTiered_WritesBothLayers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	c, err := New(Config{SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Set(context.Background(), "k", []byte("v"))

	// A fresh L2 opened on the same file proves the write reached SQLite.
	l2, err := newSQLite(path, 0, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if got := string(mustGet(t, l2, "k")); got != "v" {
		t.Fatalf("L2 got %q, want v", got)
	}
}

func TestNew_MemoryOnlyWhenNoPath(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(*memory); !ok {
		t.Fatalf("expected *memory with no SQLitePath, got %T", c)
	}
}

func TestMemory_ConcurrentAccess(t *testing.T) {
	c := newMemory(64, 0, time.Now)
	ctx := context.Background()
	var hits int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Set(ctx, "k", []byte("v"))
				if _, ok := c.Get(ctx, "k"); ok {
					atomic.AddInt64(&hits, 1)
				}
			}
		}()
	}
	wg.Wait()
	if hits == 0 {
		t.Fatal("expected some hits under concurrency")
	}
}
