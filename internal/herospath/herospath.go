// Package herospath is the ONE place heros services resolve their on-disk paths
// and deploy binaries, so the fleet stops sprawling across ~/.config, ~/.local/
// share/heros, ~/.local/share/<own>, ~/.<dotdir>, and CWD-relative defaults.
//
// The canonical pattern a service should follow:
//
//	db  := herospath.ResolveData("AGENT_DESK_DB",     "agentdesk", "agentdesk.db")
//	cfg := herospath.ResolveConfig("AGENT_DESK_CONFIG","agentdesk", "config.yaml")
//	bin := herospath.ResolveBin("agent-pi") // deploy binary, not a ~/go/bin shadow
//
// Rules:
//   - STATE/DATA (SQLite, caches, corpora) lives under the DATA tree:
//     ${XDG_DATA_HOME:-~/.local/share}/heros/<svc>/… — per XDG, mutable state is
//     DATA, not CONFIG.
//   - CONFIG (user-editable files) lives under the CONFIG tree:
//     ${XDG_CONFIG_HOME:-~/.config}/heros/<svc>/… — honoring XDG_CONFIG_HOME,
//     which nothing in the fleet did before this.
//   - a service's own full-path env var (AGENT_<SVC>_DB / _CONFIG) always wins.
//   - BINARIES prefer $HEROS_BIN_DIR (the #586 deploy bin dir) over a bare PATH
//     lookup, so a stale go-install copy can't shadow the pinned deploy binary.
//
// Home-resolution failure degrades to a relative path (name) rather than
// crashing — the same posture the private copies had.
package herospath

import (
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
// (${XDG_DATA_HOME:-~/.local/share}/heros/<name>). Retained for existing callers
// (agentfeed/agentknowledge use a service-prefixed filename); prefer DataPath for
// new code so each service gets its own subdirectory.
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

// BinDir is the deploy bin dir ($HEROS_BIN_DIR — the #586 deploy boundary), or ""
// if unset. Use it when building a PATH for child processes: prepend it ahead of
// ~/go/bin so scheduled jobs run the pinned deploy binaries.
func BinDir() string { return strings.TrimSpace(os.Getenv("HEROS_BIN_DIR")) }

// ResolveBin resolves a heros CLI to run, preferring $HEROS_BIN_DIR/<name> when
// it holds an executable of that name, else the bare name for the caller to
// PATH-resolve. This keeps a context that sets the deploy bin dir on the pinned
// binary rather than a stale ~/go/bin go-install shadow (the #586 drift fixed in
// agentshell.resolveAgentExec, generalized here).
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
//	db, err := herospath.EnsureParent(herospath.DataPath("agentdesk", "agentdesk.db"))
func EnsureParent(file string) (string, error) {
	if dir := filepath.Dir(file); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return file, err
		}
	}
	return file, nil
}
