package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	MaximumProcedureTitleRunes = 80
	MaximumProcedureStepsRunes = 1000
	MaximumProcedures          = 20

	toolProcedureRead = "procedure_read"
	toolProcedureSave = "procedure_save"

	procedureReadGuidance = "Procedures: when a request matches a procedure listed in the Memory section, read it with procedure_read first and follow its steps, adapting them to the request."
	procedureSaveGuidance = "Propose keeping a procedure with procedure_save only when a request needed several tool calls, or a recovery from failures, to succeed and the same kind of request is likely again, or when the user asks you to remember how something was done. " +
		"The title names the kind of request; the steps name the tools, the arguments that mattered, the order, and what to avoid. Never put private values in the steps, such as addresses, names, or account details; those belong in facts. " +
		"Every save is a proposal the user confirms after your reply, so never claim a procedure was kept. To improve a kept procedure, save it again with its id. A user keeps at most 20 procedures and deletes them in /memory."

	// outcomeUnknown marks a save naming no kept procedure; it is the model's mistake, not a write
	// outcome, so it is never counted.
	outcomeUnknown = "unknown"
)

var (
	procedureReadDefinition = tools.Definition{
		Name:        toolProcedureRead,
		Description: "Read the steps of a procedure listed in the Memory section, by its id, before following it.",
		Risk:        tools.RiskRead,
		Confirmed:   true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"The procedure's id from the Memory section."}},` +
			`"required":["id"],"additionalProperties":false}`),
	}
	procedureSaveDefinition = tools.Definition{
		Name:        toolProcedureSave,
		Description: "Propose keeping how a request was done as a procedure to read before similar later requests; the user confirms it after your reply. With an id, it replaces that procedure.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"title":{"type":"string","description":"The kind of request it is for, at most 80 characters."},` +
			`"steps":{"type":"string","description":"The tools, the arguments that mattered, the order, and what to avoid, at most 1000 characters."},` +
			`"id":{"type":"string","description":"The id of a kept procedure this one replaces; leave out for a new one."}},` +
			`"required":["title","steps"],"additionalProperties":false}`),
	}
)

// Procedure is how a request that needed several steps was done, kept for similar later requests.
// The prompt lists its title; the model reads its steps with procedure_read. ReadAt is zero until
// it is first read.
type Procedure struct {
	ID          string
	PrincipalID string
	Title       string
	Steps       string
	Source      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ReadAt      time.Time
}

type procedureReadArguments struct {
	ID string `json:"id"`
}

type procedureSaveArguments struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Steps string `json:"steps"`
}

// proposedProcedure is what a procedure_save record stores; ID is "" for a new procedure. Updated
// is when the replaced procedure last changed, so a confirmation does not overwrite a procedure
// that changed since the card was shown.
type proposedProcedure struct {
	ID      string    `json:"id,omitempty"`
	Title   string    `json:"title"`
	Steps   string    `json:"steps"`
	Updated time.Time `json:"updated,omitzero"`
}

func (s *Service) Procedures(ctx context.Context, principalID string) ([]Procedure, error) {
	return s.repository.ListProcedures(ctx, principalID)
}

func (s *Service) RemoveProcedure(ctx context.Context, principalID, id string) (bool, error) {
	return s.repository.RemoveProcedure(ctx, principalID, id)
}

// LearnsProcedures reports whether a chat turn may propose a procedure: the user has memory on and
// has not turned learning procedures off.
func (s *Service) LearnsProcedures(ctx context.Context, principalID string) (bool, error) {
	enabled, err := s.repository.MemoryEnabled(ctx, principalID)
	if err != nil || !enabled {
		return false, err
	}
	return s.repository.LearnProcedures(ctx, principalID)
}

func (s *Service) SetLearnProcedures(ctx context.Context, principalID string, learn bool) error {
	return s.repository.SetLearnProcedures(ctx, principalID, learn)
}

