package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// doUsage implements `agent-model usage`: a read-only report over request_logs.
// It opens the DB read-only (no schema mutation, no write lock against a live
// server), aggregates one grouping dimension, and renders a table (default) or
// JSON. STDOUT carries pure data; the data-quality canary goes to STDERR in
// table mode and into a structured warnings[] array in JSON mode.
func doUsage(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to YAML config that resolves the DB path (default ~/.config/agentmodel/config.yaml)")
	dbPath := fs.String("db", "", "path to the SQLite DB (overrides --config)")
	since := fs.String("since", "7d", "lookback window: e.g. 7d, 24h, 30m")
	by := fs.String("by", "model", "group by: model|provider|api_key|auth_mode|day")
	org := fs.String("org", "default", "org/tenant id to report on")
	limit := fs.Int("limit", 100, "maximum rows to return")
	jsonOut := fs.Bool("json", false, "emit JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dim, err := store.ParseDimension(*by)
	if err != nil {
		return err
	}
	window, err := store.ParseSince(*since)
	if err != nil {
		return err
	}

	path := *dbPath
	if path == "" {
		cfg, err := agentmodel.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		path = cfg.DB
	}

	st, err := store.OpenSQLiteReadOnly(path)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	end := time.Now().UTC()
	start := end.Add(-window)

	rows, err := st.UsageReport(ctx, store.UsageFilter{
		OrgID: *org,
		Start: start,
		End:   end,
		By:    []store.UsageDimension{dim},
		Limit: *limit,
	})
	if err != nil {
		return err
	}
	health, err := st.UsageHealthReport(ctx, *org, start, end)
	if err != nil {
		return err
	}
	warnings := usageWarnings(health, dim)

	if *jsonOut {
		return renderUsageJSON(stdout, *org, *by, start, end, rows, warnings)
	}
	return renderUsageTable(stdout, stderr, *by, rows, warnings)
}

// usageWarnings turns health counts into human-readable canary lines. A healthy
// DB yields none. The active grouping dimension escalates the wording, since a
// near-degenerate `--by` is the most misleading case.
func usageWarnings(h store.UsageHealth, dim store.UsageDimension) []string {
	var w []string
	flag := func(count int64, name string, escalateDim store.UsageDimension) {
		if count == 0 {
			return
		}
		msg := fmt.Sprintf("%d/%d rows have an unset %s", count, h.Total, name)
		if dim == escalateDim {
			msg += fmt.Sprintf(" — grouping by %s is near-degenerate", escalateDim)
		}
		w = append(w, msg)
	}
	flag(h.UnsetProvider, "provider", store.DimProvider)
	flag(h.UnsetModel, "model", store.DimModel)
	flag(h.UnsetAPIKey, "api_key", store.DimAPIKey)
	if h.ZeroCostWithTokens > 0 {
		w = append(w, fmt.Sprintf("%d/%d rows have tokens but $0 cost (flat-fee subscription or an unpriced model)", h.ZeroCostWithTokens, h.Total))
	}
	return w
}

// usageJSON is the report envelope. Flat rows in a typed envelope keep adding
// totals / next_cursor / nested buckets non-breaking later.
type usageJSON struct {
	Object   string           `json:"object"`
	Org      string           `json:"org"`
	GroupBy  string           `json:"group_by"`
	Window   usageWindowJSON  `json:"window"`
	Rows     []store.UsageRow `json:"rows"`
	Warnings []string         `json:"warnings,omitempty"`
}

type usageWindowJSON struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

func renderUsageJSON(w io.Writer, org, by string, start, end time.Time, rows []store.UsageRow, warnings []string) error {
	if rows == nil {
		rows = []store.UsageRow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(usageJSON{
		Object:   "usage.report",
		Org:      org,
		GroupBy:  by,
		Window:   usageWindowJSON{Start: start.Unix(), End: end.Unix()},
		Rows:     rows,
		Warnings: warnings,
	})
}

func renderUsageTable(stdout, stderr io.Writer, by string, rows []store.UsageRow, warnings []string) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	header := strings.ToUpper(by)
	// AlignRight pads on the left, so the final column needs a trailing tab to
	// be treated as a real (padded) column instead of touching its neighbor.
	fmt.Fprintf(tw, "%s\tREQUESTS\tPROMPT\tCOMPLETION\tTOTAL\tCACHE_R\tCACHE_W\tCOST\tERROR_RATE\t\n", header)
	for _, r := range rows {
		key := r.Key
		if key == "" {
			key = "(unset)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t$%.4f\t%.1f%%\t\n",
			key,
			humanInt(r.Requests),
			humanInt(r.PromptTokens),
			humanInt(r.CompletionTokens),
			humanInt(r.TotalTokens),
			humanInt(r.CacheReadInputTokens),
			humanInt(r.CacheCreationInputTokens),
			r.CostUSD,
			r.ErrorRate*100,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(stderr, "note: no usage rows in the selected window")
	}
	for _, line := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", line)
	}
	return nil
}

// humanInt renders an int64 with thousands separators, no extra dependency.
func humanInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
