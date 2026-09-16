// Package contentlog provides an opt-in JSON Lines sink for full LLM request
// and response bodies.
//
// It is OFF by default. The gateway's standard audit log (store.RequestLog)
// records only metadata — model, token counts, cost, latency — and never the
// prompts or completions. content_log is for the cases where you do need the
// raw exchange: debugging a provider translation, reviewing prompts, or
// reproducing a bad response.
//
// Privacy: every record contains raw request and response content. Restrict
// the file's permissions and retention accordingly — nothing here redacts or
// truncates content.
//
// Rotation. Agentic clients resend the whole conversation every turn, so the
// file grows fast; left unbounded it once filled the agent-model disk.
// Open accepts Options that rotate the active file by size and/or age, gzip the
// rotated files, and prune them by count, age, or total bytes. Rotation happens
// under the same lock that serializes writes, so an in-flight line is never
// split or lost. With a zero Options the logger is a single ever-growing file,
// as before.
package contentlog

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/herospath"
)

// Record is one logged request/response exchange. It is deliberately minimal:
// correlate to the metadata audit log out of band. RequestID is the per-request
// id stamped by the chi RequestID middleware.
type Record struct {
	Timestamp time.Time       `json:"timestamp"`
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
}

// backupTimeLayout stamps rotated files. It is colon-free (Windows-safe) and
// lexically sortable, so a plain filename sort is also chronological. A literal
// "Z" is appended after formatting (and trimmed before parsing) to mark UTC
// without relying on time's ambiguous lone-"Z" zone handling.
const backupTimeLayout = "20060102T150405.000000000"

// RotateEvent describes a completed rotation. DiskBytes is the content log's
// total on-disk footprint (active file plus surviving backups) measured after
// retention pruning — a monitoring signal for abnormal growth.
type RotateEvent struct {
	Backup    string // path of the rotated file (with .gz when compressed)
	DiskBytes int64
}

// Options configures rotation, compression, and retention. The zero value
// disables all of them: Open then behaves as it always did, appending to one
// unbounded file. Sizes are bytes; durations are absolute.
type Options struct {
	// MaxSizeBytes rotates the active file when the next line would push it past
	// this size. 0 disables size-based rotation.
	MaxSizeBytes int64
	// RotateEvery rotates the active file once it has been open (since Open or
	// the last rotation) at least this long and holds at least one line. 0
	// disables time-based rotation.
	RotateEvery time.Duration
	// Compress gzips each rotated file (adding a .gz suffix) after it is renamed
	// aside. The active file is never compressed.
	Compress bool

	// MaxBackups keeps at most this many rotated files, deleting the oldest
	// beyond it. 0 means unlimited.
	MaxBackups int
	// MaxAge deletes rotated files older than this. 0 means no age limit.
	MaxAge time.Duration
	// MaxTotalBytes deletes the oldest rotated files until the total footprint
	// (active + backups) is at or below this. The active file is never deleted,
	// so a single active file larger than the limit is left in place. 0 means no
	// total-size limit.
	MaxTotalBytes int64

	// OnRotate, if set, is called after each successful rotation (post-retention)
	// with the resulting disk footprint. Use it to record a success metric and
	// refresh a disk-usage gauge.
	OnRotate func(RotateEvent)
	// OnError, if set, is called when a rotation, compression, or retention step
	// fails. The logger keeps writing regardless — content logging must never
	// fail a request — so this is the hook for a failure alert.
	OnError func(error)
}

// Logger appends Records to a JSON Lines file, one compact JSON object per
// line. Writes are serialized so concurrent requests cannot interleave bytes
// within a line. A nil *Logger is the disabled state: every method is a no-op,
// so callers never have to branch on whether logging is configured.
type Logger struct {
	mu   sync.Mutex
	f    *os.File
	path string
	opts Options

	size      int64     // bytes written to the active file
	rotatedAt time.Time // when the active file was opened / last rotated
	now       func() time.Time
}

// Open opens (creating if needed, appending if present) the JSONL file at path,
// with no rotation. It is Options-less shorthand for OpenWithOptions(path,
// Options{}).
func Open(path string) (*Logger, error) {
	return OpenWithOptions(path, Options{})
}

