package integration

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

// Switch is an integration each principal turns on or off for themselves. Name is how a refusal
// names it, such as "Train information"; DefaultOn is its state for a principal who never chose.
type Switch struct {
	Integration string
	Name        string
	DefaultOn   bool
}

// OffText tells the model that the principal switched the integration off.
func (s Switch) OffText() string {
	return prompt.ToolIntegrationOff(s.Name)
}

// UserOffText tells the principal that an action they confirmed did not run because they switched
// its integration off.
func (s Switch) UserOffText() string {
	return s.Name + " is off for you; turn it on in /settings and ask again."
}

// Switched is implemented by a tool provider or an approval executor whose tools all belong to one
// switchable integration; the core offers and runs them only while the principal has it on.
type Switched interface {
	Switch() Switch
}

// SwitchRepository keeps the choices principals made. A principal without one has the default.
type SwitchRepository interface {
	LoadIntegrationSwitch(ctx context.Context, principalID, integration string) (enabled, chosen bool, err error)
	SaveIntegrationSwitch(ctx context.Context, principalID, integration string, enabled bool) error
}

// SwitchReader reports a principal's switch for an integration.
type SwitchReader interface {
	Enabled(ctx context.Context, principalID string, integration Switch) (bool, error)
}

// Switches reads and changes every principal's integration switches.
type Switches struct {
	repository SwitchRepository
	logger     *slog.Logger
}

var _ SwitchReader = (*Switches)(nil)

func NewSwitches(repository SwitchRepository, logger *slog.Logger) (*Switches, error) {
	if repository == nil {
		return nil, fmt.Errorf("integration switch repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("integration switch logger is required")
	}
	return &Switches{repository: repository, logger: logger}, nil
}

func (s *Switches) Enabled(ctx context.Context, principalID string, integration Switch) (bool, error) {
	if !integrationNamePattern.MatchString(integration.Integration) {
		return false, fmt.Errorf("integration switch has an invalid name")
	}
	enabled, chosen, err := s.repository.LoadIntegrationSwitch(ctx, principalID, integration.Integration)
	if err != nil {
		return false, err
	}
	if !chosen {
		return integration.DefaultOn, nil
	}
	return enabled, nil
}

// SetEnabled records the principal's choice. It takes effect on the next tool call, including one
// later in a turn already running.
func (s *Switches) SetEnabled(ctx context.Context, principalID string, integration Switch, enabled bool) error {
	current, err := s.Enabled(ctx, principalID, integration)
	if err != nil {
		return err
	}
	if err := s.repository.SaveIntegrationSwitch(ctx, principalID, integration.Integration, enabled); err != nil {
		return err
	}
	if current != enabled {
		s.logger.InfoContext(ctx, "integration.switch.changed", "principal_id", principalID, "integration", integration.Integration, "enabled", enabled)
	}
	return nil
}

// Toggle binds the switches to one integration, for code that controls only that one.
func (s *Switches) Toggle(integration Switch) Toggle {
	return Toggle{switches: s, integration: integration}
}

// Toggle is one integration's switch for every principal.
type Toggle struct {
	switches    *Switches
	integration Switch
}

func (t Toggle) Enabled(ctx context.Context, principalID string) (bool, error) {
	return t.switches.Enabled(ctx, principalID, t.integration)
}

func (t Toggle) SetEnabled(ctx context.Context, principalID string, enabled bool) error {
	return t.switches.SetEnabled(ctx, principalID, t.integration, enabled)
}
