package telegram

import (
	"context"
	"log/slog"
	"strings"
)

const (
	switchOnAction  = "on"
	switchOffAction = "off"
)

// SwitchPage is the text of an integration's on/off page in /settings.
type SwitchPage struct {
	// Integration is the name of the integration the page switches, as its integration.Switch has it.
	Integration string
	// Prefix starts the page's callback data and places it in settingsGroups.
	Prefix string
	// Name labels the page in the overview and in its group.
	Name string
	// OnLine and OffLine describe the integration in each state; ExtraOnLines follow OnLine.
	OnLine, OffLine string
	ExtraOnLines    []string
	// OnButton and OffButton turn the integration on and off.
	OnButton, OffButton string
	// Event is logged when a principal turns the integration on or off; empty logs nothing.
	Event string
	// HelpTopic names the page for app_help.
	HelpTopic string
	// PrivacyLine is added to /privacy while the integration is on.
	PrivacyLine string
	// SummaryState words the state for the overview; nil says on or off.
	SummaryState func(on bool) string
}

// SwitchDetails adds lines to a switch page while its integration is on, and names what the user
// has to act on, if anything.
type SwitchDetails interface {
	Details(ctx context.Context) (lines []string, attention string)
}

// SwitchSettings is one integration's on/off page: its text, the principal's switch, and what the
// page adds beyond the switch.
type SwitchSettings struct {
	Page   SwitchPage
	Toggle SwitchController
	// Details is optional.
	Details SwitchDetails
	// Allowed limits the page to some principals, such as those the operator configured; nil shows
	// it to everyone.
	Allowed func(principalID string) bool
}

// NewSwitchSection is the /settings page that turns one integration on and off.
func NewSwitchSection(settings SwitchSettings, logger *slog.Logger, telemetry Telemetry) Section {
	return switchSection{settings: settings, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type switchSection struct {
	settings  SwitchSettings
	logger    *slog.Logger
	telemetry Telemetry
}

func (s switchSection) prefix() string { return s.settings.Page.Prefix }

func (s switchSection) helpTopic() string { return s.settings.Page.HelpTopic }

func (s switchSection) availableTo(principalID string) bool {
	return s.settings.Allowed == nil || s.settings.Allowed(principalID)
}

func (s switchSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := s.settings.Toggle.Enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	state := onOff(enabled)
	if s.settings.Page.SummaryState != nil {
		state = s.settings.Page.SummaryState(enabled)
	}
	summary := settingsSummary{name: s.settings.Page.Name, state: state}
	if s.settings.Details != nil {
		if _, attention := s.settings.Details.Details(ctx); enabled {
			summary.attention = attention
		}
	}
	return summary, nil
}

func (s switchSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := s.settings.Toggle.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	page := s.settings.Page
	if !enabled {
		return settingsView{
			lines:   []string{page.OffLine},
			buttons: []InlineButton{{Text: page.OnButton, CallbackData: page.Prefix + ":" + switchOnAction}},
		}, nil
	}
	lines := append([]string{page.OnLine}, page.ExtraOnLines...)
	if s.settings.Details != nil {
		details, _ := s.settings.Details.Details(ctx)
		lines = append(lines, details...)
	}
	return settingsView{
		lines:   lines,
		buttons: []InlineButton{{Text: page.OffButton, CallbackData: page.Prefix + ":" + switchOffAction}},
	}, nil
}

func (s switchSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	if action != switchOnAction && action != switchOffAction {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	enabled := action == switchOnAction
	if err := s.settings.Toggle.SetEnabled(ctx, principalID, enabled); err != nil {
		return settingsOutcome{}, err
	}
	if s.settings.Page.Event != "" {
		s.telemetry.ContextLogger(ctx, s.logger).InfoContext(ctx, s.settings.Page.Event, "principal_id", principalID, "enabled", enabled)
	}
	return settingsOutcome{}, nil
}

func (s switchSection) privacy(ctx context.Context, principalID string) ([]string, error) {
	enabled, err := s.settings.Toggle.Enabled(ctx, principalID)
	if err != nil || !enabled {
		return nil, err
	}
	return []string{s.settings.Page.PrivacyLine}, nil
}

// HomeDetails shows the Home Assistant connector's exposure check on the home page: whether it
// could be made, and the devices that need confirmation but are exposed to Assist.
func HomeDetails(exposure func(context.Context) (checked bool, conflicts []string)) SwitchDetails {
	return homeDetails(exposure)
}

type homeDetails func(context.Context) (bool, []string)

func (h homeDetails) Details(ctx context.Context) ([]string, string) {
	checked, conflicts := h(ctx)
	switch {
	case !checked:
		return []string{HomeAssistantUncheckedLine}, homeAssistantPausedAttention
	case len(conflicts) > 0:
		return []string{HomeAssistantConflictLine + strings.Join(conflicts, "; ") + "."}, homeAssistantPausedAttention
	default:
		return nil, ""
	}
}
