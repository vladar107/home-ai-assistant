package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/connection"
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
