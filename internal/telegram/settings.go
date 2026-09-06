package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
)

const (
	SettingsTitle         = "Settings"
	SettingsPrivacyLine   = "Privacy mode: strict-local. Use /privacy for details."
	ModelNotConnectedLine = "Model subscription: not connected."

	maximumCallbackDataBytes = 64
)

var errUnknownSettingsAction = errors.New("unknown settings action")

// settingsSection renders one group of a principal's settings and performs the actions behind
// the buttons it rendered. Callback data is "<prefix>:<action>".
type settingsSection interface {
	prefix() string
	render(context.Context, string) (settingsView, error)
	// act performs a validated action and returns a plain message for the user when one is
	// needed beyond the refreshed view; an unknown action yields errUnknownSettingsAction.
	act(ctx context.Context, principalID string, chatID int64, action string) (string, error)
}

type settingsView struct {
	lines   []string
	buttons []InlineButton
}

// messageInterceptor lets a section consume an ordinary message before it is enqueued for the
// model, for deterministic intake such as a pasted token.
type messageInterceptor interface {
	intercept(ctx context.Context, principalID string, text string) (interception, error)
}

type interception struct {
	handled bool
	// remove deletes the user's message before the reply is sent.
	remove bool
	reply  string
	reason string
}

func renderSettings(ctx context.Context, principalID string, sections []settingsSection) (string, InlineKeyboard, error) {
	lines := []string{SettingsTitle, ""}
	var keyboard InlineKeyboard
	for _, section := range sections {
		view, err := section.render(ctx, principalID)
		if err != nil {
			return "", nil, err
		}
		lines = append(lines, view.lines...)
		if len(view.buttons) > 0 {
			keyboard = append(keyboard, view.buttons)
		}
	}
	lines = append(lines, SettingsPrivacyLine)
	return strings.Join(lines, "\n"), keyboard, nil
}

// settingsSectionFor resolves callback data to its section and action; anything outside the fixed
// vocabulary resolves to nothing so a stale or forged button only refreshes the view.
func settingsSectionFor(sections []settingsSection, data string) (settingsSection, string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return nil, "", false
	}
	prefix, action, found := strings.Cut(data, ":")
	if !found || action == "" {
		return nil, "", false
	}
	for _, section := range sections {
		if section.prefix() == prefix {
			return section, action, true
		}
	}
	return nil, "", false
}

const (
	modelSectionPrefix    = "model"
	modelConnectAction    = "connect"
	modelDisconnectAction = "disconnect"
)

type modelSection struct {
	connections  ConnectionController
	conversation ConversationController
	logger       *slog.Logger
	telemetry    Telemetry
}

func (m modelSection) prefix() string { return modelSectionPrefix }

