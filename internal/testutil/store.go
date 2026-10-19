// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

var migratedTemplate = sync.OnceValues(buildTemplate)

// OpenStore opens a migrated store at path. The migrations run once per test
// process; each call copies the result into path and opens it through the real
// sqlite.Open, which still checks permissions, symlinks, and migration
// checksums. Anything already at path is replaced. Tests that exercise the
// migrations must use sqlite.Open directly.
func OpenStore(t testing.TB, path string) *sqlite.Store {
	t.Helper()
	template, err := migratedTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, template, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func buildTemplate() ([]byte, error) {
	directory, err := os.MkdirTemp("", "assistant-template-")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()

	path := filepath.Join(directory, "assistant.sqlite")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("migrate template database: %w", err)
	}
	if err := store.Close(); err != nil {
		return nil, fmt.Errorf("close template database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_, err := os.Stat(path + suffix)
		if err == nil {
			return nil, fmt.Errorf("template database left %s behind after close", suffix)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check template database %s: %w", suffix, err)
		}
	}
	template, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template database: %w", err)
	}
	return template, nil
}
