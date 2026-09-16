// Package procexit decodes an *exec.Cmd run error into an honest, two-field
// process-plane outcome: a POSIX exit code (128+signal on signal death) plus
// whether that code is actually known.
//
// It exists so the two executors that run subprocesses — the agentsched runner
// and the agentshell submitter — classify exit status through one code path and
// cannot drift. Stdlib only; no external dependency.
package procexit

import (
	"errors"
	"os/exec"
)

// Decoded is the honest process-plane outcome of a finished command.
type Decoded struct {
	ExitCode int  // 0–255 normal exit, or 128+signal on signal death
	Signal   int  // positive signal number if Signaled; 0 otherwise
	Signaled bool // true => killed by a signal
	Known    bool // false => ExitCode is a best-effort placeholder (non-ExitError)
}

// Decode classifies runErr (the return of cmd.Run/cmd.Wait):
//   - nil                            -> {0, 0, false, true}
//   - *exec.ExitError, signal death  -> {128+sig, sig, true, true}
//   - *exec.ExitError, normal exit   -> {code, 0, false, true}
//   - anything else (start failure)  -> {1, 0, false, false}
//
// The 128+signal mapping (SIGTERM=143, SIGKILL=137, SIGPIPE=141) is the POSIX
// shell convention; it never returns the negative code that Go's
// ExitError.ExitCode() yields on signal death.
func Decode(runErr error) Decoded {
	if runErr == nil {
		return Decoded{ExitCode: 0, Known: true}
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		// Start/pipe failure (e.g. *exec.Error): the process never produced an
		// exit status, so the 1 is a best-effort placeholder, not a real code.
		return Decoded{ExitCode: 1, Known: false}
	}
	if sig, ok := signalOf(ee); ok {
		return Decoded{ExitCode: 128 + sig, Signal: sig, Signaled: true, Known: true}
	}
	return Decoded{ExitCode: ee.ExitCode(), Known: true}
}
