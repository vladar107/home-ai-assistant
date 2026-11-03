package export

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyFileName is the export key's file in the data directory.
const KeyFileName = "export.key"

const keyBytes = 32

// errNoKey reports that the installation has never exported, so it holds no key to verify with.
var errNoKey = errors.New("export key does not exist")

// loadKey reads the installation's export key, creating it when create is set and there is none.
// The key is a regular file of exactly keyBytes, readable by the owner alone. A key file that is
// there but unreadable is never replaced, since a new key would make every earlier export
// unrestorable; the operator restores it from the data backup instead.
func loadKey(path string, create bool) ([]byte, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return nil, errNoKey
		}
		return createKey(path)
	case err != nil:
		return nil, fmt.Errorf("inspect export key: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return nil, fmt.Errorf("export key %q is not a regular file; restore it from the data backup", path)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read export key: %w", err)
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("export key %q is corrupt: it holds %d bytes, not %d; restore it from the data backup, since a new key would make every earlier export unrestorable", path, len(key), keyBytes)
	}
	return key, nil
}

// createKey writes a new key to a temporary file beside path, syncs it, and links it into place,
// so the key file never exists half written. When another creator linked theirs first, its key is
// the installation's.
func createKey(path string) ([]byte, error) {
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate export key: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+KeyFileName+"-*")
	if err != nil {
		return nil, fmt.Errorf("create export key: %w", err)
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure export key: %w", err)
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write export key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write export key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("write export key: %w", err)
	}
	if err := os.Link(temporary, path); errors.Is(err, os.ErrExist) {
		return loadKey(path, false)
	} else if err != nil {
		return nil, fmt.Errorf("store export key: %w", err)
	}
	return key, nil
}