func (m modelSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := m.connections.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if !status.Connected {
		return settingsView{
			lines:   []string{ModelNotConnectedLine},
			buttons: []InlineButton{{Text: "Connect model", CallbackData: modelSectionPrefix + ":" + modelConnectAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{ModelConnectedLine(status)},
		buttons: []InlineButton{{Text: "Disconnect model", CallbackData: modelSectionPrefix + ":" + modelDisconnectAction}},
	}, nil
}

func (m modelSection) act(ctx context.Context, principalID string, chatID int64, action string) (string, error) {
	switch action {
	case modelConnectAction:
		ceremony, err := m.connections.Connect(ctx, principalID, chatID)
		if err != nil {
			m.telemetry.ContextLogger(ctx, m.logger).WarnContext(ctx, "telegram.connection.start_failed", "principal_id", principalID, "error", err)
			return connection.ConnectFailedReply, nil
		}
		return ConnectInstructions(ceremony), nil
	case modelDisconnectAction:
		if m.conversation != nil {
			if _, err := m.conversation.Cancel(ctx, principalID); err != nil {
				return "", err
			}
		}
		if _, err := m.connections.Disconnect(ctx, principalID); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", errUnknownSettingsAction
	}
}

const (
	notionSectionPrefix    = "notion"
	notionConnectAction    = "connect"
	notionDisconnectAction = "disconnect"

	NotionNotConnectedLine    = "Notion: not connected."
	NotionOperatorLine        = "Notion: not connected. Ask the operator to install your token with scripts/notion-set-token."
	NotionInvalidTokenLine    = "Notion: the installed token could not be used. Install a fresh token or disconnect."
	NotionUnavailableLine     = "Notion: the installed token could not be verified because Notion is unavailable. Try again later."
	NotionPrivacyLine         = "Notion content read through your own connection is sent to your model subscription as well."
	NotionConnectInstructions = "Send your Notion token as your next message within 10 minutes. I will delete that message right away and never show the token again. " +
		"Recommended: an internal connection with read, update, and insert content capabilities, shared only with the pages and data sources you want me to see. " +
		"A personal access token also works, but it grants everything you can access and expires. " +
		"Notion content read through the connection is sent to your own model subscription."
	NotionNotATokenReply     = "That did not look like a Notion token, so nothing was stored and it was not sent to the model. Press Connect Notion again when you have one."
	NotionInvalidTokenReply  = "Notion rejected that token, so nothing was stored. Check it in Notion's developer portal and press Connect Notion again."
	NotionConnectFailedReply = "Notion could not be reached to verify the token, so nothing was stored. Please try again later."
	NotionTokenGuardReply    = "That looked like a Notion token, so I deleted it and did not send it to the model. To connect Notion, use Connect Notion in /settings or ask the operator to install it."
	DeleteFailedLine         = "I could not delete your message; please delete it yourself."
)

func notionKind(status notion.Status) string {
	if status.TokenKind == "personal" {
		return "personal access token"
	}
	return "internal connection"
}

func NotionConnectedLine(status notion.Status) string {
	writes := "off"
	if status.WritesEnabled {
		writes = "on"
	}
	if status.WorkspaceName == "" {
		return fmt.Sprintf("Notion: connected as %q (%s), writes %s.", status.BotName, notionKind(status), writes)
	}
	return fmt.Sprintf("Notion: connected to workspace %q as %q (%s), writes %s.", status.WorkspaceName, status.BotName, notionKind(status), writes)
}

func NotionConnectedReply(status notion.Status) string {
	if status.WorkspaceName == "" {
		return fmt.Sprintf("Notion is connected as %q (%s).", status.BotName, notionKind(status))
	}
	return fmt.Sprintf("Notion is connected to workspace %q as %q (%s).", status.WorkspaceName, status.BotName, notionKind(status))
}

type notionSection struct {
	notion    NotionController
	logger    *slog.Logger
	telemetry Telemetry
}

func (n notionSection) prefix() string { return notionSectionPrefix }

func (n notionSection) render(ctx context.Context, principalID string) (settingsView, error) {
	status, err := n.notion.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	disconnect := []InlineButton{{Text: "Disconnect Notion", CallbackData: notionSectionPrefix + ":" + notionDisconnectAction}}
	switch {
	case status.Problem == notion.ProblemInvalidToken:
		return settingsView{lines: []string{NotionInvalidTokenLine}, buttons: disconnect}, nil
	case status.Problem == notion.ProblemUnavailable:
		return settingsView{lines: []string{NotionUnavailableLine}, buttons: disconnect}, nil
	case status.Connected:
		return settingsView{lines: []string{NotionConnectedLine(status)}, buttons: disconnect}, nil
	case n.notion.ChatConnectEnabled():
		return settingsView{
			lines:   []string{NotionNotConnectedLine},
			buttons: []InlineButton{{Text: "Connect Notion", CallbackData: notionSectionPrefix + ":" + notionConnectAction}},
		}, nil
	default:
		return settingsView{lines: []string{NotionOperatorLine}}, nil
	}
}

func (n notionSection) act(ctx context.Context, principalID string, _ int64, action string) (string, error) {
	switch action {
	case notionConnectAction:
		if !n.notion.ChatConnectEnabled() {
			return "", errUnknownSettingsAction
		}
		n.notion.BeginIntake(principalID)
		n.telemetry.ContextLogger(ctx, n.logger).InfoContext(ctx, "notion.intake.started", "principal_id", principalID)
		return NotionConnectInstructions, nil
	case notionDisconnectAction:
		if _, err := n.notion.Disconnect(ctx, principalID); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", errUnknownSettingsAction
	}
}

// intercept consumes the message after Connect Notion as the token, and keeps any token-looking
// message out of the model whether or not an intake is pending. The text is never logged.
func (n notionSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	if n.notion.ConsumeIntake(principalID) {
		if !notion.LooksLikeToken(text) {
			return interception{handled: true, reply: NotionNotATokenReply, reason: "notion_intake_not_token"}, nil
		}
		status, err := n.notion.Connect(ctx, principalID, text)
		switch {
		case errors.Is(err, notion.ErrInvalidToken):
			return interception{handled: true, remove: true, reply: NotionInvalidTokenReply, reason: "notion_intake_rejected"}, nil
		case err != nil:
			n.telemetry.ContextLogger(ctx, n.logger).WarnContext(ctx, "notion.intake.failed", "principal_id", principalID, "error", err)
			return interception{handled: true, remove: true, reply: NotionConnectFailedReply, reason: "notion_intake_failed"}, nil
		default:
			return interception{handled: true, remove: true, reply: NotionConnectedReply(status), reason: "notion_intake_connected"}, nil
		}
	}
	if notion.LooksLikeToken(text) {
		return interception{handled: true, remove: true, reply: NotionTokenGuardReply, reason: "notion_token_guard"}, nil
	}
	return interception{}, nil
}
