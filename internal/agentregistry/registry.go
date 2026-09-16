package agentregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const metaFile = "meta.json"

// beforeReadEntry is a test-only seam, called with each subdir name at the top
// of List's per-entry read loop. It is nil (a no-op) in production; tests set it
// to inject a deterministic vanish-mid-List race.
var beforeReadEntry func(name string)

// Handle owns one registered entry's lifetime. Close removes the entry's
// directory and is idempotent and safe to defer (including on panic).
type Handle struct{ dir string }

// Close removes the entry directory. A missing directory is not an error, so
// Close is idempotent.
func (h *Handle) Close() error {
	if h == nil || h.dir == "" {
		return nil
	}
	return os.RemoveAll(h.dir)
}

// Register records a running agent and returns a Handle whose Close removes the
// record. A nil error means meta.json is durably on disk.
//
// Register fills e.StartTimeUnixNano from e.PID itself (so producers never
// import the build-tagged start-time reader) and sets e.SchemaVersion. A
// start-time read failure is non-fatal: StartTimeUnixNano stays 0 and the entry
// is still written, meaning that entry has no PID-reuse guard (liveness-only).
// Register never blocks.
func Register(e Entry) (*Handle, error) {
	e.SchemaVersion = entrySchemaVersion
	e.Status = "" // derived at read time, never authoritative on disk
	if e.StartTimeUnixNano == 0 {
		if st, err := processStartTime(e.PID); err == nil {
			e.StartTimeUnixNano = st
		}
		// On error StartTimeUnixNano stays 0: liveness-only, no reuse guard.
	}

	root, err := Root()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, e.ID)
	// MkdirAll before the write so the .tmp is only ever written inside the
	// entry's own dir, never as debris in a parent. A MkdirAll failure aborts.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agentregistry: create entry dir: %w", err)
	}

	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: marshal entry: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(dir, metaFile), data, 0o600); err != nil {
		return nil, fmt.Errorf("agentregistry: write meta: %w", err)
	}
	return &Handle{dir: dir}, nil
}

// List returns every entry in the scan dir, each reconciled (Status set).
// Bad, vanished, or half-written entries are skipped, never fatal. A missing
// scan dir means "no agents", not an error. List only reports; it never deletes
// (GC is a later phase).
func List() ([]Entry, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	dirents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no scan dir yet -> no agents
		}
		return nil, fmt.Errorf("agentregistry: read root: %w", err)
	}

	var out []Entry
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		if beforeReadEntry != nil {
			beforeReadEntry(name)
		}
		data, err := os.ReadFile(filepath.Join(root, name, metaFile))
		if err != nil {
			continue // vanished or unreadable — skip, never fatal
		}
		var e Entry
		if json.Unmarshal(data, &e) != nil {
			continue // half-written / garbage — skip
		}
		// Reconcile and append in one step so Status is populated as the entry is
		// produced (no separate pass that could widen the TOCTOU window).
		out = append(out, reconcile(e))
	}
	return out, nil
}

// atomicWriteFile writes data to path via a temp file + rename so a reader never
// observes a half-written file. On rename failure a best-effort cleanup of the
// temp file is attempted (its error is ignored; a leftover .tmp is harmless —
// List only iterates directories). Same shape as
// services/agentmodel/auth.atomicWriteFileMode.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
