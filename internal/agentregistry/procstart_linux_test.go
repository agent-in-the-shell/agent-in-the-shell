//go:build linux

package agentregistry

import "testing"

// TestProcessStartTime_CommParsing proves the /proc/<pid>/stat parser uses the
// LAST ')' to terminate the (comm) field, so a process whose comm contains ')'
// and spaces does not misparse a wrong field as starttime. The read seams are
// overridden to inject a pathological stat line with a known starttime.
func TestProcessStartTime_CommParsing(t *testing.T) {
	// comm = "))(foo) bar)" — full of ')' and a space. Fields after the LAST ')':
	//   state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt
	//   utime stime cutime cstime priority nice num_threads itrealvalue starttime ...
	// starttime is index 19 of the post-')' fields; we set it to 4242.
	post := "S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 4242 0 0"
	statLine := "1234 ())(foo) bar) " + post

	origStat, origBoot := readProcPidStat, readProcStat
	t.Cleanup(func() { readProcPidStat, readProcStat = origStat, origBoot })
	readProcPidStat = func(int) (string, error) { return statLine, nil }
	readProcStat = func() (string, error) { return "btime 1000000\nother 1\n", nil }

	got, err := processStartTime(1234)
	if err != nil {
		t.Fatalf("processStartTime: %v", err)
	}
	// nanoseconds = (btime + ticks/clkTck)*1e9 + (ticks%clkTck)*1e9/clkTck
	//             = (1000000 + 42)*1e9 + 42*1e9/100  (btime=1000000, ticks=4242)
	const btime, ticks = 1000000, 4242
	want := int64((btime+ticks/clkTck)*nanosPerSec + (ticks%clkTck)*nanosPerSec/clkTck)
	if got != want {
		t.Errorf("got %d, want %d (parser misidentified the starttime field)", got, want)
	}
}

func TestBootTimeUnix_Missing(t *testing.T) {
	orig := readProcStat
	t.Cleanup(func() { readProcStat = orig })
	readProcStat = func() (string, error) { return "cpu 1 2 3\nintr 9\n", nil } // no btime
	if _, err := bootTimeUnix(); err == nil {
		t.Error("expected an error when btime is absent from /proc/stat")
	}
}
