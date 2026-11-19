package eval

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/config"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/wiring"
)

// SendPlainReply records a notice of the assistant mailbox as a reply, so a triage_mail step's
// reply is its notice.
func (c *Chat) SendPlainReply(_ context.Context, _, replyTo int64, text string) error {
	c.reply(replyTo, text)
	return nil
}

// seedAssistantMail serves the assistant's own mailbox from a Google fake of its own, authorized
// once as the operator does, with the eval principal's tag; its mail waits for a triage_mail step.
func (w *World) seedAssistantMail(in *wiring.Inputs) error {
	var section assistantMailSection
	found, err := w.section(SectionAssistantMail, &section)
	if err != nil || !found {
		return err
	}
	sim := googlesim.New(w.reporter)
	sim.SetClock(w.Clock.Now)
	address := cmp.Or(section.Address, DefaultAssistantMailbox)
	refresh, err := authorizeAssistantMail(sim, address)
	if err != nil {
		return fmt.Errorf("authorize the assistant mailbox fake: %w", err)
	}
	w.assistantMail, w.assistantMailSeed = sim, section
	in.AssistantMail = &wiring.AssistantMailConfig{
		Mailbox: config.AssistantMailConfig{
			Enabled: true, Address: address, DisplayName: "Assistant", ClientID: googlesim.AssistantClientID,
			ClientSecret: googlesim.AssistantClientSecret, RefreshToken: refresh,
			Tags:           map[string][]string{PrincipalID: {cmp.Or(section.Tag, DefaultAssistantTag)}},
			DailySendLimit: 10, PollInterval: 5 * time.Minute,
		},
		Sender: w.Chat, TokenURL: sim.TokenURL(), APIBaseURL: sim.URL(), HTTPClient: sim.Client(),
	}
	return nil
}

// authorizeAssistantMail plays the operator's one-time consent for the mailbox's own client and
// returns its refresh token.
func authorizeAssistantMail(sim *googlesim.Simulator, account string) (string, error) {
	const verifier = "eval-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
	const redirect = "http://127.0.0.1:49152/callback"
	tokens, err := google.NewClient(google.ClientConfig{
		ClientID: googlesim.AssistantClientID, ClientSecret: googlesim.AssistantClientSecret, RedirectURL: redirect,
		TokenURL: sim.TokenURL(), HTTPClient: sim.Client(),
	})
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	link := sim.AuthURL() + "?" + url.Values{
		"client_id": {googlesim.AssistantClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {google.MailScope + " " + google.SendScope}, "access_type": {"offline"}, "state": {"state"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	back, err := url.Parse(sim.Consent(link, account, googlesim.Grant{Mail: true, Send: true}))
	if err != nil {
		return "", err
	}
	granted, err := tokens.Exchange(context.Background(), back.Query().Get("code"), verifier)
	if err != nil {
		return "", err
	}
	return granted.Refresh, nil
}

// deliverAssistantMail delivers the section's mail to the eval principal's tagged address, with
// Gmail's authentication results for each sender, once. A sent message lands in the mailbox as the
// assistant sent it for the principal, with the tagged address as its Reply-To, which is how the
// poller records it as theirs.
func (w *World) deliverAssistantMail() error {
	section := w.assistantMailSeed
	w.assistantMailSeed.Messages = nil
	address := cmp.Or(section.Address, DefaultAssistantMailbox)
	local, domain, _ := strings.Cut(address, "@")
	tagged := local + "+" + cmp.Or(section.Tag, DefaultAssistantTag) + "@" + domain
	now := w.Clock.Now()
	for i, message := range section.Messages {
		received := now
		if message.Received != "" {
			var err error
			if received, err = w.parseLocal(message.Received); err != nil {
				return err
			}
			if received.Before(now) {
				return fmt.Errorf("assistant_mail.messages[%d]: received %s is before the case's clock, so it would never be triaged", i, message.Received)
			}
		}
		if message.Sent {
			w.assistantMail.DeliverTo(address, googlesim.Message{
				ThreadID: message.ThreadID, From: "Assistant <" + address + ">", To: message.To, ReplyTo: tagged,
				Subject: message.Subject, PlainText: message.Body, Received: received, Labels: []string{"SENT"},
			})
			continue
		}
		sender, err := mail.ParseAddress(message.From)
		if err != nil {
			return fmt.Errorf("assistant_mail.messages[%d].from: %w", i, err)
		}
		w.assistantMail.DeliverTo(tagged, googlesim.Message{
			ThreadID: message.ThreadID, From: message.From, To: cmp.Or(message.To, tagged), Subject: message.Subject,
			PlainText: message.Body, Received: received,
			AuthenticationResults: googlesim.AuthenticationResults(sender.Address, cmp.Or(message.DMARC, "pass")),
		})
	}
	return nil
}
