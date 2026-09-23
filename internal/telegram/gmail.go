package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
)

const (
	gmailSectionPrefix    = "gmail"
	gmailConnectAction    = "connect"
	gmailDisconnectAction = "disconnect"

	GmailNotConnectedLine   = "Gmail: not connected."
	GmailReconnectLine      = "Gmail: Google no longer accepts the connection. Reconnect or disconnect."
	GmailPrivacyLine        = "Email the model reads through your Gmail connection is sent to your model subscription as well."
	GmailSignInInstructions = "To connect Gmail read-only:\n" +
		"1. Open the link below and sign in with the Google account to connect. Google will say the app is unverified; it is the household's own app, so continue.\n" +
		"2. Allow reading your email. Nothing can send, change, or delete mail.\n" +
		"3. Your browser then fails to open a localhost page. Copy that page's full address and send it here within 10 minutes. I will delete it right away.\n\n"
	GmailConnectedReply     = "Gmail is connected read-only as %s."
	GmailNoSignInReply      = "That sign-in address does not match a pending sign-in, or it expired, so nothing was connected. Press Connect Gmail in /settings to start again. I deleted the message."
	GmailDeniedReply        = "Read access to Gmail was not granted, so nothing was connected. Press Connect Gmail in /settings and allow reading your email."
	GmailRejectedReply      = "Google refused that sign-in, so nothing was connected. Codes work once and only for a few minutes; press Connect Gmail in /settings to try again."
	GmailTakenReply         = "That mailbox is already connected for another household member, so it was not connected for you."
	GmailConnectFailedReply = "Google could not be reached to finish the sign-in, so nothing was connected. Please try again later."
)

// GmailController is implemented by the Gmail connector; nil disables the Gmail settings section.
type GmailController interface {
	Status(context.Context, string) (gmail.Status, error)
	BeginSignIn(string) (string, error)
	CompleteSignIn(context.Context, string, string) (gmail.Status, error)
	Disconnect(context.Context, string) (bool, error)
}

func GmailConnectedLine(status gmail.Status) string {
	return "Gmail: connected read-only as " + status.Address + "."
}

type gmailSection struct {
	gmail     GmailController
	logger    *slog.Logger
	telemetry Telemetry
}

func (g gmailSection) prefix() string { return gmailSectionPrefix }

func (g gmailSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := g.gmail.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	connect := InlineButton{Text: "Connect Gmail", CallbackData: gmailSectionPrefix + ":" + gmailConnectAction}
	disconnect := InlineButton{Text: "Disconnect Gmail", CallbackData: gmailSectionPrefix + ":" + gmailDisconnectAction}
	switch {
	case status.NeedsReconnect:
		connect.Text = "Reconnect Gmail"
		return settingsView{lines: []string{GmailReconnectLine}, buttons: []InlineButton{connect, disconnect}}, nil
	case status.Connected:
		return settingsView{lines: []string{GmailConnectedLine(status)}, buttons: []InlineButton{disconnect}}, nil
	default:
		return settingsView{lines: []string{GmailNotConnectedLine}, buttons: []InlineButton{connect}}, nil
	}
}

func (g gmailSection) act(ctx context.Context, principalID string, _ int64, action string) (string, error) {
	switch action {
	case gmailConnectAction:
		link, err := g.gmail.BeginSignIn(principalID)
		if err != nil {
			return "", err
		}
		g.telemetry.ContextLogger(ctx, g.logger).InfoContext(ctx, "gmail.signin.started", "principal_id", principalID)
		return GmailSignInInstructions + link, nil
	case gmailDisconnectAction:
		if _, err := g.gmail.Disconnect(ctx, principalID); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", errUnknownSettingsAction
	}
}

// intercept consumes any message carrying a sign-in address, pending sign-in or not, so an
// authorization code never reaches the model or stays in the chat. The text is never logged.
func (g gmailSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if !gmail.LooksLikeRedirect(text) {
		return interception{}, nil
	}
	status, err := g.gmail.CompleteSignIn(ctx, principalID, text)
	result := interception{handled: true, remove: true}
	switch {
	case err == nil:
		result.reply, result.reason = fmt.Sprintf(GmailConnectedReply, status.Address), "gmail_signin_connected"
	case errors.Is(err, gmail.ErrNoSignIn):
		result.reply, result.reason = GmailNoSignInReply, "gmail_signin_unmatched"
	case errors.Is(err, gmail.ErrDenied):
		result.reply, result.reason = GmailDeniedReply, "gmail_signin_denied"
	case errors.Is(err, gmail.ErrRejected):
		result.reply, result.reason = GmailRejectedReply, "gmail_signin_rejected"
	case errors.Is(err, gmail.ErrMailboxTaken):
		result.reply, result.reason = GmailTakenReply, "gmail_signin_taken"
	default:
		g.telemetry.ContextLogger(ctx, g.logger).WarnContext(ctx, "gmail.signin.failed", "principal_id", principalID, "error", err)
		result.reply, result.reason = GmailConnectFailedReply, "gmail_signin_failed"
	}
	return result, nil
}
