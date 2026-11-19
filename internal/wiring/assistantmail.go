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
	"github.com/vladar107/home-ai-assistant/internal/turn"
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

// mailTriageChecker runs the mailbox's triage on the turn engine.
type mailTriageChecker struct {
	engine *turn.Engine
}

func (c mailTriageChecker) RunConfined(ctx context.Context, principalID, message string, scope tools.Scope) (string, string, error) {
	reply, err := c.engine.RunConfined(ctx, principalID, message, scope)
	return reply.Text, reply.ErrorKind, err
}

// buildAssistantMail builds the mailbox poller with the mailbox's own OAuth client, which no
// principal's Google connection shares, its list and read tools, and its send tool, which only
// proposes, with the executor of confirmed sends. Notices of the mail that triage finds needs the
// user reach only the principal's own chat, resolved from the allowlist at delivery.
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
	mailConfig := assistantmail.Config{
		Mailbox: cfg.Mailbox, Repository: in.Store, Gmail: client, Tokens: tokens, Switches: a.Switches,
		Chats: in.Store, Sender: cfg.Sender, Languages: in.Store, Location: a.location, Notices: a.mailNotices,
		Proposer: a.Approvals, DisplayNames: a.displayNames, Logger: in.Logger, Now: in.Core.Now,
	}
	if a.exposures != nil {
		mailConfig.Exposure = a.exposures
	}
	// Triage reads each thread's new mail on the principal's own subscription, confined to that
	// thread; without the engine, every new message is listed untriaged.
	if a.Turns != nil {
		mailConfig.Checker = mailTriageChecker{engine: a.Turns}
	}
	service, err := assistantmail.NewService(mailConfig)
	if err != nil {
		return fmt.Errorf("initialize assistant mailbox: %w", err)
	}
	service.SetTelemetry(in.Telemetry)
	if err := a.register(ctx, Connector{Module: service, Tools: []tools.Provider{service}, Executor: service, Switch: &SwitchSettings{
		Switch: assistantmail.Switch, Unavailable: service.Unavailable, Address: service.Address,
	}}); err != nil {
		return err
	}
	a.AssistantMail = service
	return nil
}
