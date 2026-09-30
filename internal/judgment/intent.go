package judgment

import (
	"context"
	"fmt"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

// Calibrated in plan 0029: requested actions scored 0.60 and above, unrequested ones 0.06 and
// below.
const (
	intentWarnBelow        = 0.3
	intentWarnBelowExposed = 0.5
)

const intentQuestionID = "requested"

// RecentMessages returns a principal's latest user messages, oldest first.
type RecentMessages interface {
	RecentMessages(principalID string) []string
}

// ExposureLevels reports how much untrusted content a principal's turn has seen.
type ExposureLevels interface {
	Exposure(principalID string) turnstate.Level
}

// IntentChecker judges whether a proposed action matches what the user recently asked for.
type IntentChecker struct {
	client   *Client
	optIn    OptIn
	recent   RecentMessages
	exposure ExposureLevels
}

var _ approval.IntentChecker = (*IntentChecker)(nil)

func NewIntentChecker(client *Client, optIn OptIn, recent RecentMessages, exposure ExposureLevels) *IntentChecker {
	return &IntentChecker{client: client, optIn: optIn, recent: recent, exposure: exposure}
}

// Mismatch reports true when the action does not look requested. Any missing input means no
// judgment and false, so the check never blocks a proposal on its own.
func (c *IntentChecker) Mismatch(ctx context.Context, principalID, tool string, intent approval.Intent) (bool, error) {
	enabled, err := c.optIn.JudgmentsEnabled(ctx, principalID)
	if err != nil || !enabled {
		return false, nil
	}
	level := c.exposure.Exposure(principalID)
	if level == turnstate.LevelSuspicious {
		return true, nil
	}
	messages := c.recent.RecentMessages(principalID)
	if len(messages) == 0 {
		return false, nil
	}
	redacted := make([]string, len(messages))
	for index, message := range messages {
		redacted[index] = Redact(message).String()
	}
	action := fmt.Sprintf("%s %s %q using tool %s", intent.Verb, intent.TargetKind, intent.TargetName, tool)
	answers, err := c.client.Ask(ctx, "intent",
		map[string]Redacted{
			"recent_messages": Redact(strings.Join(redacted, "\n---\n")),
			"action":          Redact(action),
		},
		map[string]Question{intentQuestionID: Noul(
			"Did the user, in `recent_messages`, ask for or clearly agree to the action in `action`? A short confirmation such as 'yes' or 'do it' counts as agreeing to an action the assistant proposed.",
			"The user asked for or agreed to this action.",
			"The user did not ask for this action, or asked for something different.",
		)},
	)
	if err != nil {
		return false, err
	}
	probability, ok := answers.Noul(intentQuestionID)
	if !ok {
		return false, nil
	}
	threshold := intentWarnBelow
	if level == turnstate.LevelExposed {
		threshold = intentWarnBelowExposed
	}
	return probability < threshold, nil
}
