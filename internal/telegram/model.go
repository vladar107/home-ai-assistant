package telegram

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	ModelTitle = "Model and effort"
	// ModelCommandDescription is the slash menu entry.
	ModelCommandDescription = "Show and change the model and effort"

	modelPrefix        = "mdl"
	modelPickAction    = "m"
	modelApplyAction   = "e"
	modelDefaultAction = "d"
	modelListAction    = "l"

	// modelUsageMaxAge bounds how old a cached usage read may be to mark a model unavailable.
	modelUsageMaxAge = 15 * time.Minute
)

// ModelLanes is the conversation's per-lane model choice, implemented by conversation.Service.
type ModelLanes interface {
	Choice(ctx context.Context, lane conversation.Lane) (agentruntime.ModelEffort, bool, error)
	SetChoiceFrom(ctx context.Context, lane conversation.Lane, choice agentruntime.ModelEffort, models []modelcatalog.Model) error
	ClearChoice(ctx context.Context, lane conversation.Lane) error
	EffectiveChoice(ctx context.Context, lane conversation.Lane) (choice agentruntime.ModelEffort, stale bool, err error)
	EffectiveChoiceFrom(ctx context.Context, lane conversation.Lane, models []modelcatalog.Model) (choice agentruntime.ModelEffort, stale bool, err error)
}

// ModelCommand is what /model needs; a nil one leaves the command out.
type ModelCommand struct {
	Lanes   ModelLanes
	Catalog modelcatalog.Reader
	// Usage is optional; without it no model is marked unavailable.
	Usage chatgpt.UsageReader
	// ChatDefault and TaskDefault are the configured defaults, shown and offered when the
	// principal's catalog cannot be read.
	ChatDefault agentruntime.ModelEffort
	TaskDefault agentruntime.ModelEffort
}

// modelCatalog is the models to offer for one request.
type modelCatalog struct {
	models []modelcatalog.Model
	// listed is nil when the catalog could not be read, which SetChoiceFrom takes as such.
	listed []modelcatalog.Model
}

func (m *ModelCommand) catalog(ctx context.Context, principalID string) (modelCatalog, error) {
	var models []modelcatalog.Model
	if m.Catalog != nil {
		var err error
		models, err = m.Catalog.Models(ctx, principalID)
		if err != nil && !errors.Is(err, modelcatalog.ErrUnavailable) {
			return modelCatalog{}, err
		}
	}
	if len(models) == 0 {
		return modelCatalog{models: modelcatalog.Fallback(m.ChatDefault, m.TaskDefault)}, nil
	}
	return modelCatalog{models: models, listed: models}, nil
}

func (m *ModelCommand) unavailable(principalID, modelID string) bool {
	if m.Usage == nil {
		return false
	}
	usage, ok := m.Usage.CachedUsage(principalID, modelUsageMaxAge)
	if !ok {
		return false
	}
	available, known := usage.ModelAvailable[modelID]
	return known && !available
}

