package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	TypeModelTurn = "model.turn"

	modelTurnCatchUp         = 6 * time.Hour
	modelTurnMinimumInterval = time.Hour
	maximumPromptRunes       = 1000
)

// TurnRunner is the turn engine as a scheduled model turn sees it: one isolated,
// read-only turn on the principal's own subscription.
type TurnRunner interface {
	RunScheduled(ctx context.Context, principalID, turn, message string, model agentruntime.ModelEffort) (turn.ScheduledReply, error)
}

type modelTurnConfig struct {
	Prompt string `json:"prompt"`
	// Model is the job's own "id:effort"; without it the job runs on its default.
	Model string `json:"model,omitempty"`
}

// ModelTurn is the job type that asks the owner's model a stored question on a schedule.
func (s *Service) ModelTurn(runner TurnRunner) JobType {
	return JobType{
		Name:            TypeModelTurn,
		Handler:         modelTurn{runner: runner, service: s},
		CatchUp:         modelTurnCatchUp,
		MinimumInterval: modelTurnMinimumInterval,
	}
}

// ModelTurnConfig encodes the stored prompt of a model.turn job that runs on its default model.
func ModelTurnConfig(prompt string) (json.RawMessage, error) {
	return ModelTurnConfigWithModel(prompt, "")
}

// ModelTurnConfigWithModel encodes the stored prompt of a model.turn job and its own model,
// "id:effort", or "" for its default.
func ModelTurnConfigWithModel(prompt, model string) (json.RawMessage, error) {
	return json.Marshal(modelTurnConfig{Prompt: strings.TrimSpace(prompt), Model: model})
}

// ModelTurnPrompt reads the stored prompt back for display.
func ModelTurnPrompt(config json.RawMessage) string {
	var decoded modelTurnConfig
	if err := json.Unmarshal(config, &decoded); err != nil {
		return ""
	}
	return decoded.Prompt
}

// ModelTurnModel reads the job's own model back, "" when it runs on its default.
func ModelTurnModel(config json.RawMessage) string {
	var decoded modelTurnConfig
	if err := json.Unmarshal(config, &decoded); err != nil {
		return ""
	}
	return decoded.Model
}

type modelTurn struct {
	runner  TurnRunner
	service *Service
}

func (m modelTurn) Validate(config json.RawMessage) error {
	_, err := decodeModelTurn(config)
	return err
}

func decodeModelTurn(config json.RawMessage) (modelTurnConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	var decoded modelTurnConfig
	if err := decoder.Decode(&decoded); err != nil {
		return modelTurnConfig{}, fmt.Errorf("the job's instructions could not be read")
	}
	prompt := strings.TrimSpace(decoded.Prompt)
	if prompt == "" || !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > maximumPromptRunes {
		return modelTurnConfig{}, fmt.Errorf("the prompt must be between 1 and %d characters", maximumPromptRunes)
	}
	decoded.Prompt = prompt
	if decoded.Model != "" {
		model, err := agentruntime.ParseModelEffort(decoded.Model)
		if err != nil || !modelcatalog.ValidChoice(model) || model.String() != decoded.Model {
			return modelTurnConfig{}, fmt.Errorf("the job's model must be written as id:effort")
		}
	}
	return decoded, nil
}

func (m modelTurn) Run(ctx context.Context, occurrence Occurrence) (Result, error) {
	config, err := decodeModelTurn(occurrence.Config)
	if err != nil {
		return Result{Text: FailedText, ErrorKind: "config"}, nil
	}
	task := config.Prompt
	if occurrence.JobID == HeadsUpJobID(occurrence.PrincipalID) {
		ctx = tools.WithCallBudget(ctx, headsUpCallBudget)
		task = HeadsUpTask(occurrence.JobID, config.Prompt)
	}
	model := m.service.runModel(ctx, occurrence, config.Model)
	reply, err := m.runner.RunScheduled(ctx, occurrence.PrincipalID, outbox.ScheduledTurn(occurrence.RunID), prompt.ScheduledJob(occurrence.JobName, occurrence.ScheduledFor, occurrence.Location, task), model)
	if err != nil {
		return Result{}, err
	}
	result := Result{Text: reply.Text, ErrorKind: reply.ErrorKind, Private: reply.Private, External: reply.External}
	if reply.ErrorKind == "" && strings.Contains(reply.Text, prompt.NothingToAdd) {
		result.Text = strings.TrimSpace(strings.ReplaceAll(reply.Text, prompt.NothingToAdd, ""))
		result.Quiet = result.Text == ""
	}
	return result, nil
}

// Where the model a job runs on comes from, as schedule_get names it.
const (
	modelSourceOwn     = "its own choice"
	modelSourceHeadsUp = "Heads-up default"
	modelSourceChat    = "chat default"
)

// jobModel is the model a model.turn job asks for and where that choice comes from: its own,
// the Heads-up default for the Heads-up job, otherwise the chat default. The chat default may be
// the empty pair, which leaves the choice to Pi.
func (s *Service) jobModel(principalID, jobID, own string) (agentruntime.ModelEffort, string) {
	if own != "" {
		if model, err := agentruntime.ParseModelEffort(own); err == nil {
			return model, modelSourceOwn
		}
	}
	if jobID == HeadsUpJobID(principalID) && s.models.headsUp.Model != "" {
		return s.models.headsUp, modelSourceHeadsUp
	}
	return s.models.chat, modelSourceChat
}

// effectiveModel is the model a job's run uses: the one it asks for while the principal's
// subscription offers it, otherwise the chat default, with the reason it fell back.
func (s *Service) effectiveModel(ctx context.Context, principalID string, want agentruntime.ModelEffort) (agentruntime.ModelEffort, string) {
	model, fallback, err := modelcatalog.Effective(ctx, s.models.catalog, s.models.usage, principalID, want, s.models.chat)
	if err != nil {
		s.logger.WarnContext(ctx, "schedule.model_catalog.failed", "principal_id", principalID, "error", err)
	}
	return model, fallback
}

// runModel is the model a run uses; a job on the chat default leaves it to Pi.
func (s *Service) runModel(ctx context.Context, occurrence Occurrence, own string) agentruntime.ModelEffort {
	want, source := s.jobModel(occurrence.PrincipalID, occurrence.JobID, own)
	if source == modelSourceChat {
		return agentruntime.ModelEffort{}
	}
	model, fallback := s.effectiveModel(ctx, occurrence.PrincipalID, want)
	if fallback != "" {
		s.logger.InfoContext(ctx, "schedule.model.fallback", "principal_id", occurrence.PrincipalID, "job_id", occurrence.JobID, "model", want.String(), "reason", fallback)
	}
	return model
}