func (s *Service) recordProcedureRead(ctx context.Context, principalID string) {
	if s.telemetry != nil {
		s.telemetry.RecordProcedureRead(ctx, principalID)
	}
}

func renderProcedureTitles(procedures []Procedure) string {
	lines := make([]string, 0, len(procedures))
	for _, procedure := range procedures {
		lines = append(lines, "- ["+procedure.ID+"] "+procedure.Title)
	}
	return strings.Join(lines, "\n")
}

// contributeProcedures adds procedure_read while the principal keeps a procedure, and
// procedure_save while they learn procedures, to a memory contribution.
func (s *Service) contributeProcedures(ctx context.Context, principalID string, contribution tools.Contribution) (tools.Contribution, error) {
	procedures, err := s.repository.ListProcedures(ctx, principalID)
	if err != nil {
		return tools.Contribution{}, err
	}
	learn, err := s.repository.LearnProcedures(ctx, principalID)
	if err != nil {
		return tools.Contribution{}, err
	}
	if len(procedures) > 0 {
		contribution.Guidance = procedureReadGuidance
		contribution.Definitions = append(contribution.Definitions, procedureReadDefinition)
	}
	if learn {
		contribution.WriteGuidance += " " + procedureSaveGuidance
		contribution.Definitions = append(contribution.Definitions, procedureSaveDefinition)
	}
	return contribution, nil
}

func (s *Service) readProcedure(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments procedureReadArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	procedure, found, err := s.repository.ReadProcedure(ctx, principalID, strings.TrimSpace(arguments.ID), s.now())
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: unknownProcedureText, IsError: true}, nil
	}
	s.recordProcedureRead(ctx, principalID)
	return tools.Result{Text: "Procedure: " + procedure.Title + "\n" + procedure.Steps}, nil
}

const unknownProcedureText = "No procedure has that id. The ids are in the Memory section of your prompt."

