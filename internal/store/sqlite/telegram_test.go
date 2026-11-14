package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/identity"
)

// migratedTemplate mirrors testutil's template, which this package cannot import without a cycle.
var migratedTemplate = sync.OnceValues(buildMigratedTemplate)

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := t.Context()
	store := openMigratedStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

// openMigratedStore copies the database migrated once per process to path and opens it through
// Open, which still checks the migration checksums. Tests of the migrations themselves start from
// an empty file instead.
func openMigratedStore(t *testing.T, path string) *Store {
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
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func buildMigratedTemplate() ([]byte, error) {
	directory, err := os.MkdirTemp("", "assistant-store-template-")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()

	path := filepath.Join(directory, "assistant.sqlite")
	store, err := Open(context.Background(), path)
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

// A file is delivered to the chat this resolves, so every way of losing access must resolve to no
// chat at all rather than to a stale one.
func TestResolveActiveChatFollowsTheAllowlist(t *testing.T) {
	store, ctx := openTestStore(t)

	chat, found, err := store.ResolveActiveChat(ctx, "alice")
	if err != nil || !found || chat != 101 {
		t.Fatalf("alice's chat = %d %v %v", chat, found, err)
	}
	if _, found, err := store.ResolveActiveChat(ctx, "nobody"); err != nil || found {
		t.Fatalf("an unknown principal resolved a chat: %v %v", found, err)
	}

	if _, err := store.db.ExecContext(ctx,
		`UPDATE telegram_allowlist SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE principal_id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ResolveActiveChat(ctx, "alice"); err != nil || found {
		t.Fatalf("a revoked identity resolved a chat: %v %v", found, err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE principals SET status = 'disabled' WHERE id = 'bob'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ResolveActiveChat(ctx, "bob"); err != nil || found {
		t.Fatalf("a disabled principal resolved a chat: %v %v", found, err)
	}
}

// The allowlist does not make principal_id unique. Two active identities is an operator mistake,
// and guessing between them could put a household file in an account its owner has left.
func TestResolveActiveChatRefusesAnAmbiguousPrincipal(t *testing.T) {
	store, ctx := openTestStore(t)

	if _, err := store.db.ExecContext(ctx, `INSERT INTO telegram_identities (telegram_user_id) VALUES (199)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO telegram_allowlist (telegram_user_id, principal_id) VALUES (199, 'alice')`); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.ResolveActiveChat(ctx, "alice"); err != nil || found {
		t.Fatalf("a principal with two active identities resolved a chat: %v %v", found, err)
	}
}
