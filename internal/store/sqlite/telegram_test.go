package sqlite

import (
	"context"
	"database/sql"
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
	t.Parallel()
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
	t.Parallel()
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

var (
	configuredAlice = identity.ConfiguredPrincipal{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}
	configuredBob   = identity.ConfiguredPrincipal{ID: "bob", DisplayName: "Bob", TelegramUserID: 202}
)

func bootstrap(t *testing.T, store *Store, configured ...identity.ConfiguredPrincipal) {
	t.Helper()
	if err := store.BootstrapConfiguredPrincipals(t.Context(), configured); err != nil {
		t.Fatal(err)
	}
}

func allowlistTimes(t *testing.T, store *Store, telegramUserID int64) (allowedAt string, revokedAt sql.NullString) {
	t.Helper()
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT allowed_at, revoked_at FROM telegram_allowlist WHERE telegram_user_id = ?`, telegramUserID,
	).Scan(&allowedAt, &revokedAt); err != nil {
		t.Fatal(err)
	}
	return allowedAt, revokedAt
}

// The users file is the whole allowlist: a user taken out of it loses their chat at the next start,
// and putting them back restores it.
func TestBootstrapRevokesAUserRemovedFromTheUsersFile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	store := openMigratedStore(t, path)
	bootstrap(t, store, configuredAlice, configuredBob)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bootstrap(t, store, configuredAlice)

	if _, found, err := store.ResolveActiveChat(ctx, "bob"); err != nil || found {
		t.Fatalf("a removed user resolved a chat: %v %v", found, err)
	}
	if authorization, err := store.ResolveActivePrincipal(ctx, 202); err != nil || authorization.Status != identity.AuthorizationStatusAllowlistRevoked {
		t.Fatalf("a removed user's authorization = %+v %v", authorization, err)
	}
	if chat, found, err := store.ResolveActiveChat(ctx, "alice"); err != nil || !found || chat != 101 {
		t.Fatalf("a configured user's chat = %d %v %v", chat, found, err)
	}

	bootstrap(t, store, configuredAlice, configuredBob)
	if chat, found, err := store.ResolveActiveChat(ctx, "bob"); err != nil || !found || chat != 202 {
		t.Fatalf("a user configured again resolved chat %d %v %v", chat, found, err)
	}
}

func TestBootstrapRevokesRestoresAndRevokesAgain(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openMigratedStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	bootstrap(t, store, configuredAlice, configuredBob)
	allowedAt, _ := allowlistTimes(t, store, 202)

	for round := range 2 {
		bootstrap(t, store, configuredAlice)
		if _, revokedAt := allowlistTimes(t, store, 202); !revokedAt.Valid || revokedAt.String < allowedAt {
			t.Fatalf("round %d: revoked_at = %+v, allowed_at %s", round, revokedAt, allowedAt)
		}
		if _, found, err := store.ResolveActiveChat(ctx, "bob"); err != nil || found {
			t.Fatalf("round %d: a revoked user resolved a chat: %v %v", round, found, err)
		}

		bootstrap(t, store, configuredAlice, configuredBob)
		restoredAllowedAt, revokedAt := allowlistTimes(t, store, 202)
		if revokedAt.Valid || restoredAllowedAt != allowedAt {
			t.Fatalf("round %d: restored row allowed_at %s revoked_at %+v, want allowed_at %s", round, restoredAllowedAt, revokedAt, allowedAt)
		}
		if chat, found, err := store.ResolveActiveChat(ctx, "bob"); err != nil || !found || chat != 202 {
			t.Fatalf("round %d: a restored user resolved chat %d %v %v", round, chat, found, err)
		}
	}
	if _, revokedAt := allowlistTimes(t, store, 101); revokedAt.Valid {
		t.Fatalf("a configured user was revoked at %s", revokedAt.String)
	}
}

// A row allowed under a clock that ran ahead must still be revocable, or the start would fail on
// the allowlist's CHECK.
func TestBootstrapRevokesAUserAllowedAheadOfTheClock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openMigratedStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	bootstrap(t, store, configuredAlice, configuredBob)
	const ahead = "2999-01-01T00:00:00.000Z"
	if _, err := store.db.ExecContext(ctx, `UPDATE telegram_allowlist SET allowed_at = ? WHERE telegram_user_id = 202`, ahead); err != nil {
		t.Fatal(err)
	}

	bootstrap(t, store, configuredAlice)
	if _, revokedAt := allowlistTimes(t, store, 202); revokedAt.String != ahead {
		t.Fatalf("revoked_at = %+v, want %s", revokedAt, ahead)
	}
	if _, found, err := store.ResolveActiveChat(ctx, "bob"); err != nil || found {
		t.Fatalf("a revoked user resolved a chat: %v %v", found, err)
	}
}

func TestBootstrapRefusesAnIdentityMappedToAnotherPrincipal(t *testing.T) {
	t.Parallel()
	store := openMigratedStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	bootstrap(t, store, configuredAlice, configuredBob)

	impostor := identity.ConfiguredPrincipal{ID: "carol", DisplayName: "Carol", TelegramUserID: 202}
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{configuredAlice, impostor}); err == nil {
		t.Fatal("an identity mapped to another principal was remapped")
	}
	if _, revokedAt := allowlistTimes(t, store, 202); revokedAt.Valid {
		t.Fatalf("a refused bootstrap revoked bob at %s", revokedAt.String)
	}
}
