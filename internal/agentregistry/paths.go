package agentregistry

import (
	"fmt"
	"os"
	"path/filepath"
)

// dirName is the registry's directory under the chosen runtime/cache root.
const dirName = "agent-in-the-shell"

// Root returns the scan-directory root for the running-agent registry,
// resolved in order:
//
//  1. $XDG_RUNTIME_DIR (when set) -> <XDG_RUNTIME_DIR>/agent-in-the-shell/agents.
//     On Linux this is typically /run/user/<uid>, mode 0700, cleared on logout —
//     exactly the ephemeral, per-user semantics the registry wants.
//  2. os.UserCacheDir() -> <cache>/agent-in-the-shell/agents. This is the macOS
//     path (no XDG_RUNTIME_DIR by default -> ~/Library/Caches/...). Cache, not
//     config, because the registry is ephemeral. os.TempDir is deliberately not
//     used: it is world-writable and a symlink-attack surface for a directory we
//     chmod 0700 and trust pids out of.
//  3. otherwise an error.
//
// The one honest downside of the cache fallback — it survives a reboot, so a
// crash-on-reboot orphan can outlive its process — is caught by reconciliation
// (the pid is dead or its start-time differs, so the entry reads as stale).
func Root() (string, error) { return resolveRoot("agents") }

// CompletedRoot returns the directory holding durable CompletionRecords for
// finished runs — a sibling of Root's live-entry directory ("completed"
// instead of "agents"), resolved via the same fallback order, so a run's exit
// status/output survives its Entry being removed on completion.
func CompletedRoot() (string, error) { return resolveRoot("completed") }

// resolveRoot is Root/CompletedRoot's shared resolution order, parameterized
// only by the trailing directory name.
func resolveRoot(leaf string) (string, error) {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, dirName, leaf), nil
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		return filepath.Join(cache, dirName, leaf), nil
	}
	return "", fmt.Errorf("agentregistry: cannot resolve a runtime root (set XDG_RUNTIME_DIR or ensure a user cache dir exists)")
}
