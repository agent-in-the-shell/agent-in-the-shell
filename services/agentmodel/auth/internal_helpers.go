package auth

import (
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to path via tmp+rename so a crashed write
// never leaves a partial file that crashes the next read. Permissions are
// 0o600 (owner-only) since auth blobs are credentials.
func atomicWriteFile(path string, data []byte) error {
	return atomicWriteFileMode(path, data, 0o600)
}

func atomicWriteFileMode(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	// A UNIQUE temp name (not a fixed path+".tmp") so two concurrent writers —
	// e.g. a login CLI and the gateway's refresh — can't clobber each other's
	// in-flight file and publish torn bytes.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// fsync the file so the bytes are durable before the rename publishes them —
	// otherwise a crash after rename can expose a zero-length credential file.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// fsync the directory so the rename itself survives a crash.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// defaultTokenDir returns a sensible default directory for storing OAuth
// auth.json files: $envVar if set, else $HOME/.config/agentmodel/<subdir>,
// else .agentmodel/<subdir> if HOME isn't resolvable.
func defaultTokenDir(envVar, subdir string) string {
	if d := os.Getenv(envVar); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "agentmodel", subdir)
	}
	return filepath.Join(".agentmodel", subdir)
}
