package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// MaximumNotesRunes bounds a job's notes to what a rewrite of a job's instructions may hold, so
// the whole notes always fit one confirmation.
const MaximumNotesRunes = 2000

// notesField is the key of a job's notes in its stored configuration.
const notesField = "notes"

// Notes reads a job's notes from its stored configuration, "" when it has none.
func Notes(config json.RawMessage) string {
	var decoded struct {
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal(config, &decoded); err != nil {
		return ""
	}
	return decoded.Notes
}

// WithNotes sets a job's notes in its stored configuration, keeping its other fields; empty notes
// remove the field. Notes that cannot be stored return an error the model can act on.
func WithNotes(config json.RawMessage, notes string) (json.RawMessage, error) {
	notes = strings.TrimSpace(notes)
	if err := CheckNotes(notes); err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if given(config) {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, fmt.Errorf("the job's configuration could not be read")
		}
	}
	if notes == "" {
		delete(fields, notesField)
		return json.Marshal(fields)
	}
	encoded, err := json.Marshal(notes)
	if err != nil {
		return nil, err
	}
	fields[notesField] = encoded
	return json.Marshal(fields)
}

// CheckNotes reports whether notes may be stored: valid UTF-8 of at most MaximumNotesRunes.
func CheckNotes(notes string) error {
	if !utf8.ValidString(notes) {
		return fmt.Errorf("the notes must be valid UTF-8")
	}
	if runes := utf8.RuneCountInString(notes); runes > MaximumNotesRunes {
		return fmt.Errorf("the notes are %d characters and may be at most %d, so cut %d or more", runes, MaximumNotesRunes, runes-MaximumNotesRunes)
	}
	return nil
}

