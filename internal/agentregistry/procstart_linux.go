//go:build linux

package agentregistry

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// clkTck is USER_HZ. There is no portable, cgo-free _SC_CLK_TCK on Linux, and
// 100 is USER_HZ on every Linux userspace target in practice. It does not need
// to be exact: it scales both the value recorded at register time and the value
// re-read at reconcile time, so it cancels in the equality test — reconcile
// needs a stable, distinct anchor per process, not absolute accuracy.
const clkTck = 100

const nanosPerSec = 1_000_000_000

// readProcPidStat and readProcStat are seams (overridden in tests) for the two
// /proc reads, so the comm-parsing pitfall can be exercised deterministically.
var (
	readProcPidStat = func(pid int) (string, error) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return string(b), err
	}
	readProcStat = func() (string, error) {
		b, err := os.ReadFile("/proc/stat")
		return string(b), err
	}
)

// processStartTime returns pid's start-time in Unix nanoseconds, derived from
// /proc/<pid>/stat field 22 (starttime, in clock ticks since boot) plus
// /proc/stat btime (boot time in Unix seconds). Both inputs are fixed for the
// life of the process, so two reads of the same process return the identical
// value (the guard matches exactly).
func processStartTime(pid int) (int64, error) {
	line, err := readProcPidStat(pid)
	if err != nil {
		return 0, err
	}
	// Find the LAST ')', which ends the (comm) field. Any ')' inside comm is to
	// its left, so the rightmost one is the unambiguous field-2 terminator. Split
	// the fields AFTER this index; starttime is field 22 overall = index 19 of
	// the post-')' fields. Using LastIndexByte (not Split/Index on the first
	// ')') is required: a process named e.g. "))(foo))" would otherwise misparse
	// and reconciliation would report a live pid as stale.
	idx := strings.LastIndexByte(line, ')')
	if idx < 0 {
		return 0, fmt.Errorf("agentregistry: malformed /proc/%d/stat (no comm terminator)", pid)
	}
	fields := strings.Fields(line[idx+1:])
	const startTimeIdx = 19
	if len(fields) <= startTimeIdx {
		return 0, fmt.Errorf("agentregistry: /proc/%d/stat has %d post-comm fields, want >%d", pid, len(fields), startTimeIdx)
	}
	ticks, err := strconv.ParseInt(fields[startTimeIdx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("agentregistry: parse starttime: %w", err)
	}
	btime, err := bootTimeUnix()
	if err != nil {
		return 0, err
	}
	// Nanoseconds since the epoch, computed overflow-safely: keep the whole-second
	// part (btime + ticks/clkTck, ~1.7e9) and the fractional ticks separate so no
	// intermediate (e.g. ticks*1e9 on a long-uptime host) can overflow int64.
	startSec := btime + ticks/clkTck
	fracNanos := (ticks % clkTck) * nanosPerSec / clkTck
	return startSec*nanosPerSec + fracNanos, nil
}

// bootTimeUnix parses the btime line (boot time in Unix seconds) from /proc/stat.
func bootTimeUnix() (int64, error) {
	stat, err := readProcStat()
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(stat, "\n") {
		if rest, ok := strings.CutPrefix(l, "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		}
	}
	return 0, fmt.Errorf("agentregistry: btime not found in /proc/stat")
}