// OpenWithOptions opens the JSONL file at path with the given rotation,
// compression, and retention policy. An empty or whitespace path returns
// (nil, nil): content logging disabled. The file is 0600 since it holds raw
// prompt/response content — enforced on an already-existing file too, not only
// at creation (see openActive), and an open that cannot reach 0600 fails rather
// than proceeding.
func OpenWithOptions(path string, opts Options) (*Logger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	f, size, err := openActive(path)
	if err != nil {
		return nil, fmt.Errorf("contentlog: open %q: %w", path, err)
	}
	l := &Logger{f: f, path: path, opts: opts, now: time.Now, size: size}
	l.rotatedAt = l.now()
	l.tightenBackups()
	return l, nil
}

// tightenBackups forces the rotated files to 0600 as well.
//
// openActive covers the active file, but a rotated backup holds exactly the
// same raw prompts and completions and nothing else ever revisits it —
// enforceRetention only deletes. So a backup left 0644 by an external rotator,
// a restore from an archive, or an older service version remains exposed
// unless its mode is explicitly tightened.
//
// Best-effort rather than fatal, unlike the active file: a backup we cannot
// chmod is content already exposed, and refusing to start protects nothing
// that is still to be written. The failure goes to OnError, the same channel
// compression and retention failures use.
func (l *Logger) tightenBackups() {
	backups, err := l.listBackups()
	if err != nil {
		l.reportError(fmt.Errorf("contentlog: list backups to secure: %w", err))
		return
	}
	for _, b := range backups {
		if err := os.Chmod(b.path, 0o600); err != nil {
			l.reportError(fmt.Errorf("contentlog: secure backup %q: %w", b.path, err))
		}
	}
}

// openActive opens the active log for appending and guarantees it is 0600,
// returning its current size.
//
// The rule lives in herospath.OpenSecureAppend; see there for why an existing
// file has to be chmod-ed, why a mode we cannot set is fatal, and which other
// openers have hit the same trap.
func openActive(path string) (*os.File, int64, error) {
	return herospath.OpenSecureAppend(path)
}

// Enabled reports whether content logging is on. Guard expensive work (e.g.
// reconstructing a streamed response) behind this before building a Record.
func (l *Logger) Enabled() bool { return l != nil }

// Log appends one record as a single JSON line. The Timestamp is stamped here
// when zero. A nil receiver is a no-op. The returned error is for the caller to
// log-and-ignore — content logging must never fail a request. Rotation, if the
// policy calls for it, happens inline before the write under the same lock, so
// no line is ever split across files.
func (l *Logger) Log(rec Record) error {
	if l == nil {
		return nil
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = l.now().UTC()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("contentlog: marshal: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rotateNeeded(len(line)) {
		if err := l.rotate(); err != nil {
			// Report and keep going: a rotation failure must not drop the record.
			l.reportError(err)
		}
	}
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("contentlog: write: %w", err)
	}
	l.size += int64(len(line))
	return nil
}

// rotateNeeded reports whether the active file should rotate before appending a
// line of incoming bytes. Caller holds l.mu. An empty active file never rotates,
// so a fresh file is not immediately re-rotated by a time policy.
func (l *Logger) rotateNeeded(incoming int) bool {
	if l.size == 0 {
		return false // never rotate an empty file; a disabled policy also falls through the checks below
	}
	if l.opts.MaxSizeBytes > 0 && l.size+int64(incoming) > l.opts.MaxSizeBytes {
		return true
	}
	if l.opts.RotateEvery > 0 && l.now().Sub(l.rotatedAt) >= l.opts.RotateEvery {
		return true
	}
	return false
}

