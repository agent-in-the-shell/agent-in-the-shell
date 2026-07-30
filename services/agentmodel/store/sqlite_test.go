package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func newStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleLog(id string, opts ...func(*store.RequestLog)) store.RequestLog {
	rl := store.RequestLog{
		ID:               id,
		OrgID:            "org-1",
		APIKeyHash:       "hash-abc",
		ModelRequested:   "gpt-4",
		ModelUsed:        "gpt-4o",
		Provider:         "openai",
		AuthMode:         agentmodel.AuthModeAPIKey,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CostUSD:          0.0125,
		CostSource:       "priced",
		LatencyMs:        342,
		Status:           "ok",
		CreatedAt:        time.Unix(1700000000, 0).UTC(),
	}
	for _, o := range opts {
		o(&rl)
	}
	return rl
}

func TestLogRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	in := sampleLog("req-1")
	if err := s.LogRequest(ctx, in); err != nil {
		t.Fatalf("LogRequest: %v", err)
	}

	got, err := s.GetRequestLog(ctx, "req-1")
	if err != nil {
		t.Fatalf("GetRequestLog: %v", err)
	}
	if got.ID != in.ID || got.OrgID != in.OrgID || got.Provider != in.Provider {
		t.Errorf("identity mismatch: %+v vs %+v", got, in)
	}
	if got.PromptTokens != 100 || got.CompletionTokens != 50 || got.TotalTokens != 150 {
		t.Errorf("token counts wrong: %+v", got)
	}
	if got.CostUSD != 0.0125 {
		t.Errorf("CostUSD: got %v, want 0.0125", got.CostUSD)
	}
	if got.CostSource != "priced" {
		t.Errorf("CostSource: got %q, want priced", got.CostSource)
	}
	if got.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want api_key", got.AuthMode)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", got.CreatedAt, in.CreatedAt)
	}
}

func TestGetRequestLog_NotFound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.GetRequestLog(context.Background(), "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestListByOrg_OrderedAndLimited(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		log := sampleLog(stringNum("req-", i), func(r *store.RequestLog) {
			r.CreatedAt = now.Add(-time.Duration(i) * time.Minute)
		})
		if err := s.LogRequest(ctx, log); err != nil {
			t.Fatalf("LogRequest: %v", err)
		}
	}

	got, err := s.ListByOrg(ctx, "org-1", now.Add(-10*time.Minute), 3)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	// Should be DESC by created_at: req-0 (now) first, req-1 second, req-2 third.
	if got[0].ID != "req-0" || got[1].ID != "req-1" || got[2].ID != "req-2" {
		t.Errorf("ordering: got %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestListByOrg_FiltersBySince(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	old := sampleLog("old", func(r *store.RequestLog) {
		r.CreatedAt = time.Unix(1000, 0).UTC()
	})
	recent := sampleLog("recent", func(r *store.RequestLog) {
		r.CreatedAt = time.Unix(2000, 0).UTC()
	})
	_ = s.LogRequest(ctx, old)
	_ = s.LogRequest(ctx, recent)

	got, err := s.ListByOrg(ctx, "org-1", time.Unix(1500, 0), 100)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(got) != 1 || got[0].ID != "recent" {
		t.Errorf("expected only recent log, got %+v", got)
	}
}

func TestSumCostByOrg(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	costs := []float64{1.0, 2.5, 0.25}
	for i, c := range costs {
		log := sampleLog(stringNum("req-", i), func(r *store.RequestLog) {
			r.CostUSD = c
		})
		_ = s.LogRequest(ctx, log)
	}

	// Subscription request shouldn't fail anything but should still count.
	subLog := sampleLog("sub-1", func(r *store.RequestLog) {
		r.CostUSD = 0
		r.AuthMode = agentmodel.AuthModeSubscription
	})
	_ = s.LogRequest(ctx, subLog)

	// Errored request should not be summed.
	errLog := sampleLog("err-1", func(r *store.RequestLog) {
		r.CostUSD = 100
		r.Status = "error"
		r.ErrorType = agentmodel.ErrTypeRateLimit
	})
	_ = s.LogRequest(ctx, errLog)

	sum, err := s.SumCostByOrg(ctx, "org-1", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("SumCostByOrg: %v", err)
	}
	want := 1.0 + 2.5 + 0.25
	if sum != want {
		t.Errorf("SumCostByOrg: got %v, want %v (errored requests should not count)", sum, want)
	}
}

func TestCountRequestsByOrg_FilterByAuthMode(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	apiKeyLogs := []store.RequestLog{
		sampleLog("a-1"),
		sampleLog("a-2"),
		sampleLog("a-3"),
	}
	subLogs := []store.RequestLog{
		sampleLog("s-1", func(r *store.RequestLog) { r.AuthMode = agentmodel.AuthModeSubscription }),
		sampleLog("s-2", func(r *store.RequestLog) { r.AuthMode = agentmodel.AuthModeSubscription }),
	}
	for _, l := range append(apiKeyLogs, subLogs...) {
		_ = s.LogRequest(ctx, l)
	}

	all, _ := s.CountRequestsByOrg(ctx, "org-1", time.Unix(0, 0), "")
	if all != 5 {
		t.Errorf("all: got %d, want 5", all)
	}

	apiKeyCount, _ := s.CountRequestsByOrg(ctx, "org-1", time.Unix(0, 0), agentmodel.AuthModeAPIKey)
	if apiKeyCount != 3 {
		t.Errorf("api_key: got %d, want 3", apiKeyCount)
	}

	subCount, _ := s.CountRequestsByOrg(ctx, "org-1", time.Unix(0, 0), agentmodel.AuthModeSubscription)
	if subCount != 2 {
		t.Errorf("subscription: got %d, want 2", subCount)
	}
}