func (s *Service) saveProcedure(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments procedureSaveArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	stored := proposedProcedure{ID: strings.TrimSpace(arguments.ID), Title: normalize(arguments.Title), Steps: normalizeSteps(arguments.Steps)}
	if reason := validProcedure(stored.Title, stored.Steps); reason != "" {
		return tools.Result{Text: reason, IsError: true}, nil
	}
	procedures, err := s.repository.ListProcedures(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	replaced, rejected := checkProcedure(procedures, stored)
	if rejected.outcome != "" {
		if rejected.outcome != outcomeUnknown {
			s.record(ctx, principalID, KindProcedure, rejected.outcome)
		}
		return tools.Result{Text: rejected.text, IsError: true}, nil
	}
	stored.Updated = replaced.UpdatedAt
	canonical, err := json.Marshal(stored)
	if err != nil {
		return tools.Result{}, err
	}
	summary := "Keep this procedure: " + stored.Title
	if stored.ID != "" {
		summary = fmt.Sprintf("Replace the procedure %q with this one: %s", replaced.Title, stored.Title)
	}
	proposal := approval.Proposal{
		Tool: toolProcedureSave, Arguments: canonical,
		Summary: summary + "\n" + stored.Steps + "\nI will read it before similar requests until you delete it in /memory.",
	}
	exposed := s.exposed(ctx, principalID)
	if exposed {
		proposal.Summary = exposedProcedureWarning + "\n" + proposal.Summary
	}
	return s.propose(ctx, principalID, exposed, stored.Title+"\n"+stored.Steps, KindProcedure, proposal)
}

// normalizeSteps keeps the steps' lines, since steps read best one to a line, and makes each line
// plain: its spacing collapsed and empty lines dropped.
func normalizeSteps(steps string) string {
	var lines []string
	for line := range strings.SplitSeq(steps, "\n") {
		if line = normalize(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func validProcedure(title, steps string) string {
	switch {
	case title == "" || steps == "":
		return "title and steps must not be empty."
	case !utf8.ValidString(title) || !utf8.ValidString(steps):
		return "title and steps must be valid UTF-8."
	case utf8.RuneCountInString(title) > MaximumProcedureTitleRunes:
		return fmt.Sprintf("title is %d characters; a procedure's title holds at most %d. Shorten it.", utf8.RuneCountInString(title), MaximumProcedureTitleRunes)
	case utf8.RuneCountInString(steps) > MaximumProcedureStepsRunes:
		return fmt.Sprintf("steps are %d characters; a procedure's steps hold at most %d. Keep only what matters.", utf8.RuneCountInString(steps), MaximumProcedureStepsRunes)
	}
	return ""
}

// checkProcedure judges a save against the principal's procedures and returns the one it replaces.
func checkProcedure(procedures []Procedure, saved proposedProcedure) (Procedure, rejection) {
	var replaced Procedure
	if saved.ID != "" {
		found := false
		for _, procedure := range procedures {
			if procedure.ID == saved.ID {
				replaced, found = procedure, true
			}
		}
		if !found {
			return Procedure{}, rejection{outcomeUnknown, unknownProcedureText}
		}
	}
	for _, procedure := range procedures {
		if procedure.ID != saved.ID && strings.EqualFold(procedure.Title, saved.Title) {
			return Procedure{}, rejection{outcomeDuplicate, "Not saved: a procedure with that title is kept already (id " + procedure.ID + "). Save with that id to replace it."}
		}
	}
	if saved.ID == "" && len(procedures) >= MaximumProcedures {
		return Procedure{}, rejection{outcomeCap, fmt.Sprintf("Not saved: the user keeps %d procedures, the most there may be. Replace one that covers a similar request by saving with its id, or tell the user to delete one in /memory.", MaximumProcedures)}
	}
	return replaced, rejection{}
}

// executeProcedure writes a confirmed procedure, checking again the procedures it was judged
// against, which can change between the proposal and the press.
func (s *Service) executeProcedure(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed proposedProcedure
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil || validProcedure(confirmed.Title, confirmed.Steps) != "" {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed procedure could not be read; nothing was kept."}
	}
	procedures, err := s.repository.ListProcedures(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	replaced, rejected := checkProcedure(procedures, confirmed)
	if rejected.outcome != "" {
		switch rejected.outcome {
		case outcomeUnknown:
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That procedure no longer exists."}
		case outcomeDuplicate:
			s.record(ctx, record.PrincipalID, KindProcedure, rejected.outcome)
			return approval.Outcome{}, &approval.ActionError{Reason: rejected.outcome, Text: "A procedure with that title is kept already; nothing was changed."}
		default:
			s.record(ctx, record.PrincipalID, KindProcedure, rejected.outcome)
			return approval.Outcome{}, &approval.ActionError{Reason: rejected.outcome, Text: "You keep the most procedures there may be; delete one in /memory and ask again. Nothing was kept."}
		}
	}
	now := s.now()
	if confirmed.ID != "" {
		if !replaced.UpdatedAt.Equal(confirmed.Updated) {
			return approval.Outcome{}, &approval.ActionError{Reason: "changed", Text: "That procedure changed since; nothing was changed."}
		}
		found, err := s.repository.ReplaceProcedure(ctx, record.PrincipalID, confirmed.ID, confirmed.Title, confirmed.Steps, SourceConfirmed, now)
		if err != nil {
			return approval.Outcome{}, err
		}
		if !found {
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That procedure no longer exists."}
		}
		s.record(ctx, record.PrincipalID, KindProcedure, outcomeWritten)
		return approval.Outcome{Text: "Updated the procedure."}, nil
	}
	id, err := s.uniqueID(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if err := s.repository.AddProcedure(ctx, Procedure{ID: id, PrincipalID: record.PrincipalID, Title: confirmed.Title, Steps: confirmed.Steps, Source: SourceConfirmed, CreatedAt: now, UpdatedAt: now}); err != nil {
		return approval.Outcome{}, err
	}
	s.record(ctx, record.PrincipalID, KindProcedure, outcomeWritten)
	return approval.Outcome{Text: "Kept the procedure."}, nil
}
