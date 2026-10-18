package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	TypeModelTurn = "model.turn"

	modelTurnCatchUp         = 6 * time.Hour
	modelTurnMinimumInterval = time.Hour
	maximumPromptRunes       = 1000
)

// TurnRunner is the conversation service as a scheduled model turn sees it: one isolated,
// read-only turn on the principal's own subscription.
type TurnRunner interface {
	RunScheduled(ctx context.Context, principalID, turn, message string) (conversation.ScheduledReply, error)
}

type modelTurnConfig struct {
	Prompt string `json:"prompt"`
}

// ModelTurn is the job type that asks the owner's model a stored question on a schedule.
func ModelTurn(runner TurnRunner) JobType {
	return JobType{
		Name:            TypeModelTurn,
		Handler:         modelTurn{runner: runner},
		CatchUp:         modelTurnCatchUp,
		MinimumInterval: modelTurnMinimumInterval,
	}
}

// ModelTurnConfig encodes the stored prompt of a model.turn job.
func ModelTurnConfig(prompt string) (json.RawMessage, error) {
	return json.Marshal(modelTurnConfig{Prompt: strings.TrimSpace(prompt)})
}

// ModelTurnPrompt reads the stored prompt back for display.
func ModelTurnPrompt(config json.RawMessage) string {
	var decoded modelTurnConfig
	if err := json.Unmarshal(config, &decoded); err != nil {
		return ""
	}
	return decoded.Prompt
}

type modelTurn struct {
	runner TurnRunner
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
	return decoded, nil
}

func (m modelTurn) Run(ctx context.Context, occurrence Occurrence) (Result, error) {
	config, err := decodeModelTurn(occurrence.Config)
	if err != nil {
		return Result{Text: FailedText, ErrorKind: "config"}, nil
	}
	if occurrence.JobID == HeadsUpJobID(occurrence.PrincipalID) {
		ctx = tools.WithCallBudget(ctx, headsUpCallBudget)
	}
	reply, err := m.runner.RunScheduled(ctx, occurrence.PrincipalID, outbox.ScheduledTurn(occurrence.RunID), prompt.ScheduledJob(occurrence.JobName, occurrence.ScheduledFor, occurrence.Location, config.Prompt))
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
