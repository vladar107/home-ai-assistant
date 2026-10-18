package connection_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

type silentDeliverer struct{}

func (silentDeliverer) SendMessage(context.Context, int64, string) error { return nil }

func writeCredential(t *testing.T, profilesDir, accountID string) {
	t.Helper()
	directory := filepath.Join(profilesDir, "alice")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"access-token","refresh":"refresh-token","expires":%d,"accountId":%q}}`,
		time.Now().Add(time.Hour).UnixMilli(), accountID)
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The conversation gate, the settings status, and the credential handed to requests agree: a
// credential file for another account than the bound one counts as not connected everywhere.
func TestACredentialForAnotherAccountIsNotReady(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	profilesDir := filepath.Join(t.TempDir(), "profiles")
	service, err := connection.NewService(connection.Config{ProfilesDir: profilesDir, Principals: []string{"alice"}},
		store, silentDeliverer{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := store.BindModelConnection(ctx, "alice", "openai-codex", "account-a"); err != nil || !bound {
		t.Fatalf("bind: bound=%v err=%v", bound, err)
	}

	writeCredential(t, profilesDir, "account-a")
	ready, err := service.Ready(ctx, "alice")
	if err != nil || !ready {
		t.Fatalf("ready with the bound account = %v %v", ready, err)
	}
	if status, err := service.Status(ctx, "alice"); err != nil || !status.Connected {
		t.Fatalf("status with the bound account = %+v %v", status, err)
	}

	writeCredential(t, profilesDir, "account-b")
	ready, err = service.Ready(ctx, "alice")
	if err != nil || ready {
		t.Fatalf("ready with another account = %v %v", ready, err)
	}
	if status, err := service.Status(ctx, "alice"); err != nil || status.Connected {
		t.Fatalf("status with another account = %+v %v", status, err)
	}
	if _, err := service.Credential(ctx, "alice"); !errors.Is(err, connection.ErrNotConnected) {
		t.Fatalf("credential with another account: %v", err)
	}
}
