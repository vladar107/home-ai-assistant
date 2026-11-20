package events_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/events"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

// tokenSender keeps the tokens it is asked to show.
type tokenSender struct {
	tokens []string
}

func (s *tokenSender) SendEvent(context.Context, int64, events.Message) (int64, error) { return 1, nil }
func (s *tokenSender) SendToken(_ context.Context, _ int64, _, token string) (int64, error) {
	s.tokens = append(s.tokens, token)
	return int64(len(s.tokens)), nil
}
func (s *tokenSender) DeleteMessage(context.Context, int64, int64) error { return nil }

type silentAssessor struct{}

func (silentAssessor) RunAssess(context.Context, string, string) (turn.ScheduledReply, error) {
	return turn.ScheduledReply{}, nil
}

var (
	alice = identity.ConfiguredPrincipal{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}
	bob   = identity.ConfiguredPrincipal{ID: "bob", DisplayName: "Bob", TelegramUserID: 202}
)

func newEvents(t *testing.T, store *sqlite.Store, sender events.Sender) *events.Service {
	t.Helper()
	service, err := events.NewService(events.Config{}, store, store, sender, silentAssessor{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// A user taken out of the users file loses their event sources at the next start, and gets them
// back when they are configured again.
func TestEventTokenStopsWorkingForAUserRemovedFromTheUsersFile(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state", "assistant.sqlite")
	store := testutil.OpenStore(t, path)
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{alice, bob}); err != nil {
		t.Fatal(err)
	}
	sender := &tokenSender{}
	if _, err := newEvents(t, store, sender).Create(ctx, "bob", "Backups"); err != nil {
		t.Fatal(err)
	}
	if len(sender.tokens) != 1 {
		t.Fatalf("tokens shown = %d", len(sender.tokens))
	}
	token := sender.tokens[0]
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{alice}); err != nil {
		t.Fatal(err)
	}
	service := newEvents(t, store, &tokenSender{})
	if source, found, err := service.Authenticate(ctx, token); err != nil || found {
		t.Fatalf("a removed user's token authenticated %+v: %v", source, err)
	}

	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{alice, bob}); err != nil {
		t.Fatal(err)
	}
	if source, found, err := service.Authenticate(ctx, token); err != nil || !found || source.PrincipalID != "bob" {
		t.Fatalf("a user configured again authenticated %+v %v %v", source, found, err)
	}
}
