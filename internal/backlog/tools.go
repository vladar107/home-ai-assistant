package backlog

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolAdd    = "backlog_add"
	toolList   = "backlog_list"
	toolUpdate = "backlog_update"

	// listedItems and listedDetailsRunes keep a listing well inside the broker's result limit; one
	// item is read in full by its id.
	listedItems        = 20
	listedDetailsRunes = 150

	guidance = "Backlog: the user keeps a backlog of ideas for developing you later, to work on in a separate development session. " +
		"Use backlog_add when the user describes something you yourself should do or do differently in future, or asks to note an idea to build, for example add to the backlog, idea for later, or remember to implement. " +
		"Use memory instead for facts and preferences you should apply now, and a reminder for something the user must do at a time. " +
		"Give a short title and, in details, what the user said and why, written so that a developer without this conversation can act on it. " +
		"The backlog never changes how you behave; read it only with backlog_list when the user asks about it, and change an item with backlog_update by its id. " +
		"After adding, tell the user in one short line what you added; they see and manage the list with /backlog."
)

var definitions = []tools.Definition{
	{
		Name:        toolAdd,
		Description: "Add an idea for developing the assistant, a feature or change to build later in a development session, to the user's backlog; facts and preferences belong in memory instead.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"title":{"type":"string","description":"A short title, at most 120 characters."},` +
			`"details":{"type":"string","description":"What the user said and why, so a developer without the conversation can act on it; at most 2000 characters."}},` +
			`"required":["title","details"],"additionalProperties":false}`),
	},
	{
		Name:        toolList,
		Description: "List the user's backlog items, newest first, with their ids and shortened details; with an id, read that one item in full.",
		Risk:        tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"status":{"type":"string","enum":["open","done"],"description":"open (the default) or done."},` +
			`"id":{"type":"string","description":"An item's id, to read it in full."}},` +
			`"additionalProperties":false}`),
	},
	{
		Name:        toolUpdate,
		Description: "Change one backlog item by its id: a new title or details, or its status.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"The item's id from backlog_list."},` +
			`"title":{"type":"string","description":"The new title, at most 120 characters."},` +
			`"details":{"type":"string","description":"The new details, replacing the old ones; at most 2000 characters."},` +
			`"status":{"type":"string","enum":["open","done"],"description":"done when it was implemented or is no longer wanted, open to reopen it."}},` +
			`"required":["id"],"additionalProperties":false}`),
	},
}

type addArguments struct {
	Title   string `json:"title"`
	Details string `json:"details"`
}

type listArguments struct {
	Status string `json:"status"`
	ID     string `json:"id"`
}

type updateArguments struct {
	ID      string  `json:"id"`
	Title   *string `json:"title"`
	Details *string `json:"details"`
	Status  *string `json:"status"`
}

// Proposer is the core approval service as the backlog sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// Exposure answers whether the calling conversation has read text from outside the household.
type Exposure interface {
	External(ctx context.Context, principalID string) bool
}

var _ tools.Provider = (*Service)(nil)
var _ approval.Executor = (*Service)(nil)

// Contribute offers the backlog tools on chat turns alone: the backlog is the user's own list, so
// a scheduled run or background task neither reads nor writes it.
func (s *Service) Contribute(ctx context.Context, _ string) (tools.Contribution, error) {
	if _, chat := outbox.ChatRequest(tools.TurnFrom(ctx)); !chat {
		return tools.Contribution{}, nil
	}
	return tools.Contribution{Guidance: guidance, Definitions: definitions}, nil
}

func (s *Service) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolAdd:
		return s.executeAdd(ctx, principalID, call)
	case toolList:
		return s.executeList(ctx, principalID, call)
	case toolUpdate:
		return s.executeUpdate(ctx, principalID, call)
	default:
		return tools.Result{Text: "unknown backlog tool", IsError: true}, nil
	}
}

// external reports whether the conversation has read text from outside the household, after which
// a write could carry that text's instructions into a development session and is confirmed first.
func (s *Service) external(ctx context.Context, principalID string) bool {
	return s.exposure != nil && s.exposure.External(ctx, principalID)
}

func (s *Service) executeAdd(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments addArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	title, details := normalizeTitle(arguments.Title), normalizeDetails(arguments.Details)
	if reason := cmp.Or(validTitle(title), validDetails(details)); reason != "" {
		return tools.Result{Text: reason, IsError: true}, nil
	}
	if s.external(ctx, principalID) {
		canonical, err := json.Marshal(addArguments{Title: title, Details: details})
		if err != nil {
			return tools.Result{}, err
		}
		return s.propose(ctx, principalID, approval.Proposal{
			Tool: toolAdd, Arguments: canonical,
			Summary: fmt.Sprintf("Add to your backlog: %s\n%s\nI read outside content in this conversation, so I need your confirmation.", title, details),
			Intent:  approval.Intent{Verb: "add", TargetKind: "backlog item", TargetName: title},
		})
	}
	item, rejected, err := s.add(ctx, principalID, title, details)
	if err != nil {
		return tools.Result{}, err
	}
	if rejected != "" {
		return tools.Result{Text: string(rejected), IsError: true}, nil
	}
	return tools.Result{Text: fmt.Sprintf("Added to the backlog (id %s): %s", item.ID, item.Title)}, nil
}

