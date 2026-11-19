package wiring

import (
	"context"
	"fmt"
	"net/http"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

// assistantMailRedirect satisfies the OAuth client's loopback check; refreshing a token, the only
// call the mailbox makes with it, sends no redirect.
const assistantMailRedirect = "http://127.0.0.1/"

// AssistantMailConfig configures the assistant's own mailbox. TokenURL, APIBaseURL and HTTPClient
// replace Google's for tests; empty means the public ones.
type AssistantMailConfig struct {
	Mailbox    config.AssistantMailConfig
	TokenURL   string
	APIBaseURL string
	HTTPClient *http.Client
}

// buildAssistantMail builds the mailbox poller with the mailbox's own OAuth client, which no
// principal's Google connection shares.
func (a *Assembly) buildAssistantMail(ctx context.Context) error {
	in := a.in
	if in.AssistantMail == nil || !in.AssistantMail.Mailbox.Enabled {
		return nil
	}
	cfg := *in.AssistantMail
	tokens, err := google.NewClient(google.ClientConfig{
		ClientID: cfg.Mailbox.ClientID, ClientSecret: cfg.Mailbox.ClientSecret, RedirectURL: assistantMailRedirect,
		TokenURL: cfg.TokenURL, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return fmt.Errorf("initialize assistant mailbox token client: %w", err)
	}
	client, err := gmail.NewClient(gmail.ClientConfig{APIBaseURL: cfg.APIBaseURL, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return fmt.Errorf("initialize assistant mailbox Gmail client: %w", err)
	}
	service, err := assistantmail.NewService(assistantmail.Config{
		Mailbox: cfg.Mailbox, Repository: in.Store, Gmail: client, Tokens: tokens, Switches: a.Switches,
		Logger: in.Logger, Now: in.Core.Now,
	})
	if err != nil {
		return fmt.Errorf("initialize assistant mailbox: %w", err)
	}
	service.SetTelemetry(in.Telemetry)
	if err := a.register(ctx, Connector{Module: service}); err != nil {
		return err
	}
	a.AssistantMail = service
	return nil
}
