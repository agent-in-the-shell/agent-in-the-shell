// Package herospath is the ONE place heros services resolve their on-disk paths
// and deploy binaries, so the fleet stops sprawling across ~/.config, ~/.local/
// share/heros, ~/.local/share/<own>, ~/.<dotdir>, and CWD-relative defaults.
//
// The canonical pattern a service should follow:
//
//	db  := herospath.ResolveData("AGENT_MODEL_DB",      "agentmodel", "agentmodel.db")
//	cfg := herospath.ResolveConfig("AGENT_MODEL_CONFIG", "agentmodel", "config.yaml")
//	bin := herospath.ResolveBin("agent-model") // deploy binary, not a ~/go/bin shadow
//
// Rules:
//   - STATE/DATA (SQLite, caches, corpora) lives under the DATA tree:
//     ${XDG_DATA_HOME:-~/.local/share}/heros/<svc>/… — per XDG, mutable state is
//     DATA, not CONFIG.
//   - CONFIG (user-editable files) lives under the CONFIG tree:
//     ${XDG_CONFIG_HOME:-~/.config}/heros/<svc>/… — honoring XDG_CONFIG_HOME,
//     which nothing in the fleet did before this.
//   - a service's own full-path env var (AGENT_<SVC>_DB / _CONFIG) always wins.
//   - CREATING a resolved path is part of the job, not a separate concern:
//     EnsureParent, SecureDir and SecureSQLiteFile make a path safe to hold
//     secrets (0700 dir, 0600 file) so callers do not each re-derive the modes.
//     OpenSecureAppend goes one step past "resolve" and hands back the open
//     descriptor, because the mode can only be enforced at the moment of open.
//   - BINARIES prefer $HEROS_BIN_DIR (the deploy bin dir) over a bare PATH
//     lookup, so a stale go-install copy can't shadow the pinned deploy binary.
//
// Home-resolution failure degrades to a relative path (name) rather than
// crashing: a service that cannot find $HOME should still start.
package herospath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// base returns ${xdgEnv} when set, else ~/<homeSubdir> (e.g. ".local/share" or
// ".config"); "" if $HOME is unresolvable (callers degrade to a relative path).
func base(xdgEnv, homeSubdir string) string {
	if dir := strings.TrimSpace(os.Getenv(xdgEnv)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, homeSubdir)
}

// DataFile returns a file directly under the heros DATA dir
// (${XDG_DATA_HOME:-~/.local/share}/heros/<name>). Retained for callers that
// already carry the service name in the filename; prefer DataPath for new code
// so each service gets its own subdirectory.
func DataFile(name string) string {
	dir := base("XDG_DATA_HOME", filepath.Join(".local", "share"))
	if dir == "" {
		return name
	}
	return filepath.Join(dir, "heros", name)
}

// DataDir is a service's canonical DATA directory:
// ${XDG_DATA_HOME:-~/.local/share}/heros/<svc>.
func DataDir(svc string) string {
	dir := base("XDG_DATA_HOME", filepath.Join(".local", "share"))
	if dir == "" {
		return svc
	}
	return filepath.Join(dir, "heros", svc)
}

// DataPath is one file under a service's DATA directory (SQLite, caches, state).
func DataPath(svc, name string) string { return filepath.Join(DataDir(svc), name) }

// ConfigDir is a service's canonical CONFIG directory:
// ${XDG_CONFIG_HOME:-~/.config}/heros/<svc>.
func ConfigDir(svc string) string {
	dir := base("XDG_CONFIG_HOME", ".config")
	if dir == "" {
		return svc
	}
	return filepath.Join(dir, "heros", svc)
}

// ConfigPath is one file under a service's CONFIG directory (user-editable).
func ConfigPath(svc, name string) string { return filepath.Join(ConfigDir(svc), name) }

