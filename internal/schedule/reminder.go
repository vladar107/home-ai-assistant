package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
)

const (
	TypeReminder = "reminder.text"

	maximumReminderRunes = 500
	// lateAfter is how long after its time a reminder is delivered before it says when it was due.
	lateAfter = 5 * time.Minute
	// reminderCatchUp is effectively forever: a reminder is worth receiving however late it is.
	reminderCatchUp = 100 * 365 * 24 * time.Hour
)

type reminderConfig struct {
	Text string `json:"text"`
}

// Reminder is the job type that sends the owner a text they wrote, once, without asking the
// model, so it arrives even without a connected subscription or remaining quota.
func Reminder(now func() time.Time) JobType {
	return JobType{
		Name:            TypeReminder,
		Handler:         reminder{now: now},
		CatchUp:         reminderCatchUp,
		MinimumInterval: time.Minute,
		Once:            true,
		Light:           true,
	}
}

// ReminderConfig encodes the text of a reminder.text job.
func ReminderConfig(text string) (json.RawMessage, error) {
	return json.Marshal(reminderConfig{Text: strings.TrimSpace(text)})
}

// ReminderText reads the stored text back for display.
func ReminderText(config json.RawMessage) string {
	var decoded reminderConfig
	if err := json.Unmarshal(config, &decoded); err != nil {
		return ""
	}
	return decoded.Text
}

// ReminderName is the job name of a reminder: its text on one line, shortened to fit.
func ReminderName(text string) string {
	name := strings.Join(strings.FieldsFunc(text, unicode.IsControl), " ")
	return clip.Runes(strings.Join(strings.Fields(name), " "), maximumNameRunes)
}

type reminder struct {
	now func() time.Time
}

func (r reminder) Validate(config json.RawMessage) error {
	_, err := decodeReminder(config)
	return err
}

func decodeReminder(config json.RawMessage) (reminderConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	var decoded reminderConfig
	if err := decoder.Decode(&decoded); err != nil {
		return reminderConfig{}, fmt.Errorf("the reminder could not be read")
	}
	text := strings.TrimSpace(decoded.Text)
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maximumReminderRunes {
		return reminderConfig{}, fmt.Errorf("the reminder text must be between 1 and %d characters", maximumReminderRunes)
	}
	decoded.Text = text
	return decoded, nil
}

func (r reminder) Run(_ context.Context, occurrence Occurrence) (Result, error) {
	config, err := decodeReminder(occurrence.Config)
	if err != nil {
		return Result{Text: FailedText, ErrorKind: "config"}, nil
	}
	text := config.Text
	if r.now().Sub(occurrence.ScheduledFor) > lateAfter {
		text += "\n\n(This was due " + FormatOccurrence(occurrence.ScheduledFor, occurrence.Location) + ".)"
	}
	return Result{Text: text}, nil
}
