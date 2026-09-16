package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// fixtureNow anchors every RESETS countdown below, so the rendered widths are
// the same on every machine and in every zone (the tests render in UTC).
var fixtureNow = time.Date(2026, 8, 12, 6, 2, 11, 0, time.UTC)

// limitsFixture reproduces a real `limits --profiles all` report: two Anthropic
// profiles, codex, and the per-model codex row whose WINDOW cell
// ("codex_bengalfox_weekly (GPT-5.3-Codex-Spark)", 44 columns) is what pushes
// the full table past a normal terminal.
func limitsFixture() limitsReport {
	at := func(d time.Duration) *time.Time { t := fixtureNow.Add(d); return &t }
	pct := func(v float64) *float64 { return &v }
	report := limitsReport{SchemaVersion: limitsSchemaVersion, CheckedAt: fixtureNow}
	for _, profile := range []string{"default", "max"} {
		report.Backends = append(report.Backends, limitsBackend{
			Agent: "claude", Profile: profile, Status: "ok",
			Windows: []limitRow{
				{LimitName: "claude_5h", Status: "ok", UsedPercent: pct(2), Remaining: pct(98),
					ResetAt: at(4*time.Hour + 56*time.Minute), Source: "claude_oauth"},
				{LimitName: "claude_weekly", Status: "ok", UsedPercent: pct(32), Remaining: pct(68),
					ResetAt: at(34 * time.Hour), Source: "claude_oauth"},
			},
		})
	}
	report.Backends = append(report.Backends, limitsBackend{
		Agent: "codex", Status: "ok",
		Windows: []limitRow{
			{LimitName: "codex_weekly", Status: "ok", UsedPercent: pct(1), Remaining: pct(99),
				ResetAt: at(142 * time.Hour), Source: "codex_oauth"},
			{LimitName: "codex_bengalfox_weekly", RawMessage: "GPT-5.3-Codex-Spark", Status: "ok",
				UsedPercent: pct(0), Remaining: pct(100),
				ResetAt: at(168 * time.Hour), Source: "codex_oauth"},
		},
	})
	return report
}

func render(t *testing.T, report limitsReport, width int) string {
	t.Helper()
	var buf bytes.Buffer
	renderLimits(&buf, report, fixtureNow, time.UTC, width)
	return buf.String()
}

func assertFitsWidth(t *testing.T, out string, width int) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if got := cellWidth(line); got > width {
			t.Errorf("line %d is %d columns, over the %d budget: %q", i, got, width, line)
		}
	}
}

// TestRenderLimits_UnknownWidthKeepsWideTable pins the contract that made width
// adaptation safe to add: when stdout is not a terminal (a pipe, a redirect, a
// test buffer) the output is the seven-column table exactly as it was before
// --watch existed, so anything already parsing these columns keeps working.
func TestRenderLimits_UnknownWidthKeepsWideTable(t *testing.T) {
	out := render(t, limitsFixture(), 0)

	const wantHeader = "AGENT           STATUS  WINDOW                                        USED  REMAINING  RESETS                    SOURCE"
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != wantHeader {
		t.Errorf("header changed\n got: %q\nwant: %q", lines[0], wantHeader)
	}
	if len(lines) != 7 {
		t.Fatalf("got %d lines, want 1 header + 6 rows", len(lines))
	}
	if !strings.Contains(lines[1], "claude/default  ok      claude_5h") ||
		!strings.Contains(lines[1], "2%    98         4h 56m (10:58 UTC)        claude_oauth") {
		t.Errorf("first row lost a column: %q", lines[1])
	}
	// The bug this whole change exists for: 125 columns against an 80-column
	// terminal. If this ever drops below 80 the narrow layout is dead code.
	if w := widestLine(out); w <= 80 {
		t.Errorf("fixture is only %d columns wide; it no longer reproduces the overflow", w)
	}
}

