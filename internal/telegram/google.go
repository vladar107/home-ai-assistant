package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

const (
	googleSectionPrefix    = "google"
	googleConnectAction    = "connect"
	googleDisconnectAction = "disconnect"

	GoogleNotConnectedLine   = "Google: no accounts connected."
	GoogleAccountsLine       = "Google accounts:"
	GooglePrivacyLine        = "Mail and calendar events I read go to your model."
	GoogleSignInInstructions = "To connect a Google account read-only:\n" +
		"1. Open the link below and sign in with the Google account to connect. Google will say the app is unverified; it is the household's own app, so continue.\n" +
		"2. Allow reading your email, your calendar, or both. Nothing can send, change, or delete anything.\n" +
		"3. Your browser then fails to open a localhost page. Copy that page's full address and send it here within 10 minutes. I will delete it right away.\n\n"
	GoogleNoSignInReply      = "That sign-in address does not match a pending sign-in, or it expired, so nothing was connected. Press Connect a Google account in /settings to start again. I deleted the message."
	GoogleDeniedReply        = "Neither Gmail nor Calendar access was granted, so nothing was connected. Press Connect a Google account in /settings and allow at least one."
	GoogleRejectedReply      = "Google refused that sign-in, so nothing was connected. Codes work once and only for a few minutes; press Connect a Google account in /settings to try again."
	GoogleTakenReply         = "That Google account is already connected for another household member, so it was not connected for you."
	GoogleConnectFailedReply = "Google could not be reached to finish the sign-in, so nothing was connected. Please try again later."
	GoogleNotGrantedReply    = "That account did not allow it when it was connected. Press Connect a Google account, sign in with the same account, and leave it ticked."
	GoogleSignInGuardReply   = "That looked like a Google sign-in address, so I deleted it and did not send it to the model. To finish signing in, send the address on its own as a text message."
)

// GoogleController is implemented by the Google connector; nil disables the Google settings section.
type GoogleController interface {
	Status(context.Context, string) ([]google.AccountStatus, error)
	BeginSignIn(string) (string, error)
	CompleteSignIn(context.Context, string, string) (google.AccountStatus, error)
	SetProduct(ctx context.Context, principalID, accountID string, product google.Product, enabled bool) (bool, error)
	Disconnect(ctx context.Context, principalID, accountID string) (bool, error)
}

func productState(granted, enabled bool) string {
	switch {
	case !granted:
		return "not allowed"
	case enabled:
		return "on"
	default:
		return "off"
	}
}

func GoogleAccountLine(status google.AccountStatus) string {
	if status.NeedsReconnect {
		return fmt.Sprintf("%s: Google no longer accepts the connection. Reconnect or disconnect.", status.Address)
	}
	return fmt.Sprintf("%s: Mail %s, Calendar %s.", status.Address,
		productState(status.MailGranted, status.MailEnabled), productState(status.CalendarGranted, status.CalendarEnabled))
}

func GoogleConnectedReply(status google.AccountStatus) string {
	return fmt.Sprintf("%s is connected read-only: Mail %s, Calendar %s.", status.Address,
		productState(status.MailGranted, status.MailEnabled), productState(status.CalendarGranted, status.CalendarEnabled))
}

type googleSection struct {
	google    GoogleController
	logger    *slog.Logger
	telemetry Telemetry
}

func (g googleSection) prefix() string { return googleSectionPrefix }

