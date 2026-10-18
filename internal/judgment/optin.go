package judgment

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

// OptInSwitch is each principal's TypeSafe switch. It is off until the principal turns it on.
var OptInSwitch = integration.Switch{Integration: "typesafe", Name: "Safety checks"}

// NewOptIn reads the opt-in from the principal's TypeSafe switch.
func NewOptIn(switches integration.SwitchReader) OptIn {
	return switchOptIn{switches: switches}
}

type switchOptIn struct {
	switches integration.SwitchReader
}

func (o switchOptIn) JudgmentsEnabled(ctx context.Context, principalID string) (bool, error) {
	return o.switches.Enabled(ctx, principalID, OptInSwitch)
}