// TestRenderLimits_NarrowKeepsOneRowPerLine is the fix for what `watch` does at
// 80 columns: instead of folding each 125-column row onto two lines and
// interleaving neighbours, RESETS and SOURCE move to an indented continuation
// and every row still owns exactly one identity line.
func TestRenderLimits_NarrowKeepsOneRowPerLine(t *testing.T) {
	const width = 80
	out := render(t, limitsFixture(), width)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	assertFitsWidth(t, out, width)

	// RESETS and SOURCE are gone from the header; the identity columns are not.
	const wantHeader = "AGENT           STATUS  WINDOW                                      USED  REMAIN"
	if lines[0] != wantHeader {
		t.Fatalf("narrow header changed\n got: %q\nwant: %q", lines[0], wantHeader)
	}
	// 6 rows: one identity line and one continuation line each, plus a header.
	if len(lines) != 13 {
		t.Fatalf("got %d lines, want 1 header + 6 rows + 6 continuations:\n%s", len(lines), out)
	}
	for i := 1; i < len(lines); i += 2 {
		if strings.HasPrefix(lines[i], "  ") {
			t.Errorf("line %d should be an identity line, got a continuation: %q", i, lines[i])
		}
		if !strings.HasPrefix(lines[i+1], "  resets ") {
			t.Errorf("line %d should be a continuation, got: %q", i+1, lines[i+1])
		}
	}
	// Demoted, not dropped.
	if !strings.Contains(out, "  resets 4h 56m (10:58 UTC) · claude_oauth") {
		t.Errorf("continuation lost the reset or source cell:\n%s", out)
	}
}

// TestRenderLimits_ClipsWindowWhenNarrowStillOverflows covers the last tier: at
// 60 columns even the identity half does not fit, so WINDOW is clipped rather
// than allowed to fold.
func TestRenderLimits_ClipsWindowWhenNarrowStillOverflows(t *testing.T) {
	const width = 60
	out := render(t, limitsFixture(), width)

	assertFitsWidth(t, out, width)
	if !strings.Contains(out, clipMarker) {
		t.Errorf("expected a clipped WINDOW cell at %d columns:\n%s", width, out)
	}
	// Clipping must not silently eat a whole row.
	for _, want := range []string{"claude_5h", "claude_weekly", "codex_weekly", "codex_bengalfox"} {
		if !strings.Contains(out, want) {
			t.Errorf("row %q disappeared under clipping:\n%s", want, out)
		}
	}
}

// TestRenderLimits_FailedProbeRow checks the backend-with-no-windows shape,
// whose cells are built on a different branch than window rows.
func TestRenderLimits_FailedProbeRow(t *testing.T) {
	report := limitsReport{SchemaVersion: limitsSchemaVersion, CheckedAt: fixtureNow,
		Backends: []limitsBackend{
			{Agent: "claude", Profile: "stale", Status: "unknown", Error: "claude_login_required"},
			{Agent: "codex", Status: "unknown"},
		}}

	wantWide := "AGENT         STATUS   WINDOW  USED  REMAINING  RESETS                 SOURCE\n" +
		"claude/stale  unknown  -       -     -          claude_login_required  -\n" +
		"codex         unknown  -       -     -          -                      -\n"
	if wide := render(t, report, 0); wide != wantWide {
		t.Errorf("wide failed-probe rows changed\n got:\n%s\nwant:\n%s", wide, wantWide)
	}

	// The error detail is the one thing worth a continuation line here; the
	// errorless backend gets none, because "-" on its own line is noise.
	//
	// These lines run 43 columns against a 40-column budget, and that is the
	// honest answer: every WINDOW cell is "-", so clipping the only elastic
	// column cannot recover the 3 columns the headers alone need. This is the
	// tier limitsNarrow documents as giving up rather than the layout failing.
	wantNarrow := "AGENT         STATUS   WINDOW  USED  REMAIN\n" +
		"claude/stale  unknown  -       -     -\n" +
		"  claude_login_required\n" +
		"codex         unknown  -       -     -\n"
	if narrow := render(t, report, 40); narrow != wantNarrow {
		t.Errorf("narrow failed-probe rows changed\n got:\n%s\nwant:\n%s", narrow, wantNarrow)
	}
}

func TestParseWatchInterval(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		want    time.Duration
		wantErr bool
	}{
		{spec: "300", want: 5 * time.Minute}, // the watch -n spelling
		{spec: "5m", want: 5 * time.Minute},  // the Go duration spelling
		{spec: "1h30m", want: 90 * time.Minute},
		{spec: "5", want: minWatchInterval},
		{spec: "1", wantErr: true}, // under the floor
		{spec: "0", wantErr: true},
		{spec: "-5", wantErr: true},
		{spec: "", wantErr: true},
		{spec: "later", wantErr: true},
	} {
		got, err := parseWatchInterval(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseWatchInterval(%q) = %s, want an error", tc.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseWatchInterval(%q): %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseWatchInterval(%q) = %s, want %s", tc.spec, got, tc.want)
		}
	}
}

