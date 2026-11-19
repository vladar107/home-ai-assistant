package wiring

import (
	"context"
	"fmt"
	"net/http"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// assistantMailRedirect satisfies the OAuth client's loopback check; refreshing a token, the only
// call the mailbox makes with it, sends no redirect.
const assistantMailRedirect = "http://127.0.0.1/"

// AssistantMailConfig configures the assistant's own mailbox. Sender delivers its notices to each
// principal's own chat. TokenURL, APIBaseURL and HTTPClient replace Google's for tests; empty
// means the public ones.
type AssistantMailConfig struct {
	Mailbox    config.AssistantMailConfig
	Sender     assistantmail.Sender
	TokenURL   string
	APIBaseURL string
	HTTPClient *http.Client
}

// buildAssistantMail builds the mailbox poller with the mailbox's own OAuth client, which no
// principal's Google connection shares, and its list and read tools. Notices reach only the
// principal's own chat, resolved from the allowlist at delivery.
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
		Chats: in.Store, Sender: cfg.Sender, Languages: in.Store, Location: a.location, Notices: a.mailNotices,
		Logger: in.Logger, Now: in.Core.Now,
	})
	if err != nil {
		return fmt.Errorf("initialize assistant mailbox: %w", err)
	}
	service.SetTelemetry(in.Telemetry)
	if err := a.register(ctx, Connector{Module: service, Tools: []tools.Provider{service}, Switch: &SwitchSettings{
		Switch: assistantmail.Switch, Unavailable: service.Unavailable, Address: service.Address,
	}}); err != nil {
		return err
	}
	a.AssistantMail = service
	return nil
}
