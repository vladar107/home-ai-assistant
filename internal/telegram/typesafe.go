package telegram

import "context"

const (
	typeSafeSectionPrefix = "typesafe"
	typeSafeOnAction      = "on"
	typeSafeOffAction     = "off"

	typeSafeDescription = "When on, a scrubbed copy of your recent messages, of web pages and emails the assistant reads, and of proposed actions is sent to TypeSafe to warn you about actions you did not ask for and content that tries to instruct the assistant. File paths, links, email addresses, phone numbers, bank and card numbers, and keys are removed first; names and other free text may remain. Notion page contents and NAS file contents are never sent; page titles, file names, and job names may be."

	TypeSafeOnLine  = "Safety checks (TypeSafe): on.\n" + typeSafeDescription
	TypeSafeOffLine = "Safety checks (TypeSafe): off.\n" + typeSafeDescription
	// TypeSafePrivacyLine is added to /privacy while the principal's safety checks are on.
	TypeSafePrivacyLine = "Safety checks are on: a scrubbed copy of your recent messages, of web pages and emails I read, and of proposed actions is sent to TypeSafe. It is off by default; you can turn it off in /settings."
)

// TypeSafeController is implemented by the judgment opt-in service; nil disables the safety checks section.
type TypeSafeController interface {
	Status(context.Context, string) (bool, error)
	Enable(context.Context, string) error
	Disable(context.Context, string) error
}

type typeSafeSection struct {
	typeSafe TypeSafeController
}

func (t typeSafeSection) prefix() string { return typeSafeSectionPrefix }

func (t typeSafeSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := t.typeSafe.Status(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines:   []string{TypeSafeOnLine},
			buttons: []InlineButton{{Text: "Turn off safety checks", CallbackData: typeSafeSectionPrefix + ":" + typeSafeOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{TypeSafeOffLine},
		buttons: []InlineButton{{Text: "Turn on safety checks", CallbackData: typeSafeSectionPrefix + ":" + typeSafeOnAction}},
	}, nil
}

func (t typeSafeSection) act(ctx context.Context, principalID string, _ int64, action string) (string, error) {
	switch action {
	case typeSafeOnAction:
		if err := t.typeSafe.Enable(ctx, principalID); err != nil {
			return "", err
		}
	case typeSafeOffAction:
		if err := t.typeSafe.Disable(ctx, principalID); err != nil {
			return "", err
		}
	default:
		return "", errUnknownSettingsAction
	}
	return "", nil
}
