package main

// `agent-model limits` — subscription quota for the accounts this process holds
// credentials for. Migrated from `agent-shell limits`: agent-shell execs vendor
// CLIs and holds no credentials, so reporting an account's quota never belonged
// there. The renderer below started as a port of internal/shellcli/cli.go; the
// seven-column table it emits is still that layout byte for byte, but it has
// since grown the narrow and clipped layouts a terminal too small for it gets,
// so it is no longer a behavior-preserving copy to diff against.
//
// Unlike the agent-shell version this runs IN-PROCESS — it needs no running
// gateway, because the process rendering the table is the one that owns the
// credential.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// limitRow is the subset of a window the renderer reads. api returns its own
// wire type; this keeps the formatters independent of that shape.
type limitRow struct {
	LimitName        string
	UsedPercent      *float64
	Remaining        *float64
	ResetAt          *time.Time
	ResetDescription string
	Source           string
	RawMessage       string
	Status           string
}

type limitsBackend struct {
	Agent   string     `json:"agent"`
	Profile string     `json:"profile,omitempty"`
	Status  string     `json:"status"`
	Windows []limitRow `json:"windows"`
	Error   string     `json:"error,omitempty"`
}

type limitsReport struct {
	SchemaVersion int             `json:"schema_version"`
	CheckedAt     time.Time       `json:"checked_at"`
	Backends      []limitsBackend `json:"backends"`
}

// limitsSchemaVersion stays at 2, the value agent-shell shipped: the row shape
// and (agent, profile) cardinality are unchanged by the move, so a consumer
// keyed on v2 keeps working against the new producer.
const limitsSchemaVersion = 2

