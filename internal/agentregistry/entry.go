// Package agentregistry is a daemonless registry of running agents.
//
// The filesystem is the source of truth: each running agent owns one directory
// under a per-user runtime root (see Root), holding a meta.json record. Creating
// the directory means "I'm alive"; removing it means "I'm gone". There is no
// daemon and no shared database — which deliberately sidesteps the single-writer
// SQLite constraint (SetMaxOpenConns(1)) the other services use.
//
// Liveness is derived, never stored: List re-derives a per-entry verdict at read
// time via a (pid, start-time) identity guard, so a crashed producer that never
// deregistered just leaves a directory that reconciliation reports as stale.
//
// Two producers self-register a run's process group while it executes: the
// agent-shell interactive submit (Backend "agent-shell") and the agentsched
// scheduled runner (Backend "agent-sched"). `agent-shell ps` lists them across
// producers; process control is `agent-shell kill`/`agent-shell signal` (guarded
// by the (pid, start-time) identity check); stale-entry GC happens on Kill.
package agentregistry

import "time"

// Entry is the on-disk record for one running agent (meta.json) and the unit
// List returns. Fields are append-only; readers tolerate unknown fields, so the
// schema can grow without a migration.
type Entry struct {
	SchemaVersion int    `json:"schema_version"`      // = entrySchemaVersion
	ID            string `json:"id"`                  // producer-chosen id (uuid); == subdir name
	ParentID      string `json:"parent_id,omitempty"` // enclosing run's ID, if this run was itself launched by a registered run (see AGENT_RUN_ID)
	TaskID        string `json:"task_id,omitempty"`   // task this run belongs to, if any
	PID           int    `json:"pid"`                 // child leader pid
	PGID          int    `json:"pgid"`                // child process-group id (== pid for a setpgid leader)
	// StartTimeUnixNano is the kernel process start-time in nanoseconds since the
	// epoch — the identity-guard value, matched exactly against a fresh read.
	// Nanosecond (not whole-second) resolution is deliberate: it eliminates the
	// collision where a pid recycled within the same wall-clock second would read
	// back an identical start-time and defeat the guard. 0 means the guard is
	// unavailable for this entry (e.g. Windows, or a start-time read that failed
	// at registration), in which case reconciliation falls back to liveness-only.
	StartTimeUnixNano int64     `json:"start_time_unix_nano"`
	Command           string    `json:"command"`           // the command line (display only)
	Backend           string    `json:"backend,omitempty"` // free-form producer label, e.g. "agent-shell"
	StartedAt         time.Time `json:"started_at"`        // registration wall-clock (display only)

	// Status is derived at read time by reconcile and is never authoritative on
	// disk. It carries json:"status" (not json:"-") so `agent-shell ps --json` includes
	// it; Register writes an inert empty string that every List overwrites.
	Status string `json:"status"` // "running" | "stale"
}

// entrySchemaVersion is the current meta.json schema version.
const entrySchemaVersion = 1

// Entry status values, derived by reconcile.
const (
	StatusRunning = "running"
	StatusStale   = "stale"
)
