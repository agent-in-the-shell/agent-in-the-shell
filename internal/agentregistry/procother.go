//go:build !linux && !darwin

package agentregistry

import "errors"

// errStartTimeUnavailable signals that this platform has no cgo-free process
// start-time source, so the (pid, start-time) identity guard is unavailable.
var errStartTimeUnavailable = errors.New("agentregistry: process start-time unavailable on this platform")

// processStartTime is unavailable on non-Unix platforms (notably Windows).
// Register records StartTimeUnix=0 and reconcile falls back to liveness-only,
// so the registry is best-effort here (no PID-reuse detection).
func processStartTime(_ int) (int64, error) { return 0, errStartTimeUnavailable }

// processAlive is best-effort on platforms without a cheap existence probe:
// os.Process.Signal supports only Kill on Windows, so a signal-0 check would
// falsely report every process dead. Reporting alive keeps entries listed
// (liveness-best-effort) rather than always-stale; a real liveness check is a
// later, platform-specific refinement.
func processAlive(pid int) bool { return pid > 0 }

// groupAlive is best-effort on platforms without process groups (Windows): there
// is no cheap whole-group existence probe, so it mirrors processAlive.
func groupAlive(pgid int) bool { return pgid > 0 }
