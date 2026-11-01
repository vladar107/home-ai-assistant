package export

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Creators racing for the first key all end up with the one that was linked into place, and no
// temporary file is left beside it.
func TestKeyCreatorsAgreeOnOneKey(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, KeyFileName)
	const creators = 16
	keys := make([][]byte, creators)
	errs := make([]error, creators)
	var wg sync.WaitGroup
	for index := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[index], errs[index] = loadKey(path, true)
		}()
	}
	wg.Wait()
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := range creators {
		if errs[index] != nil || !bytes.Equal(keys[index], stored) {
			t.Fatalf("creator %d: key %x, error %v; stored %x", index, keys[index], errs[index], stored)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); len(entries) != 1 || err != nil || info.Mode().Perm() != 0o600 || len(stored) != keyBytes {
		t.Fatalf("data directory holds %v, key %v %v", entries, info, err)
	}
}

// A key file that is there but corrupt is reported to the operator and never replaced, since a new
// key would make every earlier export unrestorable.
func TestCorruptKeyIsNeverReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(path, true); err == nil || !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "data backup") {
		t.Fatalf("loading a corrupt key: %v", err)
	}
	if stored, err := os.ReadFile(path); err != nil || string(stored) != "short" {
		t.Fatalf("the corrupt key was replaced: %q, %v", stored, err)
	}
}