func TestConcurrentWrites(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.LogRequest(ctx, sampleLog(stringNum("c-", i)))
		}(i)
	}
	wg.Wait()

	got, _ := s.ListByOrg(ctx, "org-1", time.Unix(0, 0), 100)
	if len(got) != N {
		t.Errorf("len: got %d, want %d", len(got), N)
	}
}

func TestLogRequest_DefaultsCreatedAt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	log := store.RequestLog{
		ID:             "auto-1",
		OrgID:          "org-1",
		APIKeyHash:     "h",
		ModelRequested: "m", ModelUsed: "m", Provider: "p",
		AuthMode: agentmodel.AuthModeAPIKey,
		Status:   "ok",
		// CreatedAt left zero
	}
	before := time.Now().UTC()
	if err := s.LogRequest(ctx, log); err != nil {
		t.Fatalf("LogRequest: %v", err)
	}
	got, _ := s.GetRequestLog(ctx, "auto-1")
	if got.CreatedAt.Before(before.Add(-time.Second)) {
		t.Errorf("CreatedAt should default to now, got %v", got.CreatedAt)
	}
}

func TestPurge_DeletesOldRows(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	old := sampleLog("old", func(rl *store.RequestLog) { rl.CreatedAt = time.Unix(1000, 0).UTC() })
	recent := sampleLog("recent", func(rl *store.RequestLog) { rl.CreatedAt = time.Unix(2000, 0).UTC() })
	for _, l := range []store.RequestLog{old, recent} {
		if err := s.LogRequest(ctx, l); err != nil {
			t.Fatalf("LogRequest %s: %v", l.ID, err)
		}
	}

	n, err := s.Purge(ctx, time.Unix(1500, 0))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}
	if _, err := s.GetRequestLog(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old row should be purged, got err=%v", err)
	}
	if _, err := s.GetRequestLog(ctx, "recent"); err != nil {
		t.Errorf("recent row should remain: %v", err)
	}

	// Purging again with the same cutoff is a no-op.
	n2, err := s.Purge(ctx, time.Unix(1500, 0))
	if err != nil {
		t.Fatalf("Purge (second): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second purge removed %d rows, want 0", n2)
	}
}

// helper to avoid sprintf import noise in tests
func stringNum(prefix string, n int) string {
	const digits = "0123456789"
	if n < 10 {
		return prefix + string(digits[n])
	}
	return prefix + string(digits[n/10]) + string(digits[n%10])
}

func TestSumCostByAPIKey(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	// Two keys spending into the same org; sums must stay per-key.
	for i, c := range []float64{1.0, 0.5} {
		log := sampleLog(stringNum("ka-", i), func(r *store.RequestLog) {
			r.APIKeyHash = "key-a"
			r.CostUSD = c
		})
		_ = s.LogRequest(ctx, log)
	}
	_ = s.LogRequest(ctx, sampleLog("kb-1", func(r *store.RequestLog) {
		r.APIKeyHash = "key-b"
		r.CostUSD = 4.0
	}))
	// Errored request on key-a must not count.
	_ = s.LogRequest(ctx, sampleLog("ka-err", func(r *store.RequestLog) {
		r.APIKeyHash = "key-a"
		r.CostUSD = 100
		r.Status = "error"
	}))

	sum, err := s.SumCostByAPIKey(ctx, "key-a", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("SumCostByAPIKey: %v", err)
	}
	if sum != 1.5 {
		t.Errorf("key-a sum: got %v, want 1.5", sum)
	}

	// Zero time covers the whole ledger (lifetime caps).
	sum, err = s.SumCostByAPIKey(ctx, "key-a", time.Time{})
	if err != nil {
		t.Fatalf("SumCostByAPIKey zero-time: %v", err)
	}
	if sum != 1.5 {
		t.Errorf("key-a lifetime sum: got %v, want 1.5", sum)
	}

	// `since` excludes older rows: everything was just written, so a future
	// cutoff sums to zero.
	sum, err = s.SumCostByAPIKey(ctx, "key-a", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SumCostByAPIKey future: %v", err)
	}
	if sum != 0 {
		t.Errorf("future-window sum: got %v, want 0", sum)
	}

	// Unknown key: zero, no error.
	sum, err = s.SumCostByAPIKey(ctx, "key-none", time.Unix(0, 0))
	if err != nil || sum != 0 {
		t.Errorf("unknown key: got (%v, %v), want (0, nil)", sum, err)
	}
}
