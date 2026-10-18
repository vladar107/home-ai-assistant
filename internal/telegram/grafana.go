package telegram

import (
	"context"
	"log/slog"
)

const (
	grafanaSectionPrefix = "grafana"
	grafanaOnAction      = "on"
	grafanaOffAction     = "off"

	GrafanaOnLine      = "Monitoring (Grafana): on (the default). I can read the household's dashboards, query metrics, and search container logs."
	GrafanaOffLine     = "Monitoring (Grafana): off. Turn it back on to let me read the household's dashboards, query metrics, and search container logs."
	GrafanaPrivacyLine = "Monitoring: metrics and logs you ask about go to your model and can show other household members' activity."
)

// GrafanaController is the Grafana switch beside the connector's allowlist; nil leaves monitoring
// out of /settings and /privacy.
type GrafanaController interface {
	SwitchController
	Allowed(string) bool
}

// SetGrafana adds the monitoring section to /settings and its line to /privacy for the principals
// the operator allowed; call it before Start.
func (s *Service) SetGrafana(controller GrafanaController) {
	if controller == nil {
		return
	}
	s.grafana = controller
	s.sections = append(s.sections, grafanaSection{grafana: controller, logger: s.logger, telemetry: s.telemetry})
}

type grafanaSection struct {
	grafana   GrafanaController
	logger    *slog.Logger
	telemetry Telemetry
}

func (g grafanaSection) prefix() string { return grafanaSectionPrefix }

func (g grafanaSection) availableTo(principalID string) bool { return g.grafana.Allowed(principalID) }

func (g grafanaSection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := g.grafana.Enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Grafana", state: onOff(enabled)}, nil
}

func (g grafanaSection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := g.grafana.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		return settingsView{
			lines:   []string{GrafanaOnLine},
			buttons: []InlineButton{{Text: "Turn off monitoring", CallbackData: grafanaSectionPrefix + ":" + grafanaOffAction}},
		}, nil
	}
	return settingsView{
		lines:   []string{GrafanaOffLine},
		buttons: []InlineButton{{Text: "Turn on monitoring", CallbackData: grafanaSectionPrefix + ":" + grafanaOnAction}},
	}, nil
}

func (g grafanaSection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case grafanaOnAction, grafanaOffAction:
		enabled := action == grafanaOnAction
		if err := g.grafana.SetEnabled(ctx, principalID, enabled); err != nil {
			return settingsOutcome{}, err
		}
		g.telemetry.ContextLogger(ctx, g.logger).InfoContext(ctx, "grafana.changed", "principal_id", principalID, "enabled", enabled)
		return settingsOutcome{}, nil
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}