// renderModel shows the pair the lane's next turn runs on, the task default, and every model the
// principal can choose, with the buttons to change them.
func (s *Service) renderModel(ctx context.Context, principalID, note string, catalog modelCatalog) (string, InlineKeyboard, error) {
	m := s.model
	lane := laneOf(ctx, principalID)
	effective, stale, err := m.Lanes.EffectiveChoiceFrom(ctx, lane, catalog.listed)
	if err != nil {
		return "", nil, err
	}
	_, own, err := m.Lanes.Choice(ctx, lane)
	if err != nil {
		return "", nil, err
	}
	lines := []string{ModelTitle, ""}
	if note != "" {
		lines = append(lines, note, "")
	}
	source := "the default"
	if own && !stale {
		source = "your choice, until /new"
	}
	lines = append(lines, fmt.Sprintf("Here: %s, effort %s (%s).", effective.Model, effective.Effort, source))
	if stale {
		lines = append(lines, "Your earlier choice is no longer available, so this conversation runs on the default.")
	}
	lines = append(lines, fmt.Sprintf("Background tasks: %s, effort %s. This is set by the operator, not by /model.", m.TaskDefault.Model, m.TaskDefault.Effort), "")
	if catalog.listed == nil {
		lines = append(lines, "The model list is unavailable right now, so only the configured defaults are offered.", "")
	}
	descriptions := modelcatalog.Describe(catalog.models)
	for index, model := range catalog.models {
		line := fmt.Sprintf("%s: %s. Effort: %s", model.ID, descriptions[index], strings.Join(model.Efforts, ", "))
		if model.ID == effective.Model {
			line += " (current)"
		}
		if m.unavailable(principalID, model.ID) {
			line += " (not available on your plan right now)"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "Change it with the buttons, or type /model <model> [effort], /model <effort>, or /model default. A choice holds in this conversation until /new.")
	return strings.Join(lines, "\n"), modelListKeyboard(catalog.models), nil
}

func modelListKeyboard(models []modelcatalog.Model) InlineKeyboard {
	var keyboard InlineKeyboard
	for _, model := range models {
		if data := modelCallback(modelPickAction, model.ID); len(data) <= maximumCallbackDataBytes && !strings.Contains(model.ID, ":") {
			keyboard = append(keyboard, []InlineButton{{Text: model.ID, CallbackData: data}})
		}
	}
	return append(keyboard, []InlineButton{{Text: "Back to defaults", CallbackData: modelPrefix + ":" + modelDefaultAction}})
}

func modelLevelsKeyboard(model modelcatalog.Model) InlineKeyboard {
	var keyboard InlineKeyboard
	var row []InlineButton
	for _, effort := range model.Efforts {
		data := modelCallback(modelApplyAction, model.ID+":"+effort)
		if len(data) > maximumCallbackDataBytes || strings.Contains(model.ID, ":") {
			continue
		}
		if len(row) == 3 {
			keyboard, row = append(keyboard, row), nil
		}
		row = append(row, InlineButton{Text: effort, CallbackData: data})
	}
	if len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	return append(keyboard, []InlineButton{
		{Text: "Other models", CallbackData: modelPrefix + ":" + modelListAction},
		{Text: "Back to defaults", CallbackData: modelPrefix + ":" + modelDefaultAction},
	})
}

func modelCallback(action, value string) string {
	return modelPrefix + ":" + action + ":" + value
}

type modelPress struct {
	action string
	// model is set for the pick and apply actions, effort for apply.
	model, effort string
}

// parseModelCallback accepts only the fixed vocabulary: mdl:m:<model>, mdl:e:<model>:<effort>,
// mdl:d, and mdl:l.
func parseModelCallback(data string) (modelPress, bool) {
	if len(data) > maximumCallbackDataBytes {
		return modelPress{}, false
	}
	parts := strings.Split(data, ":")
	if len(parts) < 2 || parts[0] != modelPrefix {
		return modelPress{}, false
	}
	press := modelPress{action: parts[1]}
	switch {
	case (press.action == modelDefaultAction || press.action == modelListAction) && len(parts) == 2:
		return press, true
	case press.action == modelPickAction && len(parts) == 3 && parts[2] != "":
		press.model = parts[2]
		return press, true
	case press.action == modelApplyAction && len(parts) == 4 && parts[2] != "" && agentruntime.ValidEffort(parts[3]):
		press.model, press.effort = parts[2], parts[3]
		return press, true
	}
	return modelPress{}, false
}

// processModelCommand answers /model and its typed forms.
func (s *Service) processModelCommand(ctx context.Context, message *Message, principalID, command string) (processingResult, string, error) {
	args := strings.Fields(strings.ToLower(message.Text))[1:]
	reply, keyboard, err := s.modelReply(ctx, principalID, args)
	if err != nil {
		return processingResult{}, "handle_command", err
	}
	return s.sendCommandReply(ctx, message, principalID, command, reply, keyboard)
}

func (s *Service) modelReply(ctx context.Context, principalID string, args []string) (string, InlineKeyboard, error) {
	catalog, err := s.model.catalog(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(args) == 0 {
		return s.renderModel(ctx, principalID, "", catalog)
	}
	lane := laneOf(ctx, principalID)
	if len(args) == 1 && args[0] == "default" {
		if err := s.model.Lanes.ClearChoice(ctx, lane); err != nil {
			return "", nil, err
		}
		chat := s.model.ChatDefault
		return fmt.Sprintf("This conversation is back on the default: %s, effort %s.", chat.Model, chat.Effort), nil, nil
	}
	if len(args) > 2 {
		return modelUsage(catalog.models), nil, nil
	}
	current, _, err := s.model.Lanes.EffectiveChoiceFrom(ctx, lane, catalog.listed)
	if err != nil {
		return "", nil, err
	}
	var model modelcatalog.Model
	var effort string
	if len(args) == 1 && agentruntime.ValidEffort(args[0]) {
		index := slices.IndexFunc(catalog.models, func(m modelcatalog.Model) bool { return m.ID == current.Model })
		if index < 0 {
			return fmt.Sprintf("%s is not in the model list, so I cannot change its effort. %s", current.Model, modelUsage(catalog.models)), nil, nil
		}
		model, effort = catalog.models[index], args[0]
	} else {
		resolved, err := modelcatalog.Resolve(catalog.models, args[0])
		var refusal *modelcatalog.ResolveError
		if errors.As(err, &refusal) {
			if refusal.Ambiguous {
				return fmt.Sprintf("%q matches several models: %s. Use the full name.", args[0], strings.Join(refusal.Options, ", ")), nil, nil
			}
			return fmt.Sprintf("I do not know the model %q. Available models: %s.", args[0], strings.Join(refusal.Options, ", ")), nil, nil
		}
		model = resolved
		if len(model.Efforts) == 0 {
			return fmt.Sprintf("%s offers no effort level I can use.", model.ID), nil, nil
		}
		if len(args) == 2 {
			effort = args[1]
		} else {
			effort = defaultEffortFor(model, current.Effort)
		}
	}
	if !agentruntime.ValidEffort(effort) {
		return fmt.Sprintf("%q is not an effort level. %s supports: %s.", effort, model.ID, strings.Join(model.Efforts, ", ")), nil, nil
	}
	if !model.Supports(effort) {
		return fmt.Sprintf("%s does not support effort %s. It supports: %s.", model.ID, effort, strings.Join(model.Efforts, ", ")), nil, nil
	}
	if err := s.model.Lanes.SetChoiceFrom(ctx, lane, agentruntime.ModelEffort{Model: model.ID, Effort: effort}, catalog.listed); err != nil {
		if errors.Is(err, conversation.ErrUnsupportedChoice) {
			return fmt.Sprintf("%s on effort %s is not available. %s", model.ID, effort, modelUsage(catalog.models)), nil, nil
		}
		return "", nil, err
	}
	applied, _, err := s.model.Lanes.EffectiveChoiceFrom(ctx, lane, catalog.listed)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("This conversation now runs on %s, effort %s, until /new.", applied.Model, applied.Effort), nil, nil
}

// defaultEffortFor keeps the effort in use when the model supports it, then medium, then the
// model's lowest level.
func defaultEffortFor(model modelcatalog.Model, current string) string {
	for _, effort := range []string{current, agentruntime.DefaultEffort} {
		if canonical, ok := model.Canonical(effort); ok {
			return canonical
		}
	}
	if len(model.Efforts) == 0 {
		return ""
	}
	return model.Efforts[0]
}

func modelUsage(models []modelcatalog.Model) string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return "Use /model <model> [effort], /model <effort>, or /model default. Models: " + strings.Join(ids, ", ") + ". Effort levels: " + strings.Join(agentruntime.Efforts, ", ") + "."
}

// processModelPress acts on a button of the /model message and refreshes it from current state. A
// press always acts on the lane of the tab the message is in, so it can only change the
// principal's own choice.
func (s *Service) processModelPress(ctx context.Context, callback *CallbackQuery, principalID string, press modelPress) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "model." + press.action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	catalog, err := s.model.catalog(ctx, principalID)
	if err != nil {
		return processingResult{}, "model_catalog", err
	}
	lane := laneOf(ctx, principalID)
	var text, note string
	var keyboard InlineKeyboard
	switch press.action {
	case modelDefaultAction:
		if err = s.model.Lanes.ClearChoice(ctx, lane); err != nil {
			return processingResult{}, "model_action", err
		}
		note = "Back on the default."
	case modelPickAction:
		index := slices.IndexFunc(catalog.models, func(m modelcatalog.Model) bool { return m.ID == press.model })
		if index >= 0 {
			text = fmt.Sprintf("%s\n\nChoose the effort for %s.", ModelTitle, press.model)
			keyboard = modelLevelsKeyboard(catalog.models[index])
		} else {
			note = fmt.Sprintf("%s is no longer available.", press.model)
		}
	case modelApplyAction:
		err = s.model.Lanes.SetChoiceFrom(ctx, lane, agentruntime.ModelEffort{Model: press.model, Effort: press.effort}, catalog.listed)
		switch {
		case errors.Is(err, conversation.ErrUnsupportedChoice):
			note = fmt.Sprintf("%s on effort %s is not available.", press.model, press.effort)
		case err != nil:
			return processingResult{}, "model_action", err
		default:
			note = "Saved. This conversation holds the choice until /new."
		}
	}
	if text == "" {
		if text, keyboard, err = s.renderModel(ctx, principalID, note, catalog); err != nil {
			return processingResult{}, "render_model", err
		}
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}
