package router

import (
	"sync"
	"testing"
	"time"
)

func intPtrRM(i int) *int { return &i }

func TestRateMeter_RPMBoundary(t *testing.T) {
	m := newRateMeter()
	now := time.Date(2026, 6, 11, 10, 0, 30, 0, time.UTC)
	rpm := intPtrRM(2)

	if !m.tryAcquire("m:d", rpm, nil, now) {
		t.Fatal("1st acquire must be admitted")
	}
	if !m.tryAcquire("m:d", rpm, nil, now) {
		t.Fatal("2nd acquire must be admitted")
	}
	if m.tryAcquire("m:d", rpm, nil, now) {
		t.Fatal("3rd acquire must be rejected at rpm=2")
	}

	// Next minute: window rolls, counters reset.
	next := now.Add(time.Minute)
	if !m.tryAcquire("m:d", rpm, nil, next) {
		t.Fatal("rolled window must admit again")
	}
	if reqs, _ := m.usage("m:d", next); reqs != 1 {
		t.Errorf("usage after roll: got %d, want 1", reqs)
	}
}

func TestRateMeter_TPMBoundary(t *testing.T) {
	m := newRateMeter()
	now := time.Date(2026, 6, 11, 10, 0, 30, 0, time.UTC)
	tpm := intPtrRM(100)

	m.recordTokens("m:d", 99, now)
	if !m.tryAcquire("m:d", nil, tpm, now) {
		t.Fatal("99 of 100 tokens must still admit")
	}
	m.recordTokens("m:d", 1, now)
	if m.tryAcquire("m:d", nil, tpm, now) {
		t.Fatal("100 of 100 tokens must reject")
	}
	// Zero/negative token records are ignored.
	m.recordTokens("m:other", 0, now)
	m.recordTokens("m:other", -5, now)
	if _, toks := m.usage("m:other", now); toks != 0 {
		t.Errorf("zero/negative records must be ignored, got %d", toks)
	}
}

func TestRateMeter_KeysAreIndependent(t *testing.T) {
	m := newRateMeter()
	now := time.Date(2026, 6, 11, 10, 0, 30, 0, time.UTC)
	rpm := intPtrRM(1)

	if !m.tryAcquire("m:a", rpm, nil, now) {
		t.Fatal("m:a first acquire must be admitted")
	}
	if m.tryAcquire("m:a", rpm, nil, now) {
		t.Fatal("m:a must be at cap")
	}
	if !m.tryAcquire("m:b", rpm, nil, now) {
		t.Fatal("m:b must be unaffected by m:a's traffic")
	}
}

func TestRateMeter_NilCapsNeverReject(t *testing.T) {
	m := newRateMeter()
	now := time.Date(2026, 6, 11, 10, 0, 30, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		if !m.tryAcquire("m:d", nil, nil, now) {
			t.Fatal("nil caps must never reject")
		}
		m.recordTokens("m:d", 1000, now)
	}
	// Uncapped traffic is still counted for /v1/limits reporting.
	if reqs, _ := m.usage("m:d", now); reqs != 1000 {
		t.Errorf("uncapped attempts must still be counted, got %d", reqs)
	}
}

// The admit-and-count step is atomic: N concurrent acquires against rpm=1
// admit exactly one, regardless of interleaving. This is the check-then-act
// property the review's race repro disproved for the old two-call API.
func TestRateMeter_ConcurrentAcquireDoesNotOvershoot(t *testing.T) {
	m := newRateMeter()
	now := time.Date(2026, 6, 11, 10, 0, 30, 0, time.UTC)
	rpm := intPtrRM(1)

	const goroutines = 64
	var wg sync.WaitGroup
	admitted := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.tryAcquire("m:d", rpm, nil, now) {
				admitted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(admitted)
	n := 0
	for range admitted {
		n++
	}
	if n != 1 {
		t.Fatalf("admitted %d of %d concurrent acquires at rpm=1, want exactly 1", n, goroutines)
	}
}
