//go:build linux || darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// terminalWidth returns f's column count, or 0 when f is not a terminal — a
// pipe, a redirect, a CI log. Callers must leave the output unadapted at 0
// rather than guess a width, so redirected bytes stay identical to what scripts
// already parse.
//
// The width is re-read per frame rather than cached: `limits --watch` holds the
// terminal for minutes at a time, which is long enough for a resize to happen
// between two frames.
func terminalWidth(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}
