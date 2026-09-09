package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/identity"
)

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{
		{ID: "alice", DisplayName: "Alice", TelegramUserID: 101},
		{ID: "bob", DisplayName: "Bob", TelegramUserID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	return store, ctx
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
