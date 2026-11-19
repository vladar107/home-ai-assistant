package wiring

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

func TestTheAssistantMailboxIsRegisteredOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	sim := googlesim.New(t)
	enabled := &AssistantMailConfig{
		Mailbox: config.AssistantMailConfig{
			Enabled: true, Address: "assistant@example.com", DisplayName: "Assistant",
			ClientID: googlesim.AssistantClientID, ClientSecret: googlesim.AssistantClientSecret,
			RefreshToken: "refresh-unknown", Tags: map[string][]string{"alice": {"a1"}},
			DailySendLimit: 10, PollInterval: 5 * time.Minute,
		},
		TokenURL: sim.TokenURL(), APIBaseURL: sim.URL(), HTTPClient: sim.Client(),
	}
	disabled := *enabled
	disabled.Mailbox.Enabled = false

	for name, mail := range map[string]*AssistantMailConfig{"absent": nil, "disabled": &disabled, "enabled": enabled} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
			t.Cleanup(func() { _ = store.Close() })
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			switches, err := integration.NewSwitches(store, logger)
			if err != nil {
				t.Fatal(err)
			}
			a := &Assembly{
				Registry: module.NewRegistry(), Switches: switches,
				in: Inputs{Store: store, Logger: logger, Telemetry: observability.Noop(), AssistantMail: mail},
			}
			if err := a.buildAssistantMail(t.Context()); err != nil {
				t.Fatal(err)
			}
			var registered []string
			for _, m := range a.Registry.Ordered() {
				registered = append(registered, m.Descriptor().ID)
			}
			if name != "enabled" {
				if a.AssistantMail != nil || len(registered) != 0 {
					t.Fatalf("registered %v", registered)
				}
				return
			}
			if a.AssistantMail == nil || len(registered) != 1 || registered[0] != assistantmail.Integration {
				t.Fatalf("registered %v", registered)
			}
			// The simulator refuses the token as invalid_grant only for the mailbox's own client.
			if err := a.AssistantMail.Poll(t.Context()); !errors.Is(err, assistantmail.ErrUnavailable) {
				t.Fatalf("poll = %v", err)
			}
		})
	}
}
