package telegram

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/task"
)

const (
	// statusUsageTimeout backstops the reader's own bound, since updates are handled one at a time.
	statusUsageTimeout = 3 * time.Second

	statusUsageTimeFormat      = "Mon 2 Jan 15:04"
	statusNoTurnLine           = "Last turn: none yet since the restart."
	statusNotConnectedLine     = "Subscription usage: not connected."
	statusUsageUnavailableLine = "Subscription usage: unavailable right now."
	statusStaleChoiceLine      = "Your earlier choice is no longer available, so this conversation runs on the default."
	statusTaskCatalogLine      = "The task model is not offered on your subscription, so new tasks run on the chat model."
	statusTaskUsageLine        = "The task model is not available on your plan right now, so new tasks run on the chat model."

	modelUsagePrivacyLine = "/status reads your subscription usage from ChatGPT with your own connection."
	modelCommandHelp      = "Send /model to see the model and effort in use and every model that can be chosen with its effort levels, and to change them with buttons or by typing /model <model> [effort], /model <effort>, or /model default. A choice holds in that conversation until /new. Background tasks run on the model the operator configured, not on this choice. /status shows the model in use, the last turn, and the subscription usage."
)

var planNamePattern = regexp.MustCompile(`^[a-z0-9_ -]{1,24}$`)

// statusReply is the fixed status line plus, when /model is on, the model in use here, the task
// model, the last turn, and the principal's own subscription usage.
func (s *Service) statusReply(ctx context.Context, principalID string) (string, error) {
	m := s.model
	if m == nil {
		return ConversationStatusReply, nil
	}
	lane := laneOf(ctx, principalID)
	effective, stale, err := m.Lanes.EffectiveChoice(ctx, lane)
	if err != nil {
		return "", err
	}
	_, own, err := m.Lanes.Choice(ctx, lane)
	if err != nil {
		return "", err
	}
	source := "the default"
	if own && !stale {
		source = "your choice, until /new"
	}
	lines := []string{ConversationStatusReply, "", fmt.Sprintf("Model here: %s, effort %s (%s).", effective.Model, effective.Effort, source)}
	if stale {
		lines = append(lines, statusStaleChoiceLine)
	}
	lines = append(lines, m.statusTaskLines(ctx, principalID)...)
	if m.Turns != nil {
		lines = append(lines, statusTurnLine(m.Turns.LaneTurn(lane)))
	}
	if m.Usage != nil {
		lines = append(lines, m.statusUsageLines(ctx, principalID)...)
	}
	return strings.Join(lines, "\n"), nil
}

func (m *ModelCommand) statusTaskLines(ctx context.Context, principalID string) []string {
	if m.Tasks == nil {
		return nil
	}
	pair, fallback := m.Tasks.EffectiveModel(ctx, principalID)
	if pair.Model == "" {
		return nil
	}
	lines := []string{fmt.Sprintf("Background tasks: %s, effort %s.", pair.Model, pair.Effort)}
	switch fallback {
	case task.FallbackCatalog:
		lines = append(lines, statusTaskCatalogLine)
	case task.FallbackUsage:
		lines = append(lines, statusTaskUsageLine)
	}
	return lines
}

func statusTurnLine(turn conversation.LaneTurn) string {
	if !turn.Ran {
		return statusNoTurnLine
	}
	answered := "a model that was not reported"
	if turn.Model != "" {
		answered = turn.Model
	}
	line := fmt.Sprintf("Last turn: answered by %s, %d input and %d output tokens", answered, turn.Tokens.Input, turn.Tokens.Output)
	if cached := turn.Tokens.CacheRead; cached > 0 {
		line += fmt.Sprintf(" (%d read from cache)", cached)
	}
	line += "."
	if turn.Stale() {
		line += fmt.Sprintf(" It ran on the default because %s is no longer available.", turn.PassedOver.Model)
	}
	return line
}

func (m *ModelCommand) statusUsageLines(ctx context.Context, principalID string) []string {
	ctx, cancel := context.WithTimeout(ctx, statusUsageTimeout)
	defer cancel()
	usage, err := m.Usage.Usage(ctx, principalID)
	switch {
	case errors.Is(err, connection.ErrNotConnected):
		return []string{statusNotConnectedLine}
	case err != nil:
		return []string{statusUsageUnavailableLine}
	}
	headline := "Subscription usage:"
	if planNamePattern.MatchString(usage.PlanType) {
		headline = fmt.Sprintf("Subscription usage (%s plan):", usage.PlanType)
	}
	lines := []string{headline}
	location := m.Location
	if location == nil {
		location = time.UTC
	}
	for _, window := range []struct {
		name  string
		usage *chatgpt.UsageWindow
	}{{"5-hour", usage.Primary}, {"Weekly", usage.Secondary}} {
		if window.usage == nil {
			continue
		}
		line := fmt.Sprintf("%s: %.0f%% used", window.name, window.usage.UsedPercent)
		if !window.usage.ResetAt.IsZero() {
			line += ", resets " + window.usage.ResetAt.In(location).Format(statusUsageTimeFormat)
		}
		lines = append(lines, line)
	}
	if usage.LimitReached {
		lines = append(lines, "Limit reached: yes.")
	} else {
		lines = append(lines, "Limit reached: no.")
	}
	return lines
}
