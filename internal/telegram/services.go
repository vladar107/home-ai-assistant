package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
)

const (
	mcpSectionPrefix    = "mcp"
	mcpOnAction         = "on"
	mcpOffAction        = "off"
	mcpConnectAction    = "connect"
	mcpDisconnectAction = "disconnect"

	// serviceDescriptionUnits bounds a description on the page, so a full catalog of services with
	// the longest titles stays within Telegram's message limit.
	serviceDescriptionUnits = 160

	ServicesLine = "Services outside the household that I can use for you. Each is off until you turn it on, and what you ask a service that is on goes to it. A service that needs a key uses your own: press Connect to send it."
	ServicesHelp = "Each service is off until the user presses its Turn on button, and offers nothing until then; Turn off stops it again. " +
		"While a service is on, what the user asks it goes to that service, outside the household, and what it returns is treated as outside content. " +
		"A service that needs a key works only with the user's own key: pressing its Connect button sends instructions, the user sends the key as the next message, " +
		"the assistant deletes that message at once and never shows the key again, and connecting turns the service on. Disconnect removes the key and turns the service off. " +
		"Needs a key means the service is on but has no key yet; Needs reconnection means the service moved, so the key must be connected again; " +
		"Problem means the service refused the key, so connect a new one or disconnect."
	ServicesKeyGuardReply = "That looked like a key for one of your services, so I deleted it and did not send it to the model. To connect a service, press its Connect button in /settings."
)

var serviceStateLabels = map[mcp.State]string{
	mcp.StateOff:            "Off",
	mcp.StateOn:             "On",
	mcp.StateNeedsKey:       "Needs a key",
	mcp.StateNeedsReconnect: "Needs reconnection",
	mcp.StateProblem:        "Problem",
}

func ServiceLine(status mcp.ServerStatus) string {
	return status.Title + ": " + serviceStateLabels[status.State]
}

// ServicesConnectInstructions asks for the key; the intake ends at expires.
func ServicesConnectInstructions(title string, expires time.Time) string {
	return fmt.Sprintf("Send your key for %s as your next message by %s. I will delete that message right away and never show the key again. "+
		"The key lets me use %s for you: what you ask it is sent there with your key, to a service outside the household. Connecting also turns it on.",
		title, expires.Format("15:04"), title)
}

func ServicesConnectedReply(title string) string {
	return fmt.Sprintf("%s is connected with your key and turned on.", title)
}

func ServicesConnectedOffReply(title string) string {
	return fmt.Sprintf("Your key for %s is stored, but I could not turn it on. Press Turn on %s in /settings.", title, title)
}

func ServicesNotAKeyReply(title string) string {
	return fmt.Sprintf("That did not look like a key for %s, so nothing was stored and it was sent nowhere. Press Connect %s again when you have it.", title, title)
}

func ServicesRefusedReply(title string) string {
	return fmt.Sprintf("%s refused that key, so nothing was stored. Send the right key as your next message; I am still waiting for it.", title)
}

func ServicesUnverifiedReply(title string) string {
	return fmt.Sprintf("%s could not be reached to check that key, so nothing was stored. Send it again to retry; I am still waiting for it.", title)
}

func ServicesFailedReply(title string) string {
	return fmt.Sprintf("I could not store your key for %s, so nothing was connected. Send it again to retry; I am still waiting for it.", title)
}

func ServicesExpiredReply(title string) string {
	return fmt.Sprintf("The wait for your key for %s had ended, so nothing was stored or sent. Press Connect %s again.", title, title)
}

func ServicesDisconnectedNote(title string) string {
	return fmt.Sprintf("Your key for %s is removed and the service is off.", title)
}

// ServicePrivacyLine is added to /privacy for each service the user turned on.
func ServicePrivacyLine(title string) string {
	return fmt.Sprintf("%s: what you ask it goes to that service, outside the household, and what it returns is treated as outside content.", title)
}

// ServicesController is the MCP connector as the Services page sees it.
type ServicesController interface {
	Servers(ctx context.Context, principalID string) ([]mcp.ServerStatus, error)
	Status(ctx context.Context, principalID, server string) (mcp.ServerStatus, error)
	SetEnabled(ctx context.Context, principalID, server string, enabled bool) (mcp.ServerStatus, error)
	Disconnect(ctx context.Context, principalID, server string) (bool, error)
	BeginIntake(principalID, server string) (time.Time, error)
	PendingIntake(principalID string) (string, bool)
	CancelIntake(principalID string) bool
	OfferMessage(ctx context.Context, principalID, text string) (mcp.IntakeResult, error)
	MatchingKeyServers(principalID, text string) []string
}

