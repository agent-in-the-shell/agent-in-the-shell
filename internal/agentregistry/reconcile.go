package agentregistry

// reconcile derives a point-in-time Status for e via the (pid, start-time)
// identity guard, and never acts on it (no signal, no delete). The verdict is
// conservative: any inability to confirm identity (a dead pid, a reused pid, or
// a start-time read error — including a transient one) resolves to "stale",
// never to a false "running".
//
// Status is a momentary recon, not a durable claim. An actor (e.g. `agent-shell
// kill`) MUST re-reconcile immediately before acting and MUST NOT trust a stale
// Status from an earlier List.
func reconcile(e Entry) Entry {
	if !processAlive(e.PID) {
		e.Status = StatusStale
		return e
	}
	if e.StartTimeUnixNano == 0 {
		// Guard unavailable (Windows, or a read failure at register time):
		// fall back to liveness-only.
		e.Status = StatusRunning
		return e
	}
	now, err := processStartTime(e.PID)
	if err != nil || now != e.StartTimeUnixNano {
		// pid reused (different start-time), identity unconfirmable, or a
		// transient read error — all conservatively stale.
		e.Status = StatusStale
		return e
	}
	e.Status = StatusRunning
	return e
}

// guardAllowsAction reports whether an actor (e.g. `agent-shell kill`) may safely
// act on this entry: only a freshly-reconciled running entry clears the guard. A
// stale verdict (dead pid, reused pid, or unconfirmable identity) blocks it. This
// is the circuit breaker Kill/Signal call after a re-reconcile, so a recycled pid
// is never signalled.
func guardAllowsAction(e Entry) bool { return e.Status == StatusRunning }
