package router

import (
	"sync"
	"time"
)

// rateMeter tracks per-deployment request and token consumption over fixed,
// UTC-minute-aligned windows. It is the in-process counter behind pre-call
// RPM/TPM enforcement (#46) and the live used/remaining numbers on
// GET /v1/limits. Keys match the cooldown map: "logicalModel:depName".
//
// Fixed windows (time.Truncate(time.Minute)) rather than sliding: matches the
// 1m window /v1/limits already reports, is allocation-free per request, and
// the worst-case burst error (2x cap straddling a boundary) is acceptable for
// a self-protective limiter — the provider's own limiter remains the
// authority, this one just stops us from burning retries on deployments we
// already know are saturated.
type rateMeter struct {
	mu      sync.Mutex
	windows map[string]*minuteWindow
}

type minuteWindow struct {
	start    time.Time
	requests int
	tokens   int
}

func newRateMeter() *rateMeter {
	return &rateMeter{windows: make(map[string]*minuteWindow)}
}

// win returns the live window for key, rolling it when the minute has
// changed. Caller must hold mu.
func (m *rateMeter) win(key string, now time.Time) *minuteWindow {
	minute := now.UTC().Truncate(time.Minute)
	w := m.windows[key]
	if w == nil || !w.start.Equal(minute) {
		w = &minuteWindow{start: minute}
		m.windows[key] = w
	}
	return w
}

// tryAcquire atomically admits or rejects one dispatch: it checks both caps
// and, when admitted, counts the attempt against the RPM window under the
// same lock — so concurrent requests cannot all observe "under cap" and
// overshoot (check-then-act must not be split). Attempts count whether or
// not the upstream call later succeeds — a failed call consumed
// provider-side rate limit too, so refunding it would let a flapping
// deployment exceed its cap.
//
// A request's token size is unknown pre-call, so tpm rejects once observed
// usage reaches the cap rather than predicting the next request's cost (the
// same trade-off as the budget check: caps bound sustained, not
// instantaneous, consumption). nil caps never reject, but admitted attempts
// are still counted so /v1/limits can report consumption for uncapped
// deployments.
func (m *rateMeter) tryAcquire(key string, rpm, tpm *int, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.win(key, now)
	if rpm != nil && w.requests >= *rpm {
		return false
	}
	if tpm != nil && w.tokens >= *tpm {
		return false
	}
	w.requests++
	return true
}

// recordTokens adds observed post-response usage (input + output tokens).
func (m *rateMeter) recordTokens(key string, tokens int, now time.Time) {
	if tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.win(key, now).tokens += tokens
}

// usage returns the current minute's counters (zero when the window rolled).
func (m *rateMeter) usage(key string, now time.Time) (requests, tokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	minute := now.UTC().Truncate(time.Minute)
	if w := m.windows[key]; w != nil && w.start.Equal(minute) {
		return w.requests, w.tokens
	}
	return 0, 0
}