func TestFormatInterval(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{300 * time.Second, "5m"},
		{2 * time.Hour, "2h"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m30s"},
	} {
		if got := formatInterval(tc.d); got != tc.want {
			t.Errorf("formatInterval(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// twoFrames runs watchLimits at the reported width and stops it after exactly
// two painted frames. Cancelling from inside the third probe is what makes that
// deterministic: watchLimits checks ctx before painting, so the third frame is
// never written and the count cannot race the cadence timer.
func twoFrames(t *testing.T, width int) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	probes := 0
	var buf bytes.Buffer
	err := watchLimits(ctx, &buf, time.Millisecond, time.UTC,
		func() int { return width },
		func(context.Context, time.Time) limitsReport {
			if probes++; probes == 3 {
				cancel()
			}
			return limitsFixture()
		})
	if err != nil {
		t.Fatalf("watchLimits: %v", err)
	}
	return buf.String()
}

// TestWatchLimits_RedrawsToATerminal covers the branch the flag exists for: on
// a terminal each frame is preceded by cursor-home + clear, so the table is
// repainted in place instead of scrolling.
func TestWatchLimits_RedrawsToATerminal(t *testing.T) {
	out := twoFrames(t, 80)

	if got := strings.Count(out, "\x1b[H\x1b[2J"); got != 2 {
		t.Errorf("got %d clear-screen sequences, want one per frame:\n%q", got, out)
	}
	if got := strings.Count(out, "every 1ms · "); got != 2 {
		t.Errorf("got %d frame headers, want 2:\n%s", got, out)
	}
	// Width is honoured per frame, not just on the first paint.
	for _, frame := range strings.Split(out, "\x1b[H\x1b[2J")[1:] {
		assertFitsWidth(t, frame, 80)
	}
}

// TestWatchLimits_RedirectedIsAPlainLog is the other half: --watch pointed at a
// file should be readable afterwards, not escape soup, and frames must stay
// separated once they accumulate instead of being overwritten.
func TestWatchLimits_RedirectedIsAPlainLog(t *testing.T) {
	out := twoFrames(t, 0)

	if strings.Contains(out, "\x1b[") {
		t.Errorf("emitted ANSI escapes to a non-terminal writer:\n%q", out)
	}
	if got := strings.Count(out, "every 1ms · "); got != 2 {
		t.Errorf("got %d frame headers, want 2:\n%s", got, out)
	}
	// Unknown width keeps the full seven-column table, exactly as a one-shot
	// redirect does.
	if got := strings.Count(out, "REMAINING  RESETS"); got != 2 {
		t.Errorf("redirected frames should keep the wide table:\n%s", out)
	}
	// A blank line before every frame but the first. Within a frame the header
	// is followed by the blank line, never preceded by one, so this counts
	// separators and nothing else.
	if got := strings.Count(out, "\n\nevery 1ms"); got != 1 {
		t.Errorf("got %d frame separators, want 1 (one fewer than the frames):\n%s", got, out)
	}
}

// TestWatchLimits_SkipsFrameWhenCancelledMidProbe keeps Ctrl-C from painting a
// frame over the shell prompt that is already coming back.
func TestWatchLimits_SkipsFrameWhenCancelledMidProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	err := watchLimits(ctx, &buf, time.Hour, time.UTC, func() int { return 80 },
		func(context.Context, time.Time) limitsReport {
			cancel()
			return limitsFixture()
		})
	if err != nil {
		t.Fatalf("watchLimits: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("painted a frame after cancellation:\n%s", buf.String())
	}
}

func TestDoLimits_WatchRejectsJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := doLimits([]string{"--watch", "300", "--json"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected --watch with --json to be rejected")
	}
	if !strings.Contains(err.Error(), "--watch cannot be combined with --json") {
		t.Errorf("unexpected error: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("rejected invocation still wrote to stdout: %q", stdout.String())
	}
}
