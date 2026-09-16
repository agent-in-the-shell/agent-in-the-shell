package agentregistry

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/procexit"
	"github.com/agent-in-the-shell/agent-in-the-shell/internal/procgroup"
	"github.com/google/uuid"
)

// stageKillGrace is how long a signalled stage's process group has to exit on
// SIGTERM before the group is SIGKILLed — the same escalation ladder the
// agentsched runner uses, so a Ctrl-C at a multi-stage script stops the stage
// and its whole subtree, not just the leader.
const stageKillGrace = 2 * time.Second

// RunStage runs one command as an observed pipeline STAGE: it registers a
// live process-registry entry for the run's duration (so `agent-shell ps`
// shows the stage) and writes a durable CompletionRecord at the end (so
// `agent-shell logs <id>` has its per-stage exit + output). This is the
// axis-8 wedge — per-stage observability over a hand-written multi-stage
// shell script, WITHOUT a workflow interpreter: each stage is still just a
// process in a `.sh`, but now an individually-recorded one.
//
// Usage: agent-shell stage <name> [--task ID] [--tail-bytes N] -- <cmd> [args...]
//
// Output streams through live (stdout/stderr are tee'd) so the stage works
// inside a pipe; a bounded tail of each is retained for the record. The
// child runs in its own process group and inherits the environment plus
// AGENT_RUN_ID=<this stage's id>, so a nested `agent run` links under the
// stage in `agent-shell ps --tree`. The stage's own exit code is propagated,
// so `set -e` in the wrapping script still short-circuits on a failed stage.
func RunStage(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name, cmdArgs, taskID, tailBytes, err := parseStageArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "agent-shell stage: %v\n", err)
		return 2
	}

	// A stage links to the enclosing run (the script itself, or an outer
	// stage) via AGENT_RUN_ID, exactly like agent-shell/agentsched runs.
	parentID := os.Getenv("AGENT_RUN_ID")
	id := uuid.NewString()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	procgroup.NewGroup(cmd) // own process-group leader => PGID == PID
	// On ctx cancel (a forwarded Ctrl-C/SIGTERM) terminate the whole group,
	// then let WaitDelay force cmd.Wait to return even if a grandchild
	// setsid'd out of the group and holds the pipes open — the same backstop
	// agentsched's runner uses, so an escaped child can't hang the script.
	cmd.Cancel = func() error { return procgroup.Signal(cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = stageKillGrace + 5*time.Second
	cmd.Stdin = stdin
	outTail := NewTailWriter(tailBytes)
	errTail := NewTailWriter(tailBytes)
	// The tee to the caller's stdout/stderr is best-effort: swallow a
	// downstream write error (a closed pipe, e.g. `stage ... | head`) so it
	// never propagates into cmd.Wait as a spurious stage failure and never
	// stops the bounded tail from capturing the run's output.
	cmd.Stdout = io.MultiWriter(teeSwallow{stdout}, outTail)
	cmd.Stderr = io.MultiWriter(teeSwallow{stderr}, errTail)
	cmd.Env = append(os.Environ(), "AGENT_RUN_ID="+id)

	started := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		// The stage never became a process, so there is nothing to register or
		// record; report and use the shell exit conventions: 127 for a
		// not-found command, 126 for a found-but-not-executable one.
		fmt.Fprintf(stderr, "agent-shell stage %q: %v\n", name, err)
		if errors.Is(err, exec.ErrNotFound) {
			return 127
		}
		return 126
	}

	displayCmd := name + ": " + strings.Join(cmdArgs, " ")
	registered := false
	if h, regErr := Register(Entry{
		ID:        id,
		ParentID:  parentID,
		TaskID:    taskID,
		PID:       cmd.Process.Pid,
		PGID:      cmd.Process.Pid,
		Command:   displayCmd,
		Backend:   "agent-stage",
		StartedAt: started,
	}); regErr == nil {
		registered = true
		defer h.Close()
	}

	// cmd.Cancel SIGTERMs the group on ctx cancel (the leader-only WaitDelay
	// path would leave grandchildren alive); escalate to a group-wide SIGKILL
	// after the grace period if the SIGTERM didn't take.
	stopEsc := make(chan struct{})
	defer close(stopEsc)
	go func() {
		select {
		case <-ctx.Done():
		case <-stopEsc:
			return
		}
		select {
		case <-time.After(stageKillGrace):
			_ = procgroup.Signal(cmd.Process.Pid, syscall.SIGKILL)
		case <-stopEsc:
		}
	}()

	runErr := cmd.Wait()
	finished := time.Now().UTC()
	d := procexit.Decode(runErr)

	// Let the wait outcome speak for itself — a forwarded signal that the
	// child caught and exited 0 through is honestly "ok", not "signaled"
	// (mirrors agentsched, which only overrides status for a deadline). The
	// interruption is recorded as a note on stderr, never by corrupting the
	// status/signal invariant (Signal is nonzero only when Status=="signaled").
	status := "ok"
	switch {
	case d.Signaled:
		status = "signaled"
	case !d.Known:
		status = "error"
	}
	stderrTail := errTail.String()
	if ctx.Err() != nil {
		stderrTail += "\n[stage interrupted: " + ctx.Err().Error() + "]"
	}

	if registered {
		_ = RecordCompletion(CompletionRecord{
			ID:         id,
			ParentID:   parentID,
			TaskID:     taskID,
			Command:    displayCmd,
			Backend:    "agent-stage",
			StartedAt:  started,
			FinishedAt: finished,
			ExitCode:   d.ExitCode,
			Signal:     d.Signal,
			Status:     status,
			Stdout:     outTail.String(),
			Stderr:     stderrTail,
		})
	}
	return d.ExitCode
}

// teeSwallow wraps a writer so a downstream error (e.g. a closed pipe) is
// swallowed rather than propagated — used for the best-effort live tee, where
// the durable record, not the terminal echo, is the source of truth.
type teeSwallow struct{ w io.Writer }

func (t teeSwallow) Write(p []byte) (int, error) {
	_, _ = t.w.Write(p)
	return len(p), nil
}

// parseStageArgs splits `<name> [flags] -- <cmd...>`. The `--` is required so
// the stage command's own flags are never consumed as stage flags.
func parseStageArgs(args []string) (name string, cmdArgs []string, taskID string, tailBytes int, err error) {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return "", nil, "", 0, fmt.Errorf("usage: agent-shell stage <name> [--task ID] [--tail-bytes N] -- <cmd> [args...]")
	}
	head, cmdArgs := args[:sep], args[sep+1:]
	if len(cmdArgs) == 0 {
		return "", nil, "", 0, fmt.Errorf("no command after --")
	}
	if len(head) == 0 {
		return "", nil, "", 0, fmt.Errorf("a stage name is required before the flags/--")
	}
	name = head[0]

	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	task := fs.String("task", os.Getenv("AGENT_TASK_ID"), "task id this stage belongs to")
	tb := fs.Int("tail-bytes", MaxCompletionOutputBytes, "bytes of stdout/stderr tail to retain in the record")
	if err := fs.Parse(head[1:]); err != nil {
		return "", nil, "", 0, err
	}
	if *tb <= 0 {
		*tb = MaxCompletionOutputBytes
	}
	return name, cmdArgs, *task, *tb, nil
}