func (s *Service) executeList(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments listArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if id := strings.TrimSpace(arguments.ID); id != "" {
		item, found, err := s.repository.LoadBacklogItem(ctx, principalID, id)
		if err != nil {
			return tools.Result{}, err
		}
		if !found {
			return tools.Result{Text: string(unknownItemText), IsError: true}, nil
		}
		return tools.Result{Text: fmt.Sprintf("[%s] %s (%s, added %s)\n%s", item.ID, item.Title, item.Status, item.CreatedAt.UTC().Format("2006-01-02"), item.Details)}, nil
	}
	status := arguments.Status
	if status == "" {
		status = StatusOpen
	}
	if status != StatusOpen && status != StatusDone {
		return tools.Result{Text: "status must be open or done.", IsError: true}, nil
	}
	items, err := s.repository.ListBacklog(ctx, principalID, status)
	if err != nil {
		return tools.Result{}, err
	}
	if len(items) == 0 {
		return tools.Result{Text: fmt.Sprintf("The backlog has no %s items.", status)}, nil
	}
	lines := []string{fmt.Sprintf("%d %s backlog items, newest first:", len(items), status)}
	for index := len(items) - 1; index >= 0 && len(items)-index <= listedItems; index-- {
		item := items[index]
		line := fmt.Sprintf("- [%s] %s (added %s)", item.ID, item.Title, item.CreatedAt.UTC().Format("2006-01-02"))
		if item.Details != "" {
			line += ": " + shorten(normalizeTitle(item.Details), listedDetailsRunes)
		}
		lines = append(lines, line)
	}
	if len(items) > listedItems {
		lines = append(lines, fmt.Sprintf("%d older items are not shown.", len(items)-listedItems))
	}
	return tools.Result{Text: strings.Join(lines, "\n")}, nil
}

func (s *Service) executeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments updateArguments
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if arguments.Title == nil && arguments.Details == nil && arguments.Status == nil {
		return tools.Result{Text: "Give a new title, details, or status.", IsError: true}, nil
	}
	item, found, err := s.repository.LoadBacklogItem(ctx, principalID, strings.TrimSpace(arguments.ID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: string(unknownItemText), IsError: true}, nil
	}
	next := change{ID: item.ID, Title: item.Title, Details: item.Details, Status: item.Status}
	if arguments.Title != nil {
		next.Title = normalizeTitle(*arguments.Title)
	}
	if arguments.Details != nil {
		next.Details = normalizeDetails(*arguments.Details)
	}
	if arguments.Status != nil {
		next.Status = *arguments.Status
	}
	if next.Status != StatusOpen && next.Status != StatusDone {
		return tools.Result{Text: "status must be open or done.", IsError: true}, nil
	}
	if reason := cmp.Or(validTitle(next.Title), validDetails(next.Details)); reason != "" {
		return tools.Result{Text: reason, IsError: true}, nil
	}
	if s.external(ctx, principalID) {
		canonical, err := json.Marshal(next)
		if err != nil {
			return tools.Result{}, err
		}
		return s.propose(ctx, principalID, approval.Proposal{
			Tool: toolUpdate, Arguments: canonical,
			Summary: fmt.Sprintf("Change the backlog item %q to:\n%s (%s)\n%s\nI read outside content in this conversation, so I need your confirmation.", item.Title, next.Title, next.Status, next.Details),
			Intent:  approval.Intent{Verb: "change", TargetKind: "backlog item", TargetName: item.Title, NewName: next.Title},
		})
	}
	rejected, err := s.update(ctx, principalID, next)
	if err != nil {
		return tools.Result{}, err
	}
	if rejected != "" {
		return tools.Result{Text: string(rejected), IsError: true}, nil
	}
	return tools.Result{Text: fmt.Sprintf("Updated the backlog item (id %s): %s (%s)", next.ID, next.Title, next.Status)}, nil
}

func (s *Service) propose(ctx context.Context, principalID string, proposal approval.Proposal) (tools.Result, error) {
	if s.proposer == nil {
		return tools.Result{Text: "I read outside content in this conversation, so backlog changes need the user's confirmation, and confirming is not available. Tell the user nothing was changed.", IsError: true}, nil
	}
	if _, err := s.proposer.Propose(ctx, principalID, proposal); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		s.logger.WarnContext(ctx, "backlog.proposal.failed", "principal_id", principalID, "tool", proposal.Tool, "error", err)
		return tools.Result{Text: "the proposal could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: approval.ProposedText}, nil
}

func (s *Service) ActionTools() []string {
	return []string{toolAdd, toolUpdate}
}

// ExecuteApproved makes the confirmed change. The cap and the item are checked again, because the
// backlog can change between the proposal and the press.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var rejected rejection
	var err error
	switch record.Tool {
	case toolAdd:
		var confirmed addArguments
		if json.Unmarshal(record.Arguments, &confirmed) != nil || validTitle(confirmed.Title) != "" || validDetails(confirmed.Details) != "" {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed item could not be read; nothing was added."}
		}
		_, rejected, err = s.add(ctx, record.PrincipalID, confirmed.Title, confirmed.Details)
	case toolUpdate:
		var confirmed change
		if json.Unmarshal(record.Arguments, &confirmed) != nil || validTitle(confirmed.Title) != "" || validDetails(confirmed.Details) != "" ||
			(confirmed.Status != StatusOpen && confirmed.Status != StatusDone) {
			return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed change could not be read; nothing was changed."}
		}
		rejected, err = s.update(ctx, record.PrincipalID, confirmed)
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
	switch {
	case err != nil:
		return approval.Outcome{}, err
	case rejected == fullText:
		return approval.Outcome{}, &approval.ActionError{Reason: "full", Text: fmt.Sprintf("The backlog already holds %d open items; mark some done or delete them in /backlog. Nothing was changed.", MaximumOpenItems)}
	case rejected != "":
		return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "That backlog item no longer exists."}
	case record.Tool == toolAdd:
		return approval.Outcome{Text: "Added to the backlog."}, nil
	default:
		return approval.Outcome{Text: "Updated."}, nil
	}
}

func shorten(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func decodeArguments(raw json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("the arguments do not match the tool's schema")
	}
	return nil
}
