//go:build !linux && !darwin

package main

import "os"

// terminalWidth has no implementation outside the release targets (linux and
// darwin, per .goreleaser.yaml). Reporting 0 — "not a terminal" — degrades to
// the unadapted full-width table, which is what this command printed before
// width adaptation existed, so an unsupported GOOS still builds and works.
func terminalWidth(_ *os.File) int { return 0 }
