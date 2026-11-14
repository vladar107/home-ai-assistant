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
	"github.com/vladar107/home-ai-assistant/internal/testutil"
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
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
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

func TestCurrentCredentialNeverRefreshes(t *testing.T) {
	ctx := t.Context()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
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
	credential, err := service.CurrentCredential(ctx, "alice")
	if err != nil || credential.AccessToken != "access-token" {
		t.Fatalf("fresh credential = %+v %v", credential, err)
	}

	record := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"access-token","refresh":"refresh-token","expires":%d,"accountId":"account-a"}}`,
		time.Now().Add(time.Minute).UnixMilli())
	if err := os.WriteFile(filepath.Join(profilesDir, "alice", "auth.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CurrentCredential(ctx, "alice"); !errors.Is(err, connection.ErrCredentialStale) {
		t.Fatalf("credential due for refresh: %v", err)
	}
}

// A negative poll floor or slow-down step is refused rather than polling in a tight loop.
func TestNegativePollSettingsAreRefused(t *testing.T) {
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, cfg := range map[string]connection.Config{
		"poll interval": {MinimumPollInterval: -time.Second},
		"slow-down":     {SlowDownStep: -time.Second},
	} {
		cfg.ProfilesDir = filepath.Join(t.TempDir(), "profiles")
		if _, err := connection.NewService(cfg, store, silentDeliverer{}, logger); err == nil {
			t.Fatalf("%s: a negative value was accepted", name)
		}
	}
	if _, err := connection.NewService(connection.Config{ProfilesDir: filepath.Join(t.TempDir(), "profiles")}, store, silentDeliverer{}, logger); err != nil {
		t.Fatalf("zero poll settings: %v", err)
	}
}