var _ ServicesController = (*mcp.Service)(nil)

// NewServicesSection is the page with a row per catalog server the principal may use: it turns
// servers on and off, connects and disconnects keys, and takes the message after Connect as the
// key. allowed reports whether a principal may use any server; the page is absent for the others.
func NewServicesSection(services ServicesController, allowed func(principalID string) bool, location *time.Location, logger *slog.Logger, telemetry Telemetry) Section {
	if location == nil {
		location = time.UTC
	}
	return servicesSection{services: services, allowed: allowed, location: location, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type servicesSection struct {
	services  ServicesController
	allowed   func(principalID string) bool
	location  *time.Location
	logger    *slog.Logger
	telemetry Telemetry
}

func (s servicesSection) prefix() string { return mcpSectionPrefix }

func (s servicesSection) helpTopic() string { return "services" }

var (
	_ messageInterceptor = servicesSection{}
	_ intakeSection      = servicesSection{}
)

func (s servicesSection) availableTo(principalID string) bool {
	return s.allowed(principalID)
}

func (s servicesSection) privacy(ctx context.Context, principalID string) ([]string, error) {
	statuses, err := s.services.Servers(ctx, principalID)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, status := range statuses {
		if status.Enabled {
			lines = append(lines, ServicePrivacyLine(status.Title))
		}
	}
	return lines, nil
}

func (s servicesSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	statuses, err := s.services.Servers(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	on := 0
	var stale []string
	for _, status := range statuses {
		switch status.State {
		case mcp.StateOn:
			on++
		case mcp.StateNeedsReconnect, mcp.StateProblem:
			stale = append(stale, status.Title)
		}
	}
	summary := settingsSummary{name: "Services", state: fmt.Sprintf("%d of %d on", on, len(statuses))}
	if len(stale) > 0 {
		summary.attention = "Connect your key again for " + strings.Join(stale, ", ") + " in Services."
	}
	return summary, nil
}

// render shows each server's title, state, and description, with one row of its buttons, each
// naming the server so the rows read on their own.
func (s servicesSection) render(ctx context.Context, principalID string) (settingsView, error) {
	statuses, err := s.services.Servers(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	view := settingsView{lines: []string{ServicesLine}}
	for _, status := range statuses {
		view.lines = append(view.lines, "", ServiceLine(status), clipUTF16(status.Description, serviceDescriptionUnits))
		var row []InlineButton
		toggle := InlineButton{Text: "Turn on " + status.Title, CallbackData: servicesCallback(mcpOnAction, status.Name)}
		if status.Enabled {
			toggle = InlineButton{Text: "Turn off " + status.Title, CallbackData: servicesCallback(mcpOffAction, status.Name)}
		}
		connect := InlineButton{Text: "Connect " + status.Title, CallbackData: servicesCallback(mcpConnectAction, status.Name)}
		disconnect := InlineButton{Text: "Disconnect " + status.Title, CallbackData: servicesCallback(mcpDisconnectAction, status.Name)}
		switch {
		case !status.Keyed:
			row = []InlineButton{toggle}
		case status.Key == mcp.KeyMissing:
			row = []InlineButton{toggle, connect}
		case status.Key == mcp.KeyConnected:
			row = []InlineButton{toggle, disconnect}
		default:
			// A key the server moved away from or refused is replaced or removed, not switched.
			row = []InlineButton{connect, disconnect}
		}
		view.rows = append(view.rows, row)
	}
	return view, nil
}

func servicesCallback(action, server string) string {
	return mcpSectionPrefix + ":" + action + ":" + server
}

func (s servicesSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	verb, server, found := strings.Cut(action, ":")
	if !found || server == "" {
		return settingsOutcome{}, errUnknownSettingsAction
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	switch verb {
	case mcpOnAction, mcpOffAction:
		enabled := verb == mcpOnAction
		if _, err := s.services.SetEnabled(ctx, principalID, server, enabled); err != nil {
			return settingsOutcome{}, serviceActionError(err)
		}
		logger.InfoContext(ctx, "mcp.switch.changed", "principal_id", principalID, "server", server, "enabled", enabled)
		return settingsOutcome{}, nil
	case mcpConnectAction:
		status, err := s.services.Status(ctx, principalID, server)
		if err != nil {
			return settingsOutcome{}, serviceActionError(err)
		}
		expires, err := s.services.BeginIntake(principalID, server)
		if err != nil {
			return settingsOutcome{}, serviceActionError(err)
		}
		logger.InfoContext(ctx, "mcp.intake.started", "principal_id", principalID, "server", server)
		return settingsOutcome{message: ServicesConnectInstructions(status.Title, expires.In(s.location))}, nil
	case mcpDisconnectAction:
		status, err := s.services.Status(ctx, principalID, server)
		if err != nil {
			return settingsOutcome{}, serviceActionError(err)
		}
		removed, err := s.services.Disconnect(ctx, principalID, server)
		if err != nil {
			return settingsOutcome{}, serviceActionError(err)
		}
		if !removed {
			return settingsOutcome{}, nil
		}
		return settingsOutcome{note: ServicesDisconnectedNote(status.Title)}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// serviceActionError treats a press for a server the principal may not use, or one that takes no
// key, as a stale or forged button.
func serviceActionError(err error) error {
	if errors.Is(err, mcp.ErrUnknownServer) || errors.Is(err, mcp.ErrNotAllowed) || errors.Is(err, mcp.ErrNoKeyNeeded) {
		return errUnknownSettingsAction
	}
	return err
}

// intercept offers every message to the principal's pending key intake, and keeps a message shaped
// like one of the principal's keys out of the model whether or not an intake is pending. Whatever
// the intake consumes is deleted and goes nowhere else; the text is never logged.
func (s servicesSection) intercept(ctx context.Context, principalID string, text string) (interception, error) {
	result, err := s.services.OfferMessage(ctx, principalID, text)
	if result.Outcome == mcp.IntakeNone {
		return s.guard(principalID, text), nil
	}
	title := result.Status.Title
	if result.Outcome != mcp.IntakeConnected {
		title = s.titleOf(ctx, principalID, result.Server)
	}
	consumed := interception{handled: true, remove: true, reason: "mcp_intake_" + string(result.Outcome)}
	switch result.Outcome {
	case mcp.IntakeConnected:
		consumed.reply = ServicesConnectedReply(title)
		if !result.Status.Enabled {
			consumed.reply = ServicesConnectedOffReply(title)
		}
	case mcp.IntakeNotKey:
		consumed.reply = ServicesNotAKeyReply(title)
	case mcp.IntakeRefused:
		consumed.reply = ServicesRefusedReply(title)
	case mcp.IntakeUnverified:
		consumed.reply = ServicesUnverifiedReply(title)
	case mcp.IntakeExpired:
		consumed.reply = ServicesExpiredReply(title)
	default:
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "mcp.intake.failed", "principal_id", principalID, "server", result.Server, "error", err)
		consumed.reply = ServicesFailedReply(title)
	}
	return consumed, nil
}

// titleOf names a server by its title, or by its name when the principal can no longer see it.
func (s servicesSection) titleOf(ctx context.Context, principalID, server string) string {
	status, err := s.services.Status(ctx, principalID, server)
	if err != nil || status.Title == "" {
		return server
	}
	return status.Title
}

// guard recognises a key by the key pattern of a server the principal may use; a server without
// one has keys of no recognisable shape.
func (s servicesSection) guard(principalID, text string) interception {
	if len(s.services.MatchingKeyServers(principalID, text)) == 0 {
		return interception{}
	}
	return interception{handled: true, remove: true, reply: ServicesKeyGuardReply, reason: "mcp_key_guard"}
}

func (s servicesSection) awaiting(principalID string) bool {
	_, pending := s.services.PendingIntake(principalID)
	return pending
}

// claims reports whether text matches the key pattern of the server the intake waits for.
func (s servicesSection) claims(principalID, text string) bool {
	server, pending := s.services.PendingIntake(principalID)
	return pending && slices.Contains(s.services.MatchingKeyServers(principalID, text), server)
}

func (s servicesSection) yield(principalID string) {
	server, _ := s.services.PendingIntake(principalID)
	if s.services.CancelIntake(principalID) {
		s.logger.Info("mcp.intake.yielded", "principal_id", principalID, "server", server)
	}
}