// ResolveData is the canonical storage resolver: the service's full-path env var
// wins (an operator escape hatch), else the per-service DataPath. envVar may be
// "" to skip the override.
func ResolveData(envVar, svc, name string) string {
	if envVar != "" {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	return DataPath(svc, name)
}

// ResolveConfig is the canonical config resolver: the service's full-path env
// var wins, else the per-service ConfigPath.
func ResolveConfig(envVar, svc, name string) string {
	if envVar != "" {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	return ConfigPath(svc, name)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ResolveDataLegacy resolves a service's data file while MIGRATING it to the
// canonical layout WITHOUT ever orphaning data that already lives at an older
// location. Resolution order:
//  1. envVar — the service's full-path override (an operator escape hatch);
//  2. the canonical DataPath if it already exists (already on the new layout);
//  3. the first legacyPath that exists — an existing install keeps its data IN
//     PLACE; nothing is moved or copied, so SQLite -wal/-shm sidecars, perms,
//     and open handles are untouched (a rename would risk all three);
//  4. the canonical DataPath — a fresh install starts on the new layout.
//
// So new installs converge to ~/.local/share/heros/<svc>/, existing installs
// keep working, and a restart never silently starts on an empty store. This is
// the safe way for a service to adopt DataPath when it already has live data at
// a legacy path (~/.<svc>, ~/.local/share/<svc>, …). Pass the OLD default(s) as
// legacyPaths.
func ResolveDataLegacy(envVar, svc, name string, legacyPaths ...string) string {
	return resolveLegacy(DataPath(svc, name), envVar, legacyPaths)
}

// ResolveDataDirLegacy is the DIRECTORY analog of ResolveDataLegacy: it resolves a
// service's data DIRECTORY while MIGRATING it to the canonical layout WITHOUT ever
// orphaning a directory that already lives at an older location. Resolution order:
//  1. envVar — the service's full-path override (an operator escape hatch);
//  2. the canonical DataDir if it already exists (already on the new layout);
//  3. the first legacyDir that exists — an existing install keeps its data IN
//     PLACE; nothing is moved or copied, so its contents, perms, and open handles
//     are untouched (a rename would risk all three);
//  4. the canonical DataDir — a fresh install starts on the new layout.
//
// So new installs converge to ~/.local/share/heros/<svc>/, existing installs keep
// working, and a restart never silently starts on an empty directory. This is the
// safe way for a service to adopt DataDir when it already has a live directory at a
// legacy path (~/.<svc>, ~/.local/share/<svc>, …). Unlike ResolveDataLegacy there
// is no name arg — a directory has no filename. Pass the OLD default dir(s) as
// legacyDirs.
func ResolveDataDirLegacy(envVar, svc string, legacyDirs ...string) string {
	if envVar != "" {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	canonical := DataDir(svc)
	if dirExists(canonical) {
		return canonical
	}
	for _, ld := range legacyDirs {
		if ld != "" && dirExists(ld) {
			return ld
		}
	}
	return canonical
}

// ResolveConfigLegacy is the config-tree analog of ResolveDataLegacy.
func ResolveConfigLegacy(envVar, svc, name string, legacyPaths ...string) string {
	return resolveLegacy(ConfigPath(svc, name), envVar, legacyPaths)
}

func resolveLegacy(canonical, envVar string, legacyPaths []string) string {
	if envVar != "" {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	if fileExists(canonical) {
		return canonical
	}
	for _, lp := range legacyPaths {
		if lp != "" && fileExists(lp) {
			return lp
		}
	}
	return canonical
}

// BinDir is the deploy bin dir ($HEROS_BIN_DIR — the deploy boundary), or ""
// if unset. Use it when building a PATH for child processes: prepend it ahead of
// ~/go/bin so scheduled jobs run the pinned deploy binaries.
func BinDir() string { return strings.TrimSpace(os.Getenv("HEROS_BIN_DIR")) }

// ResolveBin resolves a heros CLI to run, preferring $HEROS_BIN_DIR/<name> when
// it holds an executable of that name, else the bare name for the caller to
// PATH-resolve. This keeps a context that sets the deploy bin dir on the pinned
// binary rather than a stale ~/go/bin go-install shadow (also used by agentshell.resolveAgentExec).
func ResolveBin(name string) string {
	if dir := BinDir(); dir != "" {
		cand := filepath.Join(dir, name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand
		}
	}
	return name
}

// EnsureParent MkdirAll's the parent directory of file (0700, since state/config
// may hold secrets) and returns file, so a resolve+create is one call:
//
//	db, err := herospath.EnsureParent(herospath.DataPath("agentmodel", "agentmodel.db"))
func EnsureParent(file string) (string, error) {
	if dir := filepath.Dir(file); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return file, err
		}
	}
	return file, nil
}

// SecureSQLiteFile prepares path to hold sensitive data. Call it BEFORE opening
// the database.
//
// SQLite creates its files 0644 (masked by umask), which is wrong for anything
// holding request or response content: under a default umask the database is
// world-readable, and so is the -wal, where the writes that have not been
// checkpointed yet live — the most recent rows.
//
// Two halves, both load-bearing:
//
//   - Whatever is already on disk gets tightened. A -wal left behind by a
//     crashed older version is REUSED rather than recreated, so it keeps its
//     old mode unless chmod-ed directly.
//   - An absent database is created 0600 first. Letting SQLite create it leaves
//     it world-readable for as long as the schema takes to apply, and a local
//     reader that opens it inside that window keeps read access for the life of
//     the descriptor — Unix checks permission at open, not at read. Restricting
//     before the first write is the same posture auth/internal_helpers.go takes
//     for credentials.
//
// Once the database itself is 0600, SQLite derives the mode of every sidecar it
// creates from it, so -wal, -shm and a -journal all follow without being named
// here. The two named below are only for the ones already on disk.
//
// ":memory:" and other non-file DSNs are left alone.
func SecureSQLiteFile(path string) error {
	if path == "" || strings.HasPrefix(path, ":") {
		return nil
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chmod %q: %w", p, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	return f.Close()
}

// SecureDir creates dir with mode 0700 and forces that mode even when the
// directory already exists.
//
// EnsureParent is the create-only form; this is the one to use when the mode
// has to hold on an upgrade. Both decline to touch "" and "." — see the guard
// below for why that matters more here than it looks. MkdirAll applies its mode only to directories it
// creates, so a tree left 0755 by an older version keeps that mode for
// good. That matters most where the *listing* is the secret — session
// directories named after the projects someone has worked on leak that list to
// anyone who can read the parent, whatever the modes of the files inside.
func SecureDir(dir string) error {
	// "" and "." are guarded for the same reason EnsureParent guards them: a
	// relative path like content_log.path: "content.jsonl" has "." for a parent,
	// and narrowing the process's own working directory to 0700 is never what a
	// caller meant to ask for.
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %q: %w", dir, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod 0700 %q: %w", dir, err)
		}
	}
	return nil
}

// OpenSecureAppend opens path for appending — creating it and its parent
// directory if needed — guarantees the file is 0600, and returns its size.
//
// This is the one place a file that holds secrets gets opened for append, so a
// future caller cannot forget the mode. That is not hypothetical: the perm
// argument to OpenFile applies only when the file is CREATED, so an existing
// file keeps whatever mode it had. Reusing this helper keeps append-only
// secret-bearing files private, including files created by older versions.
//
// A mode that cannot be set is an error rather than a warning: the alternative
// is appending secrets to a file whose reader set we do not know.
//
// The Stat is not overhead — an fstat costs a fraction of an fchmod, so
// checking first skips a metadata write on every open after the first, and the
// size it returns is what append-mode callers need anyway.
func OpenSecureAppend(path string) (*os.File, int64, error) {
	if err := SecureDir(filepath.Dir(path)); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat: %w", err)
	}
	if fi.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("chmod 0600: %w", err)
		}
	}
	return f, fi.Size(), nil
}
