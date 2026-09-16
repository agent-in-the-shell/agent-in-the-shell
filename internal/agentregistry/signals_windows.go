//go:build windows

package agentregistry

import "syscall"

// signalNames are the signal names `agent-shell signal` accepts on Windows. The
// Windows syscall package defines only this common subset; procgroup.Signal in
// turn honors only TERM/KILL (terminate-only) and rejects the rest.
var signalNames = map[string]syscall.Signal{
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM,
}