func (g googleSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	statuses, err := g.google.Status(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	summary := settingsSummary{name: "Google", state: "not connected"}
	switch len(statuses) {
	case 0:
	case 1:
		summary.state = "1 account"
	default:
		summary.state = fmt.Sprintf("%d accounts", len(statuses))
	}
	var stale []string
	for _, status := range statuses {
		if status.NeedsReconnect {
			stale = append(stale, status.Address)
		}
	}
	if len(stale) > 0 {
		summary.attention = "Reconnect " + strings.Join(stale, ", ") + " in Accounts › Google."
	}
	return summary, nil
}

// render lists every account under a header button showing its address, followed by a row of its
// action buttons.
func (g googleSection) render(ctx context.Context, principalID string) (settingsView, error) {
	statuses, err := g.google.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	view := settingsView{buttons: []InlineButton{{Text: "Connect a Google account", CallbackData: googleSectionPrefix + ":" + googleConnectAction}}}
	if len(statuses) == 0 {
		view.lines = []string{GoogleNotConnectedLine}
		return view, nil
	}
	view.lines = []string{GoogleAccountsLine}
	for _, status := range statuses {
		view.lines = append(view.lines, GoogleAccountLine(status))
		view.rows = append(view.rows, []InlineButton{{Text: status.Address, CallbackData: labelCallbackData}})
		var row []InlineButton
		if !status.NeedsReconnect {
			if status.MailGranted {
				row = append(row, productButton(status.ID, google.ProductMail, "Mail", status.MailEnabled))
			}
			if status.CalendarGranted {
				row = append(row, productButton(status.ID, google.ProductCalendar, "Calendar", status.CalendarEnabled))
			}
		}
		row = append(row, InlineButton{
			Text:         "Disconnect",
			CallbackData: googleSectionPrefix + ":" + googleDisconnectAction + ":" + status.ID,
		})
		view.rows = append(view.rows, row)
	}
	return view, nil
}

func productButton(accountID string, product google.Product, name string, enabled bool) InlineButton {
	turn, state := "on", "on"
	if enabled {
		turn, state = "off", "off"
	}
	return InlineButton{
		Text:         name + " " + turn,
		CallbackData: googleSectionPrefix + ":" + string(product) + "-" + state + ":" + accountID,
	}
}

func (g googleSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	if action == googleConnectAction {
		link, err := g.google.BeginSignIn(principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		g.telemetry.ContextLogger(ctx, g.logger).InfoContext(ctx, "google.signin.started", "principal_id", principalID)
		return settingsOutcome{message: GoogleSignInInstructions + link}, nil
	}
	verb, accountID, found := strings.Cut(action, ":")
	if !found || accountID == "" {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	switch verb {
	case googleDisconnectAction:
		_, err := g.google.Disconnect(ctx, principalID, accountID)
		return settingsOutcome{}, err
	case "mail-on", "mail-off", "calendar-on", "calendar-off":
		product, state, _ := strings.Cut(verb, "-")
		_, err := g.google.SetProduct(ctx, principalID, accountID, google.Product(product), state == "on")
		if errors.Is(err, google.ErrNotGranted) {
			return settingsOutcome{note: GoogleNotGrantedReply}, nil
		}
		return settingsOutcome{}, err
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// intercept consumes any message carrying a sign-in address, pending sign-in or not, so an
// authorization code never reaches the model or stays in the chat. The text is never logged.
func (g googleSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if !google.LooksLikeRedirect(text) {
		return interception{}, nil
	}
	status, err := g.google.CompleteSignIn(ctx, principalID, text)
	result := interception{handled: true, remove: true}
	switch {
	case err == nil:
		result.reply, result.reason = GoogleConnectedReply(status), "google_signin_connected"
	case errors.Is(err, google.ErrNoSignIn):
		result.reply, result.reason = GoogleNoSignInReply, "google_signin_unmatched"
	case errors.Is(err, google.ErrDenied):
		result.reply, result.reason = GoogleDeniedReply, "google_signin_denied"
	case errors.Is(err, google.ErrRejected):
		result.reply, result.reason = GoogleRejectedReply, "google_signin_rejected"
	case errors.Is(err, google.ErrAccountTaken):
		result.reply, result.reason = GoogleTakenReply, "google_signin_taken"
	default:
		g.telemetry.ContextLogger(ctx, g.logger).WarnContext(ctx, "google.signin.failed", "principal_id", principalID, "error", err)
		result.reply, result.reason = GoogleConnectFailedReply, "google_signin_failed"
	}
	return result, nil
}

func (g googleSection) guard(text string) interception {
	if !google.LooksLikeRedirect(text) {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: GoogleSignInGuardReply, reason: "google_signin_guard"}
}