// rotate closes the active file, renames it aside with a timestamped name,
// reopens a fresh active file, then (best-effort) compresses the rotated file
// and enforces retention. Caller holds l.mu. Compression and retention touch
// only the renamed-aside file and sibling backups, never the reopened active
// file, so doing them here keeps the whole operation atomic w.r.t. writers
// without leaving the active file closed for their duration.
func (l *Logger) rotate() error {
	if err := l.f.Close(); err != nil {
		// Reopen so subsequent writes still land somewhere.
		_ = l.reopen()
		return fmt.Errorf("contentlog: rotate close: %w", err)
	}
	backup := l.uniqueBackupName()
	if err := os.Rename(l.path, backup); err != nil {
		_ = l.reopen()
		return fmt.Errorf("contentlog: rotate rename: %w", err)
	}
	if err := l.reopen(); err != nil {
		return err
	}

	final := backup
	if l.opts.Compress {
		if gz, err := compress(backup); err != nil {
			l.reportError(fmt.Errorf("contentlog: compress %q: %w", backup, err))
		} else {
			final = gz
		}
	}
	disk, err := l.enforceRetention()
	if err != nil {
		l.reportError(err)
	}
	if l.opts.OnRotate != nil {
		l.opts.OnRotate(RotateEvent{Backup: final, DiskBytes: disk})
	}
	return nil
}

// uniqueBackupName builds the rotated-file path, disambiguating the rare case
// where a same-timestamp backup already exists (e.g. two rotations within one
// nanosecond, or a fake clock in tests).
func (l *Logger) uniqueBackupName() string {
	base := l.path + "." + l.now().UTC().Format(backupTimeLayout) + "Z"
	candidate := base
	for i := 1; !free(candidate); i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return candidate
}

// free reports whether neither name nor its compressed twin already exists.
func free(name string) bool {
	if _, err := os.Stat(name); err == nil {
		return false
	}
	if _, err := os.Stat(name + ".gz"); err == nil {
		return false
	}
	return true
}

// reopen (re)opens the active file at l.path and resyncs the size/age trackers
// from its current state. Caller holds l.mu.
func (l *Logger) reopen() error {
	f, size, err := openActive(l.path)
	if err != nil {
		return fmt.Errorf("contentlog: rotate reopen: %w", err)
	}
	l.f = f
	l.size = size
	l.rotatedAt = l.now()
	return nil
}

// compress gzips src to src+".gz" and removes src on success, returning the .gz
// path. On failure the partial .gz is removed and src is left intact.
func compress(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	dst := src + ".gz"
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		os.Remove(dst)
		return "", err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(dst)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return "", err
	}
	in.Close()
	if err := os.Remove(src); err != nil {
		return dst, err // .gz is written; leaving src is the safer failure
	}
	return dst, nil
}

// backupInfo is one rotated file with the metadata retention needs.
type backupInfo struct {
	path string
	size int64
	mod  time.Time // rotation time, parsed from the name
}

// listBackups returns the rotated files for this log, oldest first. Caller holds
// l.mu.
func (l *Logger) listBackups() ([]backupInfo, error) {
	// ReadDir, not Glob. l.path is operator-supplied config, and interpolating
	// it into a glob pattern means a directory whose name contains a
	// metacharacter enumerates somewhere else entirely: "/logs/agent[1]" reads
	// as the character class [1], so the pattern matched a sibling
	// "/logs/agent1" while never seeing this log's own backups — retention then
	// silently pruned nothing and the log grew without bound.
	//
	// It also leaves backupTime as the single authority on what belongs to this
	// log, instead of splitting that decision between a pattern and a parser.
	dir := filepath.Dir(l.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]backupInfo, 0, len(entries))
	for _, e := range entries {
		// Identity first: it is pure string work, and it rejects the active file
		// itself along with every neighbour, so a directory an external rotator
		// also writes into does not cost a stat per archive to skip.
		full := filepath.Join(dir, e.Name())
		mod, ok := l.backupTime(full)
		if !ok {
			continue // not one of ours — see backupTime
		}
		fi, err := e.Info()
		if err != nil || fi.IsDir() {
			continue
		}
		out = append(out, backupInfo{path: full, size: fi.Size(), mod: mod})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.Before(out[j].mod) })
	return out, nil
}

