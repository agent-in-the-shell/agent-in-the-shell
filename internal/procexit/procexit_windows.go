//go:build windows

package procexit

import "os/exec"

// signalOf is a no-op on Windows: there is no POSIX signal-death concept, so
// signal deaths fall through to the normal-ExitError branch (best-effort). The
// "agent as Unix process" model is POSIX-first; Windows is build-green only.
func signalOf(_ *exec.ExitError) (int, bool) { return 0, false }
