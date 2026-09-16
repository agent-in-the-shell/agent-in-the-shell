package agentregistry

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// maxTreeDepth caps --tree recursion so a bad/cyclical ParentID (e.g. two
// entries pointing at each other) can never hang the command or overflow the
// stack; no legitimate nesting comes close to this depth.
const maxTreeDepth = 100

// RunPS implements `agent-shell ps`: list every running agent on this host, each with
// an honest live/stale verdict. It mirrors the (args, ..., writers) -> int shape
// of agentrun.Run for testability.
//
// Exit codes follow ps(1): listing is not a success/fail query, so an empty
// registry and a root that cannot be read both print nothing and exit 0. A usage
// error (bad flag) exits 2; an output/encode failure writing the result exits 1.
func RunPS(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit a JSON array instead of a table")
	tree := fs.Bool("tree", false, "indent child runs (ParentID, via AGENT_RUN_ID) under their parent; table output only")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	entries, err := List()
	if err != nil {
		// Like ps(1): a registry that cannot be read lists nothing and is not a
		// failure of the query itself.
		fmt.Fprintf(stderr, "agent-shell ps: %v\n", err)
		return 0
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].StartedAt.After(entries[j].StartedAt) // newest first
	})

	if *asJSON {
		if entries == nil {
			entries = []Entry{} // emit [] not null
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			fmt.Fprintf(stderr, "agent-shell ps: encode: %v\n", err)
			return 1
		}
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPID\tPGID\tSTATUS\tSTARTED\tUPTIME\tCOMMAND")
	now := time.Now()
	if *tree {
		writeTree(tw, buildForest(entries), now, 0)
	} else {
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
				e.ID, e.PID, e.PGID, e.Status,
				e.StartedAt.Format(time.RFC3339),
				uptime(now, e.StartedAt),
				e.Command,
			)
		}
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-shell ps: %v\n", err)
		return 1
	}
	return 0
}

// treeNode groups an Entry with the children whose ParentID points at it, for
// --tree rendering.
type treeNode struct {
	entry    Entry
	children []*treeNode
}

// buildForest groups entries (assumed pre-sorted, newest first) by ParentID.
// An entry is a root when ParentID is empty, self-referential, or does not
// match any entry in this same List() call (the parent already exited, or was
// never a registered run) — so a dangling ParentID degrades to "just show it
// at the top level" rather than dropping the entry.
func buildForest(entries []Entry) []*treeNode {
	byID := make(map[string]*treeNode, len(entries))
	nodes := make([]*treeNode, len(entries))
	for i, e := range entries {
		n := &treeNode{entry: e}
		nodes[i] = n
		byID[e.ID] = n
	}
	var roots []*treeNode
	for i, e := range entries {
		n := nodes[i]
		parent, ok := byID[e.ParentID]
		if e.ParentID == "" || !ok || parent == n {
			roots = append(roots, n)
			continue
		}
		parent.children = append(parent.children, n)
	}
	return roots
}

// writeTree renders nodes depth-first, indenting the ID column two spaces per
// level. depth is capped at maxTreeDepth as a cheap guard against a
// pathological/cyclical ParentID graph.
func writeTree(tw *tabwriter.Writer, nodes []*treeNode, now time.Time, depth int) {
	if depth > maxTreeDepth {
		return
	}
	for _, n := range nodes {
		e := n.entry
		fmt.Fprintf(tw, "%s%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			strings.Repeat("  ", depth), e.ID, e.PID, e.PGID, e.Status,
			e.StartedAt.Format(time.RFC3339),
			uptime(now, e.StartedAt),
			e.Command,
		)
		writeTree(tw, n.children, now, depth+1)
	}
}

