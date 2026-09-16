//go:build darwin

package agentregistry

import "golang.org/x/sys/unix"

// sysctlKinfoProc is a seam (overridden in tests) so a transient sysctl error
// can be injected.
var sysctlKinfoProc = unix.SysctlKinfoProc

const nanosPerSec = 1_000_000_000
const nanosPerMicro = 1_000

// processStartTime returns pid's start-time in Unix nanoseconds via a cgo-free
// sysctl (kern.proc.pid), reading the kinfo_proc start-time Timeval (seconds +
// microseconds). Using the microsecond field (not just whole seconds) gives the
// identity guard sub-second resolution, so a pid recycled within the same second
// reads a different start-time and is correctly judged stale. The value is fixed
// for the life of the process, so two reads match exactly.
//
// On transient sysctl errors (EIO, ENOMEM), we return the error and reconcile
// marks the entry stale. This is conservative: a live process may be briefly
// misreported as orphaned, self-correcting on the next List once the transient
// clears. We do NOT retry/backoff (transients are vanishingly rare here) and we
// do NOT distinguish ESRCH from transient errors (reconcile only reports; nothing
// is killed on a stale verdict — kill/signal must re-reconcile before acting).
func processStartTime(pid int) (int64, error) {
	ki, err := sysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	tv := ki.Proc.P_starttime
	return int64(tv.Sec)*nanosPerSec + int64(tv.Usec)*nanosPerMicro, nil
}