func doLimits(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("limits", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	profile := fs.String("profile", "", "Anthropic profile to probe")
	profiles := fs.String("profiles", "", `"all" expands every discovered Anthropic profile`)
	watchSpec := fs.String("watch", "", "redraw every interval instead of exiting (e.g. 300 or 5m)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile != "" && *profiles != "" {
		return fmt.Errorf("--profile cannot be combined with --profiles")
	}
	if *profiles != "" && *profiles != "all" {
		return fmt.Errorf("unsupported --profiles value %q", *profiles)
	}
	// --watch has no defensible JSON contract: a stream of documents, a growing
	// array, and a screen-cleared redraw are all plausible readings, and picking
	// one silently is the kind of guess this codebase avoids. A shell loop
	// around --json covers that case and loses nothing, because the width bug
	// --watch exists to fix only affects the table.
	if *watchSpec != "" && *jsonOut {
		return fmt.Errorf("--watch cannot be combined with --json (loop the shell around --json instead)")
	}

	selected := map[string]bool{"claude": true, "codex": true}
	if rest := fs.Args(); len(rest) > 0 {
		if len(rest) > 1 {
			return fmt.Errorf("limits accepts at most one backend (got %v)", rest)
		}
		selected = map[string]bool{rest[0]: true}
		if !selected["claude"] && !selected["codex"] {
			return fmt.Errorf("unsupported backend %q (supported: claude, codex)", rest[0])
		}
	}

	// Profile discovery lives inside the closure, not above it, so a login that
	// happens while --watch is running shows up on the next frame.
	collect := func(ctx context.Context, now time.Time) limitsReport {
		report := limitsReport{SchemaVersion: limitsSchemaVersion, CheckedAt: now}
		if selected["claude"] {
			names := []string{*profile}
			if *profiles == "all" {
				discovered, err := auth.ListProfiles()
				if err == nil && len(discovered) > 0 {
					names = discovered
				}
			}
			for _, p := range names {
				report.Backends = append(report.Backends, toBackend("claude", p, api.FetchAnthropicUsage(ctx, p, now)))
			}
		}
		if selected["codex"] {
			report.Backends = append(report.Backends, toBackend("codex", "", api.FetchOpenAIUsage(ctx, now)))
		}
		return report
	}

	if *watchSpec != "" {
		interval, err := parseWatchInterval(*watchSpec)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return watchLimits(ctx, stdout, interval, time.Local,
			func() int { return displayWidth(stdout) }, collect)
	}

	now := time.Now().UTC()
	report := collect(context.Background(), now)
	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderLimits(stdout, report, now, time.Local, displayWidth(stdout))
	return nil
}

// minWatchInterval floors --watch. Every tick is a live authenticated probe per
// profile — and `auth.NewAnthropicOAuthRefreshableForProfile` may rotate a
// single-use refresh token on the way — while the windows being reported move
// on 5-hour and weekly timescales. Below a few seconds the poll buys no new
// information and spends upstream calls, so this is a floor on waste, not a
// safety limit.
const minWatchInterval = 5 * time.Second

// parseWatchInterval accepts both a bare second count ("300", the spelling
// watch(1) -n takes) and a Go duration ("5m"). The flag exists to replace a
// `watch -n 300` habit, so rejecting the number the user already had in their
// shell history would be friction for no gain.
func parseWatchInterval(spec string) (time.Duration, error) {
	var d time.Duration
	if secs, err := strconv.Atoi(spec); err == nil {
		d = time.Duration(secs) * time.Second
	} else {
		parsed, err := time.ParseDuration(spec)
		if err != nil {
			return 0, fmt.Errorf("invalid --watch interval %q (want seconds like 300, or a duration like 5m)", spec)
		}
		d = parsed
	}
	if d < minWatchInterval {
		return 0, fmt.Errorf("--watch interval %s is below the %s minimum", d, minWatchInterval)
	}
	return d, nil
}

// watchLimits redraws the report every interval until ctx is cancelled.
//
// It exists because `watch -n 300 'agent-model limits --profiles all'` cannot
// render this table. The full seven-column layout is ~125 columns wide, so at a
// normal 80-column terminal watch folds every row onto two lines and adjacent
// rows interleave — and it does so intermittently, because the RESETS cell
// changes width as countdowns roll over ("14:30 CST" vs "Aug 19 09:33 CST").
// Holding the loop in-process fixes more than the folding: the width is re-read
// per frame so a resize is honoured, the layout degrades instead of wrapping,
// and one process owns the OAuth probe rather than a fresh exec per tick.
//
// The cadence is measured from the start of each probe, matching `watch -p`: a
// probe slower than the interval fires the next frame immediately rather than
// letting the schedule drift by its own latency, which keeps the "next" stamp
// in the header truthful.
//
// termWidth and collect are both injected rather than called directly, so a
// test can drive either terminal branch — the ANSI redraw is the whole point of
// the flag and would otherwise be reachable only from a real tty.
func watchLimits(ctx context.Context, stdout io.Writer, interval time.Duration, loc *time.Location, termWidth func() int, collect func(context.Context, time.Time) limitsReport) error {
	for frames := 0; ; frames++ {
		start := time.Now()
		report := collect(ctx, start.UTC())
		if ctx.Err() != nil {
			return nil // interrupted mid-probe: don't paint a frame over the returning shell prompt
		}

		// Re-measured per frame, and the whole frame is buffered so the clear
		// and the redraw reach the terminal as one write — a partial frame on a
		// cleared screen is the flicker watch(1) is notorious for.
		width := termWidth()
		var frame bytes.Buffer
		switch {
		case width > 0:
			frame.WriteString("\x1b[H\x1b[2J") // cursor home + clear screen; scrollback left intact
		case frames > 0:
			frame.WriteString("\n") // redirected: frames accumulate, so separate them
		}
		fmt.Fprintf(&frame, "every %s · %s · next %s\n\n",
			formatInterval(interval),
			start.In(loc).Format("15:04:05 MST"),
			start.Add(interval).In(loc).Format("15:04:05"))
		renderLimits(&frame, report, start.UTC(), loc, width)
		if _, err := stdout.Write(frame.Bytes()); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(start.Add(interval))):
		}
	}
}

// formatInterval spells the cadence the way it was most likely typed — "5m"
// rather than time.Duration's "5m0s" — while keeping an unrounded interval
// whole ("1m30s").
func formatInterval(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		// Duration.String() already spells sub-minute values the short way
		// ("45s") and keeps unrounded ones whole ("1m30s"). Only the two cases
		// above, where it would say "5m0s", need overriding.
		return d.String()
	}
}

// displayWidth reports the column budget for w, or 0 for "unknown". A non-file
// writer (a test buffer) and a redirected file both read as unknown, which is
// what keeps piped output byte-identical to the pre-adaptation table.
func displayWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		return terminalWidth(f)
	}
	return 0
}