// RunKill implements `agent-shell kill <id> [--grace d]`: reap a running agent's whole
// process group. Unlike ps, kill is a success/fail query (like kill(1)): 0 on
// success, 1 on failure (not found / stale / signal error), 2 on usage error.
func RunKill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	grace := fs.Duration("grace", defaultKillGrace, "how long to wait after SIGTERM before SIGKILL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agent-shell kill <id> [--grace d]")
		return 2
	}
	id := fs.Arg(0)
	if err := Kill(id, *grace); err != nil {
		fmt.Fprintf(stderr, "agent-shell kill: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "killed %s\n", id)
	return 0
}

// RunSignal implements `agent-shell signal <id> <SIG>`: send one signal to a running
// agent's process group. Same exit-code scheme as kill.
func RunSignal(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("signal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: agent-shell signal <id> <SIG>   (e.g. TERM, HUP, USR1, 15)")
		return 2
	}
	sig, err := parseSignal(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "agent-shell signal: %v\n", err)
		return 2
	}
	if err := Signal(fs.Arg(0), sig); err != nil {
		fmt.Fprintf(stderr, "agent-shell signal: %v\n", err)
		return 1
	}
	return 0
}

// waitPollInterval is how often RunWait re-checks for a completion record.
const waitPollInterval = 250 * time.Millisecond

// RunWait implements `agent-shell wait <id> [--timeout d]`: block until id's
// run finishes (its CompletionRecord appears — written by the producer before
// the live Entry is removed, so the two never race), then print its recorded
// exit status and exit with that same code, mirroring shell `wait $pid`
// semantics. Exits 124 on --timeout expiry (the timeout(1) convention); exits 1
// if id is neither running nor has a completion (unknown id, or reaped via
// `agent-shell kill`, which does not record a completion); exits 2 on usage error.
func RunWait(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 0, "give up waiting after this long (0 = wait forever)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agent-shell wait [--timeout d] <id>")
		return 2
	}
	id := fs.Arg(0)

	var deadline <-chan time.Time
	if *timeout > 0 {
		t := time.NewTimer(*timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		rec, ok, err := ReadCompletion(id)
		if err != nil {
			fmt.Fprintf(stderr, "agent-shell wait: %v\n", err)
			return 1
		}
		if ok {
			fmt.Fprintf(stdout, "%s exited: status=%s exit_code=%d signal=%d\n", id, rec.Status, rec.ExitCode, rec.Signal)
			return rec.ExitCode
		}
		if _, err := lookup(id); err != nil {
			fmt.Fprintf(stderr, "agent-shell wait: %s is not running and has no completion record (%v)\n", id, err)
			return 1
		}
		select {
		case <-deadline:
			fmt.Fprintf(stderr, "agent-shell wait: timed out waiting for %s\n", id)
			return 124
		case <-time.After(waitPollInterval):
		}
	}
}

// RunLogs implements `agent-shell logs <id>`: print the captured stdout/stderr
// of a finished run, read from its durable CompletionRecord — the same record
// `agent-shell wait` polls for, uniform across both live producers (agent-shell
// interactive submit, agent-sched scheduled runs). Exits 1 if the run hasn't
// finished yet (or has no completion record); exits 2 on usage error.
func RunLogs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agent-shell logs <id>")
		return 2
	}
	id := fs.Arg(0)

	rec, ok, err := ReadCompletion(id)
	if err != nil {
		fmt.Fprintf(stderr, "agent-shell logs: %v\n", err)
		return 1
	}
	if !ok {
		if _, lerr := lookup(id); lerr == nil {
			fmt.Fprintf(stderr, "agent-shell logs: %s is still running; logs are available once it finishes\n", id)
		} else {
			fmt.Fprintf(stderr, "agent-shell logs: no completion record for %s\n", id)
		}
		return 1
	}
	fmt.Fprint(stdout, rec.Stdout)
	if rec.Stderr != "" {
		fmt.Fprintf(stderr, "--- stderr ---\n%s", rec.Stderr)
	}
	return 0
}

// signalNames (the supported name->number table) is defined per-platform, since
// the Windows syscall package lacks SIGUSR1/USR2/STOP/CONT.

// parseSignal accepts a name ("TERM"), a "SIG"-prefixed name ("SIGTERM"), or a
// bare number ("15").
func parseSignal(s string) (syscall.Signal, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		return syscall.Signal(n), nil
	}
	s = strings.TrimPrefix(s, "SIG")
	if sig, ok := signalNames[s]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q (try TERM, KILL, HUP, INT, USR1, USR2, or a number)", s)
}

// uptime renders a human duration since started, rounded to the second.
func uptime(now, started time.Time) string {
	if started.IsZero() {
		return "-"
	}
	d := now.Sub(started)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