// ValidateNotesConfig accepts a handler configuration that holds nothing but, optionally, notes
// CheckNotes allows, for the Validate of a job type with EditableNotes and no other settings.
func ValidateNotesConfig(config json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	var decoded struct {
		Notes string `json:"notes"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("this job takes no settings other than its notes")
	}
	return CheckNotes(decoded.Notes)
}

// notesLabel heads the notes of a job with EditableNotes wherever the model reads them, so they
// are edited as the user's preferences and not taken for the whole instruction.
func notesLabel(job Job) string {
	if job.ID == InboxJobID(job.PrincipalID) {
		return inboxNotesLabel
	}
	return "Notes (the user's preferences; how this job works is built in)"
}

// shownNotes is a job's notes as the model reads them, saying so when there are none.
func shownNotes(job Job) string {
	if notes := Notes(job.Config); notes != "" {
		return notes
	}
	return "(none yet)"
}

// toolUpdateNotes is the approval action of a notes edit made with schedule_update; the model is
// never offered it as a tool.
const toolUpdateNotes = "schedule_update_notes"

var (
	notesEditsSummary   = locale.T("Change the notes of %s. Only these passages change:", "Изменить заметки задания %s. Меняются только эти места:")
	notesRewriteSummary = locale.T("Change the notes of %[1]s to:\n%[2]s", "Изменить заметки задания %[1]s на:\n%[2]s")
	notesChanged        = locale.T("Changed the notes of %s.", "Заметки задания %s изменены.")
	notesInvalid        = locale.T("Those notes can no longer be saved (%s); nothing was changed.", "Эти заметки больше нельзя сохранить (%s); ничего не изменено.")
)

// notesArguments are the only schedule_update arguments a job with EditableNotes takes; any
// other is refused by name, because only its notes can change.
var notesArguments = []string{"job_id", "prompt", "prompt_edits"}

// longNotesText answers new notes too long to show the user in full for confirmation.
const longNotesText = "The new notes are too long to show the user in full for confirmation; nothing was changed. Keep them shorter, or change only the passages that need it with prompt_edits."

// confirmedNotes is a job's notes as they will be after the change, with Was, the notes as they
// were proposed over. Every run moves a job's last change, so a press is refused only when the
// notes themselves changed since, and an hourly job's card stays good across its runs.
type confirmedNotes struct {
	JobID string `json:"job_id"`
	Was   string `json:"was"`
	Notes string `json:"notes"`
}

// proposeNotesUpdate changes the notes of a job whose type has EditableNotes, from prompt, the
// complete new notes, or prompt_edits applied to the current ones.
func (s *Service) proposeNotesUpdate(ctx context.Context, principalID string, call tools.Call, job Job, arguments updateArguments) (tools.Result, error) {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(call.Arguments, &raw)
	var fixed []string
	for name, value := range raw {
		if !slices.Contains(notesArguments, name) && given(value) {
			fixed = append(fixed, name)
		}
	}
	slices.Sort(fixed)
	if len(fixed) > 0 {
		return tools.Result{Text: fmt.Sprintf("%q is a built-in job, turned on and off in /settings, and only its notes can change: give just prompt or prompt_edits, and leave out %s; nothing was proposed.", job.Name, strings.Join(fixed, ", ")), IsError: true}, nil
	}
	rewrite, edits := strings.TrimSpace(arguments.Prompt) != "", len(arguments.PromptEdits) > 0
	switch {
	case rewrite && edits:
		return tools.Result{Text: "Give either prompt, the complete new notes, or prompt_edits, not both; nothing was proposed.", IsError: true}, nil
	case !rewrite && !edits:
		return tools.Result{Text: "Give prompt, the complete new notes, or prompt_edits, changes to the current notes; nothing was proposed.", IsError: true}, nil
	}
	current := Notes(job.Config)
	next := arguments.Prompt
	if edits {
		edited, err := applyPromptEdits(current, arguments.PromptEdits)
		if err != nil {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		next = edited
	}
	next = strings.TrimSpace(next)
	if next == strings.TrimSpace(current) {
		return tools.Result{Text: "That changes nothing; the notes already read exactly like this.", IsError: true}, nil
	}
	if _, err := s.notesConfig(job, next); err != nil {
		return tools.Result{Text: err.Error() + "; nothing was proposed.", IsError: true}, nil
	}
	canonical, err := json.Marshal(confirmedNotes{JobID: job.ID, Was: strings.TrimSpace(current), Notes: next})
	if err != nil {
		return tools.Result{}, err
	}
	language := locale.From(ctx)
	name := quotedIn(language, job.Name)
	summary, tooLong := notesRewriteSummary.FormatIn(language, name, next), longNotesText
	if edits {
		var builder strings.Builder
		builder.WriteString(notesEditsSummary.FormatIn(language, name))
		writeEdits(&builder, language, arguments.PromptEdits)
		summary, tooLong = builder.String(), longEditsText
	}
	if !job.Enabled {
		summary += "\n" + staysPausedNote.In(language)
	}
	return s.act(ctx, principalID, call, approval.Proposal{Tool: toolUpdateNotes, Arguments: canonical, Summary: summary, Intent: approval.Intent{Verb: "change", TargetKind: "notes of the scheduled job", TargetName: job.Name}}, tooLong)
}

// notesConfig is the job's stored configuration with the given notes, checked as its type checks
// a new job's.
func (s *Service) notesConfig(job Job, notes string) (json.RawMessage, error) {
	registered, known := s.types[job.Type]
	if !known || !registered.EditableNotes {
		return nil, fmt.Errorf("that kind of job has no notes")
	}
	config, err := WithNotes(job.Config, notes)
	if err != nil {
		return nil, err
	}
	handler, _, _, err := SplitConfig(config)
	if err != nil {
		return nil, err
	}
	if err := registered.Handler.Validate(handler); err != nil {
		return nil, err
	}
	return config, nil
}

// executeNotesUpdate stores confirmed notes, keeping the job's schedule and next run.
func (s *Service) executeNotesUpdate(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed confirmedNotes
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
		return approval.Outcome{}, actionError(ctx, "malformed", changeUnreadable)
	}
	job, found, err := s.repository.LoadScheduledJob(ctx, record.PrincipalID, confirmed.JobID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if registered, known := s.types[job.Type]; !found || !job.FinishedAt.IsZero() || !known || !registered.EditableNotes {
		return approval.Outcome{}, actionError(ctx, "not_found", jobGoneNotChanged)
	}
	stale := actionError(ctx, "stale", jobStale)
	if strings.TrimSpace(Notes(job.Config)) != confirmed.Was {
		return approval.Outcome{}, stale
	}
	config, err := s.notesConfig(job, confirmed.Notes)
	if err != nil {
		return approval.Outcome{}, actionError(ctx, "invalid", notesInvalid, err.Error())
	}
	changed := job
	changed.Config, changed.UpdatedAt = config, s.now()
	updated, err := s.repository.UpdateScheduledJob(ctx, changed, job.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, stale
	}
	s.logger.InfoContext(ctx, "schedule.job.updated", "principal_id", job.PrincipalID, "job_id", job.ID, "job_type", job.Type)
	undo, err := s.undoUpdate(ctx, job, false)
	if err != nil {
		return approval.Outcome{}, err
	}
	language := locale.From(ctx)
	return approval.Outcome{Text: notesChanged.FormatIn(locale.English, quotedIn(locale.English, job.Name)), UserText: notesChanged.FormatIn(language, quotedIn(language, job.Name)), Undo: undo}, nil
}