func toBackend(agent, profile string, resp api.AccountUsageResponse) limitsBackend {
	b := limitsBackend{Agent: agent, Profile: profile, Status: resp.Status}
	if resp.Error != nil {
		b.Error = resp.Error.Code
	}
	for _, w := range resp.Windows {
		b.Windows = append(b.Windows, limitRow{
			LimitName: w.LimitName, UsedPercent: w.UsedPercent, Remaining: w.Remaining,
			ResetAt: w.ResetAt, ResetDescription: w.ResetDescription,
			Source: w.Source, RawMessage: w.RawMessage, Status: w.WindowStatus,
		})
	}
	return b
}

// limitsRowCells is one report row with every cell already formatted, so the
// two layouts below differ only in how they arrange cells — never in what a
// cell says. cont is the narrow layout's continuation line, built here because
// only this function knows whether a row describes windows or a failed probe.
type limitsRowCells struct {
	agent, status, window, used, remaining, resets, source string
	cont                                                   string
}

// renderLimits writes the report to w, laid out for width columns. width <= 0
// means "unknown" — stdout is not a terminal — and emits the full table
// unchanged, so redirected output keeps the byte-for-byte shape scripts parse.
func renderLimits(w io.Writer, report limitsReport, now time.Time, loc *time.Location, width int) {
	rows := limitsRows(report, now, loc)
	wide := limitsWideTable(rows)
	if width <= 0 || widestLine(wide) <= width {
		_, _ = io.WriteString(w, wide)
		return
	}
	_, _ = io.WriteString(w, limitsNarrow(rows, width))
}

func limitsRows(report limitsReport, now time.Time, loc *time.Location) []limitsRowCells {
	var rows []limitsRowCells
	for _, backend := range report.Backends {
		name := backend.Agent
		if backend.Profile != "" {
			name += "/" + backend.Profile
		}
		if len(backend.Windows) == 0 {
			detail := backend.Error
			if detail == "" {
				detail = "-"
			}
			row := limitsRowCells{agent: name, status: backend.Status,
				window: "-", used: "-", remaining: "-", resets: detail, source: "-"}
			if detail != "-" {
				row.cont = detail // a bare "-" is not worth a second line
			}
			rows = append(rows, row)
			continue
		}
		for _, window := range backend.Windows {
			// Per-window status, not the backend rollup: a saturated 5h
			// window must not make the weekly row read "limited" too.
			row := limitsRowCells{
				agent: name, status: window.Status, window: formatWindowCell(window),
				used:      formatOptionalPercent(window.UsedPercent),
				remaining: formatOptionalNumber(window.Remaining),
				resets:    formatResetCell(window, now, loc), source: window.Source,
			}
			row.cont = "resets " + row.resets
			if row.source != "" {
				row.cont += " · " + row.source
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// limitsWideTable is the original seven-column layout, byte-for-byte: it is
// still what a wide terminal and every redirect get.
func limitsWideTable(rows []limitsRowCells) string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATUS\tWINDOW\tUSED\tREMAINING\tRESETS\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.agent, r.status, r.window, r.used, r.remaining, r.resets, r.source)
	}
	_ = tw.Flush()
	return buf.String()
}

// minWindowCell is how far WINDOW may be clipped before the narrow layout stops
// trying. Below this, "codex_bengalfox_weekly (GPT-5.3-Codex-Spark)" and its
// siblings all clip to the same prefix, and a column that cannot tell two rows
// apart is worth less than the overflow it was avoiding.
const minWindowCell = 16

const clipMarker = ".."

// limitsNarrow keeps the identity columns — the ones read by scanning down the
// page — on one line each, and moves RESETS and SOURCE to an indented
// continuation. Those two are the widest cells and the only ones read a row at
// a time, so they are what a narrow terminal can afford to demote. Folding
// instead (what watch(1) does) costs the row/line correspondence entirely.
func limitsNarrow(rows []limitsRowCells, width int) string {
	// Measure the real table rather than adding cell widths up: only tabwriter
	// knows its own padding rule, and re-deriving it here would be a second
	// copy to keep in sync. Re-rendering is ~6µs against a 5-second frame.
	lines := limitsNarrowTable(rows, 0)
	if over := widestOf(lines) - width; over > 0 {
		lines = limitsNarrowTable(rows, max(longestWindow(rows)-over, minWindowCell))
	}

	var buf strings.Builder
	buf.WriteString(lines[0] + "\n")
	for i, r := range rows {
		buf.WriteString(lines[i+1] + "\n")
		if r.cont != "" {
			buf.WriteString(clip("  "+r.cont, width) + "\n")
		}
	}
	return buf.String()
}

// limitsNarrowTable renders the identity columns only, clipping WINDOW to
// windowCell (0 = unclipped), and returns one line per row plus a leading
// header. The caller interleaves continuation lines afterwards rather than
// writing them through the tabwriter: a line without tabs would terminate the
// aligned block, so every row would be padded independently and the columns
// would not line up down the page.
func limitsNarrowTable(rows []limitsRowCells, windowCell int) []string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATUS\tWINDOW\tUSED\tREMAIN")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.agent, r.status, clip(r.window, windowCell), r.used, r.remaining)
	}
	_ = tw.Flush()
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// cellWidth is the layout's single definition of how wide a string prints. It
// is one rune per column, which holds for everything this table carries (ASCII
// plus the "·" separator) — teaching the layout about wide CJK glyphs or ANSI
// sequences means changing this function and nothing else.
func cellWidth(s string) int { return utf8.RuneCountInString(s) }

