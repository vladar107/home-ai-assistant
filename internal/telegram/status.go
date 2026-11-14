package telegram

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/task"
)

const (
	// statusUsageTimeout backstops the reader's own bound, since updates are handled one at a time.
	statusUsageTimeout = 3 * time.Second
)

var (
	modelUsagePrivacyLine = locale.T("/status reads your subscription usage from ChatGPT with your own connection.", "/status читает использование твоей подписки в ChatGPT через твоё собственное подключение.")
	modelCommandHelp      = locale.T("Send /model to see the model and effort in use and every model that can be chosen with its effort levels, and to change them with buttons or by typing /model <model> [effort], /model <effort>, or /model default. A choice holds in that conversation until /new. Background tasks run on the model the operator configured, not on this choice. /status shows the model in use, the last turn, and the subscription usage.",
		"Отправь /model, чтобы увидеть используемую модель и глубину рассуждений и все модели, которые можно выбрать, с их уровнями глубины, и чтобы сменить их кнопками или командой /model <модель> [глубина], /model <глубина> или /model default. Выбор действует в этом разговоре до /new. Фоновые задачи работают на модели, настроенной оператором, а не на твоём выборе. /status показывает используемую модель, последний ответ и использование подписки.")

	statusNoTurnLine           = locale.T("Last turn: none yet since the restart.", "Последний ответ: после перезапуска ответов ещё не было.")
	statusNotConnectedLine     = locale.T("Subscription usage: not connected.", "Использование подписки: подписка не подключена.")
	statusUsageUnavailableLine = locale.T("Subscription usage: unavailable right now.", "Использование подписки: сейчас недоступно.")
	statusStaleChoiceLine      = locale.T("Your earlier choice is no longer available, so this conversation runs on the default.", "Твой прежний выбор больше недоступен, поэтому этот разговор идёт на модели по умолчанию.")
	statusTaskCatalogLine      = locale.T("The task model is not offered on your subscription, so new tasks run on the chat model.", "Модель для задач не предоставляется по твоей подписке, поэтому новые задачи выполняются на модели чата.")
	statusTaskUsageLine        = locale.T("The task model is not available on your plan right now, so new tasks run on the chat model.", "Модель для задач сейчас недоступна на твоём тарифе, поэтому новые задачи выполняются на модели чата.")

	statusModelDefaultLine = locale.T("Model here: %s, effort %s (the default).", "Модель здесь: %s, глубина рассуждений %s (по умолчанию).")
	statusModelChoiceLine  = locale.T("Model here: %s, effort %s (your choice, until /new).", "Модель здесь: %s, глубина рассуждений %s (твой выбор, до /new).")
	statusTaskModelLine    = locale.T("Background tasks: %s, effort %s.", "Фоновые задачи: %s, глубина рассуждений %s.")

	statusTurnLineText       = locale.T("Last turn: answered by %s, %d input and %d output tokens.", "Последний ответ: модель — %s, токенов на входе — %d, на выходе — %d.")
	statusTurnCachedLineText = locale.T("Last turn: answered by %s, %d input and %d output tokens (%d read from cache).", "Последний ответ: модель — %s, токенов на входе — %d, на выходе — %d (из кэша прочитано %d).")
	statusTurnStaleLine      = locale.T("It ran on the default because %s is no longer available.", "Ответ дала модель по умолчанию, потому что %s больше недоступна.")
	statusModelNotReported   = locale.T("a model that was not reported", "не указана")

	statusUsageHeadline     = locale.T("Subscription usage:", "Использование подписки:")
	statusUsagePlanHeadline = locale.T("Subscription usage (%s plan):", "Использование подписки (тариф %s):")
	statusWindowFiveHour    = locale.T("5-hour", "За 5 часов")
	statusWindowWeekly      = locale.T("Weekly", "За неделю")
	statusWindowUsed        = locale.T("%s: %.0f%% used", "%s: использовано %.0f%%")
	statusWindowResets      = locale.T("%s: %.0f%% used, resets %s", "%s: использовано %.0f%%, сброс %s")
	statusLimitReached      = locale.T("Limit reached: yes.", "Лимит исчерпан: да.")
	statusLimitNotReached   = locale.T("Limit reached: no.", "Лимит исчерпан: нет.")
)

