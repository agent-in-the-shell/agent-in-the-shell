package agentregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/procgroup"
)

const (
	// defaultKillGrace is how long Kill waits after SIGTERM before escalating to
	// a group SIGKILL. Longer than agentsched's internal 2s: an operator killing
	// interactively can afford the target a bit more time to clean up.
	defaultKillGrace = 5 * time.Second
	// killPollInterval is how often the ladder polls group liveness.
	killPollInterval = 20 * time.Millisecond
	// reapTimeout bounds the post-SIGKILL wait for the group to disappear.
	reapTimeout = 2 * time.Second
)

// groupAliveFn is the seam Kill uses to probe whole-group liveness. It defaults
// to the real groupAlive and exists only so tests can simulate a member that
// outlives the SIGKILL reap window (e.g. a D-state survivor) — a condition that
// cannot be produced deterministically with a real process.
var groupAliveFn = groupAlive

// ErrNotRunning is returned by Kill/Signal when the entry is absent, or is not
// provably the same running process (dead pid, reused pid, or unconfirmable
// identity). The target is never signalled in that case — refusing is the whole
// point of the guard (a recycled pid must not be killed).
//
// Platform note: the recycled-pid guarantee holds on Linux/macOS, where the
// (pid, start-time) guard can distinguish a reused pid. On Windows there is no
// cgo-free start-time source, so reconciliation is liveness-only and a recycled
// pid cannot be detected — Kill/Signal are best-effort there.
type ErrNotRunning struct {
	ID     string
	Reason string
}

func (e *ErrNotRunning) Error() string {
	return fmt.Sprintf("agent %q is not running (%s)", e.ID, e.Reason)
}

// lookup reads and reconciles a single entry by id (fresh from disk, not from a
// cached List). A missing entry is an error.
func lookup(id string) (Entry, error) {
	root, err := Root()
	if err != nil {
		return Entry{}, err
	}
	data, err := os.ReadFile(filepath.Join(root, id, metaFile))
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, &ErrNotRunning{ID: id, Reason: "no such agent"}
		}
		return Entry{}, fmt.Errorf("agentregistry: read entry %q: %w", id, err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, fmt.Errorf("agentregistry: parse entry %q: %w", id, err)
	}
	return reconcile(e), nil
}

// guardedEntry loads, reconciles, and gates an entry. It returns ErrNotRunning
// (without signalling anything) unless the entry is a freshly-confirmed running
// process — the contract every actor must honor before acting.
func guardedEntry(id string) (Entry, error) {
	e, err := lookup(id)
	if err != nil {
		return Entry{}, err
	}
	if !guardAllowsAction(e) {
		return Entry{}, &ErrNotRunning{ID: id, Reason: "stale or identity unconfirmed; refusing to signal"}
	}
	return e, nil
}

// Signal sends exactly one signal to a registered agent's process group, but
// only after re-reconciling and clearing the identity guard. It does not run a
// ladder and does not remove the entry (the agent may survive the signal). The
// reuse guard is full on Linux/macOS and best-effort on Windows (see
// ErrNotRunning). On Windows only terminating signals are honored.
func Signal(id string, sig syscall.Signal) error {
	e, err := guardedEntry(id)
	if err != nil {
		return err
	}
	return procgroup.Signal(e.PGID, sig)
}

// Kill reaps a registered agent's entire process group: re-reconcile + guard,
// then SIGTERM the group, poll group liveness up to grace, SIGKILL any survivors,
// and remove the entry directory once the group is gone. It refuses (ErrNotRunning,
// no signal) on an absent/stale/reused entry. grace <= 0 uses defaultKillGrace.
//
// The recycled-pid refusal is reliable on Linux/macOS; on Windows it is
// best-effort (liveness-only, no start-time guard — see ErrNotRunning) and the
// ladder degenerates to a single TerminateProcess on the leader.
func Kill(id string, grace time.Duration) error {
	e, err := guardedEntry(id)
	if err != nil {
		return err
	}
	if grace <= 0 {
		grace = defaultKillGrace
	}

	// Rung 1: graceful SIGTERM to the whole group.
	if err := procgroup.Signal(e.PGID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("agentregistry: SIGTERM group %d: %w", e.PGID, err)
	}

	// Poll whole-group liveness (not just the leader) so a SIGTERM-trapping child
	// that outlives the leader is still caught and escalated.
	deadline := time.Now().Add(grace)
	for groupAliveFn(e.PGID) && time.Now().Before(deadline) {
		time.Sleep(killPollInterval)
	}

	// Rung 2: SIGKILL any survivors.
	if groupAliveFn(e.PGID) {
		if err := procgroup.Signal(e.PGID, syscall.SIGKILL); err != nil {
			return fmt.Errorf("agentregistry: SIGKILL group %d: %w", e.PGID, err)
		}
		reapBy := time.Now().Add(reapTimeout)
		for groupAliveFn(e.PGID) && time.Now().Before(reapBy) {
			time.Sleep(killPollInterval)
		}
	}

	// Final aliveness check before de-registering. SIGKILL is asynchronous and the
	// reap loop is bounded (reapTimeout); a member stuck in uninterruptible D-state
	// (e.g. blocked on disk/NFS I/O) can outlive the window. Removing the entry then
	// would report success and de-register a process that is still running — it
	// would vanish from the registry yet keep executing, becoming an invisible,
	// un-re-killable orphan. So only GC once the group is provably gone; otherwise
	// surface an error and leave the entry in place so it stays visible and the
	// caller can retry the Kill.
	if groupAliveFn(e.PGID) {
		return fmt.Errorf("agentregistry: group %d still alive after SIGKILL+reap (%s); entry left registered for retry", e.PGID, reapTimeout)
	}

	// GC the reaped entry. Idempotent: the producer's defer Close may also remove
	// it (e.g. an agentsched-managed child whose Runner observed the death first).
	root, err := Root()
	if err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(root, id))
	return nil
}