func longestWindow(rows []limitsRowCells) int {
	longest := 0
	for _, r := range rows {
		longest = max(longest, cellWidth(r.window))
	}
	return longest
}

func widestOf(lines []string) int {
	widest := 0
	for _, line := range lines {
		widest = max(widest, cellWidth(line))
	}
	return widest
}

func widestLine(s string) int { return widestOf(strings.Split(s, "\n")) }

// clip shortens s to at most n columns, marking the cut so a truncated cell is
// never mistaken for a short one. n <= 0 means "no limit".
func clip(s string, n int) string {
	if n <= 0 || n <= cellWidth(clipMarker) || cellWidth(s) <= n {
		return s
	}
	return string([]rune(s)[:n-cellWidth(clipMarker)]) + clipMarker
}

// formatResetCell renders the RESETS column as a hybrid of an adaptive
// countdown and a parenthesised local clock — e.g. "1h 23m (14:05 PST)". The
// countdown answers "wait or work now?" while the absolute half self-anchors
// the row so it can't go silently stale once the output is piped or scrolled
// back. now is the report's CheckedAt anchor and loc the system zone; both are
// injected so the rendering is deterministic and testable without touching $TZ.
func formatResetCell(window limitRow, now time.Time, loc *time.Location) string {
	if window.ResetDescription != "" {
		return window.ResetDescription // producer-supplied human override
	}
	if window.ResetAt == nil {
		return "-"
	}
	if !window.ResetAt.After(now) {
		return "now" // stale poll: reset is already in the past
	}
	return formatCountdown(window.ResetAt.Sub(now)) + " (" + formatResetAbsolute(*window.ResetAt, now, loc) + ")"
}

// formatCountdown renders a duration as a two-unit ASCII countdown: seconds
// below a minute ("45s"), minutes below an hour ("12m"), hours+minutes below a
// day ("1h 23m"), and days+hours beyond ("6d 23h"). Holding the unit count
// fixed per tier keeps the column from jittering row to row.
func formatCountdown(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	default:
		return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int((d%(24*time.Hour))/time.Hour))
	}
}

// formatResetAbsolute renders the reset instant in loc with a zone token,
// tiered by how far away it is from now: time-only when it lands today, a
// weekday when within the coming week (so the abbreviation stays unambiguous),
// and a full date beyond. The zone token is mandatory — "15:04" alone would be
// just as ambiguous as the UTC bug this replaces.
func formatResetAbsolute(resetAt, now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	reset := resetAt.In(loc)
	cur := now.In(loc)
	resetMidnight := time.Date(reset.Year(), reset.Month(), reset.Day(), 0, 0, 0, 0, loc)
	curMidnight := time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc)
	daysApart := int(resetMidnight.Sub(curMidnight).Round(24*time.Hour) / (24 * time.Hour))
	switch {
	case daysApart <= 0:
		return reset.Format("15:04 MST")
	case daysApart < 7:
		return reset.Format("Mon 15:04 MST")
	default:
		return reset.Format("Jan 02 15:04 MST")
	}
}

func formatOptionalPercent(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *value)
}

func formatOptionalNumber(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f", *value)
}

// formatWindowCell renders the WINDOW column. LimitName is a machine key, and
// for per-model codex rows it embeds an upstream codename ("bengalfox") that a
// reader cannot map to a model. When the probe supplied a human label, annotate
// the name with it — the same parenthesised idiom formatResetCell uses.
func formatWindowCell(window limitRow) string {
	label := strings.TrimSpace(window.RawMessage)
	if label == "" {
		return window.LimitName
	}
	return window.LimitName + " (" + label + ")"
}