// backupTime reports whether path is one of THIS log's rotated files and, if so,
// the rotation instant its name encodes.
//
// The boolean is the point. The glob in listBackups matches on path prefix
// alone, so it also picks up whatever else happens to sit beside the active log
// — an operator's hand-made copy, an external logrotate's .1/.2.gz — and every
// caller of this is deciding what retention may os.Remove. Treating an
// unparseable name as a backup dated by its mtime (which is what this used to
// do) hands those files to the pruner, in the one directory where unredacted
// prompts and completions live. Anything we cannot positively identify
// as our own is left alone.
//
// Accepted shape, exactly what uniqueBackupName and compress produce:
// <path>.<backupTimeLayout>Z, optionally "-N", optionally ".gz".
func (l *Logger) backupTime(path string) (time.Time, bool) {
	name, ok := strings.CutPrefix(path, l.path+".")
	if !ok {
		return time.Time{}, false
	}
	name = strings.TrimSuffix(name, ".gz")
	// The "-N" uniqueness counter is the only '-' our names can carry; the
	// layout has none. ParseUint is the check that matches what uniqueBackupName
	// writes: it takes unsigned decimal only, so a foreign "…-backup" cannot
	// truncate its way down to a parseable prefix, and unlike Atoi it will not
	// accept a signed "…-+1" either.
	if i := strings.LastIndexByte(name, '-'); i >= 0 {
		if _, err := strconv.ParseUint(name[i+1:], 10, 64); err != nil {
			return time.Time{}, false
		}
		name = name[:i]
	}
	// Rotation always appends the literal UTC marker, so a name without it did
	// not come from us.
	stamp, ok := strings.CutSuffix(name, "Z")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(backupTimeLayout, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// enforceRetention prunes rotated files per the count, age, and total-size
// policies, returning the resulting disk footprint (active + surviving backups).
// Caller holds l.mu.
func (l *Logger) enforceRetention() (int64, error) {
	backups, err := l.listBackups()
	if err != nil {
		return l.size, err
	}
	var errs []error
	remove := func(b backupInfo) {
		if e := os.Remove(b.path); e != nil {
			errs = append(errs, e)
		}
	}

	// 1. Age: drop anything older than MaxAge.
	survivors := backups
	if l.opts.MaxAge > 0 {
		cutoff := l.now().Add(-l.opts.MaxAge)
		kept := survivors[:0]
		for _, b := range backups {
			if b.mod.Before(cutoff) {
				remove(b)
			} else {
				kept = append(kept, b)
			}
		}
		survivors = kept
	}

	// 2. Count: keep only the newest MaxBackups (survivors are oldest first).
	if l.opts.MaxBackups > 0 && len(survivors) > l.opts.MaxBackups {
		excess := len(survivors) - l.opts.MaxBackups
		for _, b := range survivors[:excess] {
			remove(b)
		}
		survivors = survivors[excess:]
	}

	// 3. Total bytes: drop the oldest until active+backups fits the budget.
	if l.opts.MaxTotalBytes > 0 {
		total := footprint(l.size, survivors)
		i := 0
		for total > l.opts.MaxTotalBytes && i < len(survivors) {
			remove(survivors[i])
			total -= survivors[i].size
			i++
		}
		survivors = survivors[i:]
	}

	disk := footprint(l.size, survivors)
	if len(errs) > 0 {
		return disk, fmt.Errorf("contentlog: retention: %w", errors.Join(errs...))
	}
	return disk, nil
}

// footprint sums the active-file size and the sizes of the given backups.
func footprint(active int64, backups []backupInfo) int64 {
	total := active
	for _, b := range backups {
		total += b.size
	}
	return total
}

// DiskUsage returns the content log's total on-disk footprint (active file plus
// all rotated backups). A nil receiver returns 0. Intended for a periodic
// monitoring gauge so abnormal growth is visible between rotations.
func (l *Logger) DiskUsage() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	backups, _ := l.listBackups()
	return footprint(l.size, backups)
}

func (l *Logger) reportError(err error) {
	if l.opts.OnError != nil {
		l.opts.OnError(err)
	}
}

// Close closes the underlying file. A nil receiver is a no-op.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
