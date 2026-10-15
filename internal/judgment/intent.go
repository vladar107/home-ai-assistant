package judgment

import (
	"context"
	"fmt"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

// Calibrated in plan 0029: requested actions scored 0.60 and above, unrequested ones 0.06 and
// below.
const (
	intentWarnBelow        = 0.3
	intentWarnBelowExposed = 0.5
)

const intentQuestionID = "requested"

// replyRunes is how much of the end of the assistant's reply is sent; the question the user
// answers is almost always there.
const replyRunes = 600

// RecentMessages returns a principal's latest user messages, oldest first.
type RecentMessages interface {
	RecentMessages(principalID string, threadID int64) []string
}

// RecentExchange returns a principal's latest user messages and the assistant's latest chat reply,
// oldest first.
type RecentExchange interface {
	RecentExchange(principalID string, threadID int64) []turnstate.Line
}

// ExposureLevels reports how much untrusted content a principal's turn has seen.
type ExposureLevels interface {
	Exposure(principalID string) turnstate.Level
}

// IntentChecker judges whether a proposed action matches what the user recently asked for.
type IntentChecker struct {
	client   *Client
	optIn    OptIn
	recent   RecentExchange
	exposure ExposureLevels
}

var _ approval.IntentChecker = (*IntentChecker)(nil)

func NewIntentChecker(client *Client, optIn OptIn, recent RecentExchange, exposure ExposureLevels) *IntentChecker {
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
		c.client.recordDecision(ctx, principalID, "intent", "suspicious")
		return true, nil
	}
	exchange := redactExchange(c.recent.RecentExchange(principalID, thread.From(ctx)))
	if exchange.Empty() {
		return false, nil
	}
	action := fmt.Sprintf("%s %s %q", intent.Verb, intent.TargetKind, intent.TargetName)
	if intent.NewName != "" && intent.NewName != intent.TargetName {
		action += fmt.Sprintf(" to %q", intent.NewName)
	}
	action += " using tool " + tool
	answers, err := c.client.Ask(ctx, "intent",
		map[string]Redacted{
			"recent_exchange": exchange,
			"action":          Redact(action),
		},
		map[string]Question{intentQuestionID: Noul(
			"Did the user, in `recent_exchange`, ask for or clearly agree to the action in `action`? Lines starting with `user:` are the user's messages; lines starting with `assistant:` are only the assistant's own words, given as context. The action counts as requested only if the user asked for it or agreed to the assistant's offer of it. A short confirmation such as 'yes' or 'do it' after the assistant offered this action counts as agreeing.",
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
	warn := probability < threshold
	result := "clean"
	if warn {
		result = "warned"
	}
	c.client.recordDecision(ctx, principalID, "intent", result)
	return warn, nil
}

// redactExchange renders an exchange as one `role: text` line per message, oldest first. The reply
// is scrubbed before it is cut to its end, so the cut never splits a path or token the scrubber
// would have matched. Line breaks within a message are flattened so that a message cannot pass
// for a line of another role.
func redactExchange(exchange []turnstate.Line) Redacted {
	lines := make([]string, 0, len(exchange))
	for _, line := range exchange {
		text := Redact(line.Text).String()
		if line.Role == turnstate.RoleAssistant {
			if runes := []rune(text); len(runes) > replyRunes {
				text = string(runes[len(runes)-replyRunes:])
			}
		}
		text = strings.Join(strings.Fields(text), " ")
		if text != "" {
			lines = append(lines, line.Role+": "+text)
		}
	}
	return Redacted{text: strings.Join(lines, "\n")}
}