var planNamePattern = regexp.MustCompile(`^[a-z0-9_ -]{1,24}$`)

// statusReply is the fixed status line plus, when /model is on, the model in use here, the task
// model, the last turn, and the principal's own subscription usage.
func (s *Service) statusReply(ctx context.Context, principalID string) (string, error) {
	m := s.model
	if m == nil {
		return ConversationStatusReply.Format(ctx), nil
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
	modelLine := statusModelDefaultLine
	if own && !stale {
		modelLine = statusModelChoiceLine
	}
	lines := []string{ConversationStatusReply.Format(ctx), "", modelLine.Format(ctx, effective.Model, effective.Effort)}
	if stale {
		lines = append(lines, statusStaleChoiceLine.Format(ctx))
	}
	lines = append(lines, m.statusTaskLines(ctx, principalID)...)
	if m.Turns != nil {
		lines = append(lines, statusTurnLine(ctx, m.Turns.LaneTurn(lane)))
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
	lines := []string{statusTaskModelLine.Format(ctx, pair.Model, pair.Effort)}
	switch fallback {
	case task.FallbackCatalog:
		lines = append(lines, statusTaskCatalogLine.Format(ctx))
	case task.FallbackUsage:
		lines = append(lines, statusTaskUsageLine.Format(ctx))
	}
	return lines
}

func statusTurnLine(ctx context.Context, turn conversation.LaneTurn) string {
	if !turn.Ran {
		return statusNoTurnLine.Format(ctx)
	}
	answered := statusModelNotReported.Format(ctx)
	if turn.Model != "" {
		answered = turn.Model
	}
	line := statusTurnLineText.Format(ctx, answered, turn.Tokens.Input, turn.Tokens.Output)
	if cached := turn.Tokens.CacheRead; cached > 0 {
		line = statusTurnCachedLineText.Format(ctx, answered, turn.Tokens.Input, turn.Tokens.Output, cached)
	}
	if turn.Stale() {
		line += " " + statusTurnStaleLine.Format(ctx, turn.PassedOver.Model)
	}
	return line
}

func (m *ModelCommand) statusUsageLines(ctx context.Context, principalID string) []string {
	ctx, cancel := context.WithTimeout(ctx, statusUsageTimeout)
	defer cancel()
	usage, err := m.Usage.Usage(ctx, principalID)
	switch {
	case errors.Is(err, connection.ErrNotConnected):
		return []string{statusNotConnectedLine.Format(ctx)}
	case err != nil:
		return []string{statusUsageUnavailableLine.Format(ctx)}
	}
	headline := statusUsageHeadline.Format(ctx)
	if planNamePattern.MatchString(usage.PlanType) {
		headline = statusUsagePlanHeadline.Format(ctx, usage.PlanType)
	}
	lines := []string{headline}
	location := m.Location
	if location == nil {
		location = time.UTC
	}
	for _, window := range []struct {
		name  locale.Text
		usage *chatgpt.UsageWindow
	}{{statusWindowFiveHour, usage.Primary}, {statusWindowWeekly, usage.Secondary}} {
		if window.usage == nil {
			continue
		}
		name := window.name.Format(ctx)
		line := statusWindowUsed.Format(ctx, name, window.usage.UsedPercent)
		if !window.usage.ResetAt.IsZero() {
			line = statusWindowResets.Format(ctx, name, window.usage.UsedPercent, locale.Date(locale.From(ctx), window.usage.ResetAt.In(location), locale.DayTime))
		}
		lines = append(lines, line)
	}
	if usage.LimitReached {
		lines = append(lines, statusLimitReached.Format(ctx))
	} else {
		lines = append(lines, statusLimitNotReached.Format(ctx))
	}
	return lines
}
