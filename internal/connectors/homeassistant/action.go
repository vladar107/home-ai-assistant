package homeassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const maximumIntentRunes = 80

var (
	actionSummary    = locale.T("Home Assistant action %s: %s\n%s\nI read outside content in this conversation, so I need your confirmation. /new starts a fresh conversation where home actions run at once.", "Действие Home Assistant %s: %s\n%s\nВ этом разговоре был прочитан внешний контент, поэтому нужно твоё подтверждение. /new начинает новый разговор, где действия по дому выполняются сразу.")
	argumentsNone    = locale.T("Arguments: none", "Аргументы: нет")
	argumentsHeading = locale.T("Arguments:", "Аргументы:")

	actionUnreadable = locale.T("The stored action could not be read.", "Не удалось прочитать сохранённое действие.")
	actionWithheld   = locale.T("Home actions are withheld until the household fixes which devices are exposed; check /settings.", "Действия по дому приостановлены, пока не будет исправлен список доступных устройств; загляни в /settings.")
	actionGone       = locale.T("That home action is no longer available.", "Это действие по дому больше недоступно.")
	actionRejected   = locale.T("Home Assistant could not do that: %s", "Home Assistant не смог это сделать: %s")
	actionDone       = locale.T("Done.", "Готово.")
)

func actionFailed(ctx context.Context, reason string, message locale.Text, arguments ...any) *approval.ActionError {
	return &approval.ActionError{Reason: reason, Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

type actionArguments struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// proposeAction records an immediate action for confirmation because the conversation has read
// outside content; nothing reaches the device.
func (s *Service) proposeAction(ctx context.Context, principalID, name string, tool mappedTool, arguments map[string]any) tools.Result {
	if s.proposer == nil {
		return tools.Result{Text: "This conversation has read outside content, so the action needs the user's confirmation, which is unavailable. Do not retry; tell the user the action was not taken.", IsError: true}
	}
	encoded, err := json.Marshal(actionArguments{Tool: name, Arguments: arguments})
	if err != nil {
		return failure(errMalformed)
	}
	rendered, err := renderArguments(ctx, arguments)
	// The stored arguments are capped too, so whitespace that the rendering collapses cannot hide a
	// long value behind a short summary.
	if err != nil || len([]rune(rendered)) > maximumActionArgumentRunes || len(encoded) > maximumDataBytes {
		return tools.Result{Text: fmt.Sprintf("The arguments are too long to show for confirmation; send them in at most %d characters.", maximumActionArgumentRunes), IsError: true}
	}
	description := strings.Join(strings.Fields(tool.definition.Description), " ")
	summary := actionSummary.Format(ctx, name, description, rendered)
	target := description
	if device, ok := arguments["name"].(string); ok && strings.TrimSpace(device) != "" {
		target = description + " " + device
	}
	intent := approval.Intent{Verb: "run", TargetKind: "home action", TargetName: clip.Runes(target, maximumIntentRunes)}
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolAction, Arguments: encoded, Summary: summary, Intent: intent}); err != nil {
		if approval.Refused(err) {
			return failure(err)
		}
		s.logger.WarnContext(ctx, "homeassistant.proposal.failed", "principal_id", principalID, "error", err)
		return failure(fmt.Errorf("the proposal could not be recorded"))
	}
	return tools.Result{Text: approval.ProposedText}
}

// executeApprovedAction re-checks a fresh snapshot, that actions are still allowed, and
// that the tool is still a write tool before running it with the stored arguments.
func (s *Service) executeApprovedAction(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var stored actionArguments
	if err := json.Unmarshal(record.Arguments, &stored); err != nil || stored.Tool == "" {
		return approval.Outcome{}, actionFailed(ctx, "malformed", actionUnreadable)
	}
	s.forget()
	snap, err := s.current(ctx)
	if err != nil {
		return approval.Outcome{}, actionError(ctx, err)
	}
	if !snap.actionsAllowed() {
		return approval.Outcome{}, actionFailed(ctx, "withheld", actionWithheld)
	}
	tool, found := snap.byName[stored.Tool]
	if !found || tool.definition.Risk != tools.RiskWrite {
		return approval.Outcome{}, actionFailed(ctx, "unavailable", actionGone)
	}
	if stored.Arguments == nil {
		stored.Arguments = map[string]any{}
	}
	text, isError, err := s.callTool(ctx, tool.remote, stored.Arguments)
	s.logger.InfoContext(ctx, "homeassistant.action", "principal_id", record.PrincipalID, "tool", stored.Tool, "is_error", isError || err != nil)
	s.forget()
	if err != nil {
		return approval.Outcome{}, actionError(ctx, err)
	}
	if isError {
		return approval.Outcome{}, actionFailed(ctx, "rejected", actionRejected, text)
	}
	if text == "" {
		return approval.Outcome{Text: actionDone.FormatIn(locale.English), UserText: actionDone.Format(ctx)}, nil
	}
	return approval.Outcome{Text: text}, nil
}

// renderArguments shows the arguments as the user will confirm them, one field per line in key
// order; the reply renderer escapes markup, so nothing is HTML-escaped here. Whitespace in keys and
// strings collapses so model-written text cannot add lines to the summary.
func renderArguments(ctx context.Context, arguments map[string]any) (string, error) {
	if len(arguments) == 0 {
		return argumentsNone.Format(ctx), nil
	}
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys)+1)
	lines = append(lines, argumentsHeading.Format(ctx))
	for _, key := range keys {
		value, err := renderValue(arguments[key])
		if err != nil {
			return "", err
		}
		lines = append(lines, strings.Join(strings.Fields(key), " ")+": "+value)
	}
	return strings.Join(lines, "\n"), nil
}

// renderValue shows a scalar plainly, a list of scalars comma-separated, quoting a string that
// holds a comma, and anything else as compact JSON.
func renderValue(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return strings.Join(strings.Fields(typed), " "), nil
	case json.Number:
		return typed.String(), nil
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case []any:
		parts := make([]string, 0, len(typed))
		for _, element := range typed {
			switch typedElement := element.(type) {
			case string:
				part, _ := renderValue(typedElement)
				if strings.Contains(part, ",") {
					part = strconv.Quote(part)
				}
				parts = append(parts, part)
			case json.Number, bool, float64:
				part, _ := renderValue(typedElement)
				parts = append(parts, part)
			default:
				return compactJSON(value)
			}
		}
		return strings.Join(parts, ", "), nil
	}
	return compactJSON(value)
}

func compactJSON(value any) (string, error) {
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSpace(rendered.String()), nil
}
