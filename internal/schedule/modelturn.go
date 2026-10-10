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
	reply, err := m.runner.RunScheduled(ctx, occurrence.PrincipalID, outbox.ScheduledTurn(occurrence.RunID), scheduledMessage(occurrence, config.Prompt))
	if err != nil {
		return Result{}, err
	}
	return Result{Text: reply.Text, ErrorKind: reply.ErrorKind, Private: reply.Private}, nil
}

// scheduledMessage tells the model that nobody is waiting on this turn, so it does the task and
// reports instead of asking questions or offering changes it cannot make.
func scheduledMessage(occurrence Occurrence, prompt string) string {
	at := occurrence.ScheduledFor.In(occurrence.Location).Format("Monday 2 January 2006, 15:04 MST")
	return fmt.Sprintf(
		"[system] This is the user's scheduled job %q, running for %s. The user is not watching this turn and cannot answer questions, "+
			"and only read-only tools are available, so nothing can be created, changed, or saved. "+
			"Never show a status, time, or count that no tool returned in this run, and never carry one over from an earlier run; "+
			"if a check failed, say which one, and leave a leg out rather than guess its status. "+
			"Do the task below and reply with the report the user should receive.\n\n%s",
		occurrence.JobName, at, prompt,
	)
}
