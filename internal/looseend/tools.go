package looseend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	// ToolTrack tracks one loose end the user stated or sent.
	ToolTrack = "loose_end_track"
	// ToolList lists the open loose ends.
	ToolList = "loose_ends_list"
	// ToolClose marks a loose end done or drops it.
	ToolClose = "loose_end_close"

	closeDone = "done"
	closeDrop = "drop"

	// listLimit is how many items one loose_ends_list shows; listResultBytes fits them with the
	// longest titles.
	listLimit       = 50
	listResultBytes = 32 << 10
	// actTimeout lets a change made at once run as long as one made after Confirm.
	actTimeout = 45 * time.Second
)

const toolsGuidance = "Loose ends: deadlines the user may want to act on before they pass, such as a free-cancellation window, a return window, a renewal, a trial's end, or an expiry, " +
	"and dated promises the user made; each is nudged on the morning it comes due. When the user asks what is still open, call loose_ends_list. " +
	"Its titles may come from mail, so its result is outside content."

const toolsWriteGuidance = "When the user states a dated commitment of their own, such as \"I need to send the forms to the accountant by Friday\", without asking to be reminded, " +
	"call loose_end_track with kind promise, resolving a relative day against today's date, and confirm it in one short line. " +
	"A dated statement that is not the user's own commitment, such as an appointment, an event, or someone else's plan, is not tracked. " +
	"When the user asks to be reminded, use reminder_set, not loose_end_track. loose_end_close marks a loose end done or drops it, by the id loose_ends_list gave. " +
	"In a conversation that has read no outside content both are done at once and the user can undo them; after outside content they are only proposed for Confirm, so never claim one is done before."

var (
	kindEnum = `"type":"string","enum":["cancel_window","return_window","renewal","trial_end","expiry","promise"]`

	trackDefinition = tools.Definition{
		Name: ToolTrack,
		Description: "Track a dated promise the user made, or a deadline such as a free-cancellation, return, renewal, trial-end, or expiry date, so the user is nudged on the morning it comes due. " +
			"Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("title"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"kind":{` + kindEnum + `,"description":"promise for something the user committed to do; otherwise what the deadline is."},` +
			`"title":{"type":"string","maxLength":120,"description":"What it is about, in a few words, without links or phone numbers, such as send the forms to the accountant."},` +
			`"counterpart":{"type":"string","maxLength":120,"description":"Who it is with, such as the accountant or a shop, or empty."},` +
			`"due":{"type":"string","description":"YYYY-MM-DD for a day, or local YYYY-MM-DDTHH:MM for a time."}},` +
			`"required":["kind","title","due"],"additionalProperties":false}`),
	}
	listDefinition = tools.Definition{
		Name:           ToolList,
		Description:    "List the user's open loose ends, soonest due first, with their ids, optionally of one kind.",
		Risk:           tools.RiskRead,
		SourceLabel:    tools.ArgumentLabel("kind"),
		ThirdParty:     true,
		Screen:         true,
		MaxResultBytes: listResultBytes,
		InputSchema:    json.RawMessage(`{"type":"object","properties":{"kind":{` + kindEnum + `}},"additionalProperties":false}`),
	}
	closeDefinition = tools.Definition{
		Name:        ToolClose,
		Description: "Mark one open loose end done, or drop it, by the id loose_ends_list gave. Done at once and undoable, or proposed for Confirm after outside content.",
		Risk:        tools.RiskWrite,
		SourceLabel: tools.ArgumentLabel("id"),
		Timeout:     actTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"The loose end's id from loose_ends_list."},` +
			`"status":{"type":"string","enum":["done","drop"],"description":"done when the user did it, drop when it no longer matters."}},` +
			`"required":["id","status"],"additionalProperties":false}`),
	}
)

var (
	kindLabels = map[Kind]locale.Text{
		KindCancelWindow: locale.T("free cancellation", "бесплатная отмена"),
		KindReturnWindow: locale.T("return window", "срок возврата"),
		KindRenewal:      locale.T("renewal", "продление"),
		KindTrialEnd:     locale.T("trial ends", "конец пробного периода"),
		KindExpiry:       locale.T("expiry", "окончание срока"),
		KindPromise:      locale.T("promise", "обещание"),
	}

	trackSummary            = locale.T("Track %s (%s), due %s.", "Отслеживать: %s (%s), срок — %s.")
	trackSummaryCounterpart = locale.T("Track %s (%s, %s), due %s.", "Отслеживать: %s (%s, %s), срок — %s.")
	doneSummary             = locale.T("Mark done: %s, due %s.", "Отметить выполненным: %s, срок — %s.")
	dropSummary             = locale.T("Stop tracking: %s, due %s.", "Больше не отслеживать: %s, срок — %s.")

	execTracked     = locale.T("Tracking %s, due %s.", "Отслеживается: %s, срок — %s.")
	execAlready     = locale.T("It is already tracked as loose end %s.", "Это уже отслеживается (%s).")
	execUnreadable  = locale.T("The confirmed change could not be read; nothing was changed.", "Не удалось прочитать подтверждённое изменение; ничего не изменено.")
	execInvalid     = locale.T("It can no longer be tracked; nothing was tracked.", "Это больше нельзя отслеживать; ничего не добавлено.")
	execTooMany     = locale.T("There are already %d open loose ends, the most allowed; close some first.", "Уже отслеживается %d дел, это предел; сначала закрой какие-нибудь.")
	execTombstoned  = locale.T("The same loose end was closed before, so it was not tracked again.", "Это уже было закрыто раньше, поэтому снова не добавлено.")
	execDone        = locale.T("Marked %s done.", "Отмечено выполненным: %s.")
	execDropped     = locale.T("Dropped %s.", "Больше не отслеживается: %s.")
	execGone        = locale.T("That loose end no longer exists.", "Такого больше нет.")
	execNotOpen     = locale.T("That loose end is no longer open; nothing was changed.", "Это уже закрыто; ничего не изменено.")
	execUnavailable = locale.T("That action is no longer available.", "Это действие больше недоступно.")

	untrackedOutcome  = locale.T("Stopped tracking it.", "Больше не отслеживается.")
	reopenedOutcome   = locale.T("Reopened it.", "Снова отслеживается.")
	unreadableRefusal = locale.T("what to undo could not be read.", "не удалось прочитать, что отменять.")
	goneRefusal       = locale.T("the loose end no longer exists.", "этого больше нет.")
	changedRefusal    = locale.T("it was changed since; change it with a new request instead.", "это уже изменили; измени это новым запросом.")
	notUndoable       = locale.T("this action cannot be undone.", "это действие нельзя отменить.")
)

// Actor is the approval service as the tools use it: it acts at once in a clean chat turn and
// proposes otherwise.
type Actor interface {
	Act(ctx context.Context, principalID string, proposal approval.Proposal) (string, error)
}

// Tools offers loose_end_track, loose_ends_list, and loose_end_close while the principal has Loose
// ends on, and executes and undoes what the first and the last did or proposed.
type Tools struct {
	service *Service
	actor   Actor
	logger  *slog.Logger
}

var (
	_ tools.Provider       = (*Tools)(nil)
	_ integration.Switched = (*Tools)(nil)
	_ approval.Executor    = (*Tools)(nil)
	_ approval.Undoer      = (*Tools)(nil)
)

func NewTools(service *Service, actor Actor, logger *slog.Logger) (*Tools, error) {
	if service == nil || actor == nil {
		return nil, errors.New("the loose end tools need the service and an actor")
	}
	if logger == nil {
		return nil, errors.New("the loose end tools need a logger")
	}
	return &Tools{service: service, actor: actor, logger: logger}, nil
}

// Switch is the Loose ends switch.
func (t *Tools) Switch() integration.Switch { return Switch }

func (t *Tools) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{
		Guidance: toolsGuidance, WriteGuidance: toolsWriteGuidance,
		Definitions: []tools.Definition{listDefinition, trackDefinition, closeDefinition},
	}, nil
}

func (t *Tools) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case ToolTrack:
		return t.track(ctx, principalID, call)
	case ToolList:
		return t.list(ctx, principalID, call)
	case ToolClose:
		return t.close(ctx, principalID, call)
	default:
		return errorResult("unknown loose end tool"), nil
	}
}

// trackArguments are loose_end_track's arguments, and, cleaned, what a track proposes.
type trackArguments struct {
	Kind        Kind   `json:"kind"`
	Title       string `json:"title"`
	Counterpart string `json:"counterpart"`
	Due         string `json:"due"`
}

func (a trackArguments) draft(outside bool) Draft {
	return Draft{Kind: a.Kind, Title: a.Title, Counterpart: a.Counterpart, Due: a.Due, Source: SourceChat, Outside: outside}
}

type closeArguments struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// itemUndo reverses a direct track or close while the item is as the action left it.
type itemUndo struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// track validates the loose end before it is acted on, so a refusal reaches the model at once, and
// proposes it cleaned, with its due date as the service reads it.
func (t *Tools) track(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments trackArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	item, reason := validate(arguments.draft(false), t.service.stamp(), t.service.location, t.service.runTime)
	if reason != "" {
		return errorResult(reason + " Nothing was tracked."), nil
	}
	canonical, err := json.Marshal(trackArguments{Kind: item.Kind, Title: item.Title, Counterpart: item.Counterpart, Due: t.dueArgument(item)})
	if err != nil {
		return tools.Result{}, err
	}
	summary := trackSummary.Format(ctx, item.Title, kindLabel(ctx, item.Kind), t.userDue(ctx, item))
	if item.Counterpart != "" {
		summary = trackSummaryCounterpart.Format(ctx, item.Title, kindLabel(ctx, item.Kind), item.Counterpart, t.userDue(ctx, item))
	}
	return t.act(ctx, principalID, call, approval.Proposal{Tool: ToolTrack, Arguments: canonical, Summary: summary})
}

func (t *Tools) close(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments closeArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	summary, verb := doneSummary, "mark done"
	switch arguments.Status {
	case closeDone:
	case closeDrop:
		summary, verb = dropSummary, "stop tracking"
	default:
		return errorResult("status must be done or drop."), nil
	}
	item, found, err := t.service.Get(ctx, principalID, strings.TrimSpace(arguments.ID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return errorResult("No loose end has that id. Use loose_ends_list to see the open ones."), nil
	}
	if item.Status != StatusOpen {
		return errorResult(fmt.Sprintf("That loose end is already %s; nothing was changed.", item.Status)), nil
	}
	canonical, err := json.Marshal(closeArguments{ID: item.ID, Status: arguments.Status})
	if err != nil {
		return tools.Result{}, err
	}
	return t.act(ctx, principalID, call, approval.Proposal{Tool: ToolClose, Arguments: canonical,
		Summary: summary.Format(ctx, item.Title, t.userDue(ctx, item)),
		Intent:  approval.Intent{Verb: verb, TargetKind: "loose end", TargetName: item.Title}})
}

// act makes the change at once in a clean turn and proposes it otherwise.
func (t *Tools) act(ctx context.Context, principalID string, call tools.Call, proposal approval.Proposal) (tools.Result, error) {
	text, err := t.actor.Act(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return errorResult(err.Error()), nil
		}
		t.logger.WarnContext(ctx, "looseend.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return errorResult("the change could not be recorded"), nil
	}
	return tools.Result{Text: text}, nil
}

func (t *Tools) list(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments struct {
		Kind Kind `json:"kind"`
	}
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return errorResult(err.Error()), nil
	}
	if arguments.Kind != "" && !arguments.Kind.Valid() {
		return errorResult("kind must be cancel_window, return_window, renewal, trial_end, expiry, or promise."), nil
	}
	items, err := t.service.List(ctx, principalID, arguments.Kind)
	if err != nil {
		return tools.Result{}, err
	}
	if len(items) == 0 {
		if arguments.Kind != "" {
			return tools.Result{Text: fmt.Sprintf("No open loose ends of the kind %s.", arguments.Kind)}, nil
		}
		return tools.Result{Text: "No open loose ends."}, nil
	}
	lines := []string{"Open loose ends, soonest due first:"}
	for index, item := range items {
		if index == listLimit {
			lines = append(lines, fmt.Sprintf("%d more are not shown; list one kind at a time.", len(items)-listLimit))
			break
		}
		lines = append(lines, "- "+t.brief(item))
	}
	return tools.Result{Text: strings.Join(lines, "\n")}, nil
}

// brief is one item in a line for the model: its id, kind, title, counterpart, due date, next
// nudge, and where it was found.
func (t *Tools) brief(item Item) string {
	text := fmt.Sprintf("[%s] %s: %s", item.ID, item.Kind, item.Title)
	if item.Counterpart != "" {
		text += ", with " + item.Counterpart
	}
	return text + fmt.Sprintf("; due %s; nudged on %s; from %s", t.modelDue(item), item.NudgeOn, item.Source)
}

func (t *Tools) ActionTools() []string { return []string{ToolTrack, ToolClose} }

// ExecuteApproved tracks or closes the loose end as acted on or confirmed. A loose end tracked from
// a card, which only a conversation that read outside content proposes, is outside.
func (t *Tools) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case ToolTrack:
		var confirmed trackArguments
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil {
			return approval.Outcome{}, actionFailed(ctx, "malformed", execUnreadable)
		}
		return t.executeTrack(ctx, record, confirmed)
	case ToolClose:
		var confirmed closeArguments
		if err := json.Unmarshal(record.Arguments, &confirmed); err != nil || confirmed.ID == "" {
			return approval.Outcome{}, actionFailed(ctx, "malformed", execUnreadable)
		}
		return t.executeClose(ctx, record, confirmed)
	default:
		return approval.Outcome{}, actionFailed(ctx, "unavailable", execUnavailable)
	}
}

func (t *Tools) executeTrack(ctx context.Context, record approval.Record, confirmed trackArguments) (approval.Outcome, error) {
	tracked, err := t.service.Track(ctx, record.PrincipalID, confirmed.draft(afterOutsideContent(record)))
	if err != nil {
		return approval.Outcome{}, err
	}
	switch tracked.Outcome {
	case OutcomeCreated:
		item := tracked.Item
		t.logger.InfoContext(ctx, "looseend.tracked", "principal_id", record.PrincipalID, "item_id", item.ID, "kind", item.Kind, "outside", item.Outside)
		undo, err := json.Marshal(itemUndo{ID: item.ID, UpdatedAt: item.UpdatedAt})
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{
			Text:     fmt.Sprintf("Tracking %s as loose end %s, due %s.", item.Title, item.ID, t.modelDue(item)),
			UserText: execTracked.Format(ctx, item.Title, t.userDue(ctx, item)),
			Undo:     undo,
		}, nil
	case OutcomeDuplicate, OutcomeMerged:
		return outcome(ctx, execAlready, tracked.ID), nil
	case OutcomeInvalid:
		return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "It can no longer be tracked: " + tracked.Reason + " Nothing was tracked.", UserText: execInvalid.Format(ctx)}
	case OutcomeOpenCapped:
		return approval.Outcome{}, actionFailed(ctx, "too_many", execTooMany, MaximumOpen)
	default:
		return approval.Outcome{}, actionFailed(ctx, string(tracked.Outcome), execTombstoned)
	}
}

func (t *Tools) executeClose(ctx context.Context, record approval.Record, confirmed closeArguments) (approval.Outcome, error) {
	status, message := StatusDone, execDone
	if confirmed.Status == closeDrop {
		status, message = StatusDropped, execDropped
	}
	item, changed, err := t.service.Close(ctx, record.PrincipalID, confirmed.ID, status)
	if err != nil {
		return approval.Outcome{}, err
	}
	switch {
	case item.ID == "":
		return approval.Outcome{}, actionFailed(ctx, "not_found", execGone)
	case !changed:
		return approval.Outcome{}, actionFailed(ctx, "changed", execNotOpen)
	}
	t.logger.InfoContext(ctx, "looseend.closed", "principal_id", record.PrincipalID, "item_id", item.ID, "status", status)
	undo, err := json.Marshal(itemUndo{ID: item.ID, UpdatedAt: item.UpdatedAt})
	if err != nil {
		return approval.Outcome{}, err
	}
	done := outcome(ctx, message, item.Title)
	done.Undo = undo
	return done, nil
}

// Undo takes back a direct track by deleting the item, and a direct close by reopening it, each
// only while the item is as the action left it.
func (t *Tools) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	var undo itemUndo
	if err := json.Unmarshal(raw, &undo); err != nil || undo.ID == "" {
		return approval.Outcome{}, refusal(ctx, unreadableRefusal)
	}
	var undone bool
	var err error
	message := untrackedOutcome
	switch record.Tool {
	case ToolTrack:
		undone, err = t.service.Delete(ctx, record.PrincipalID, undo.ID, undo.UpdatedAt)
	case ToolClose:
		undone, err = t.service.Reopen(ctx, record.PrincipalID, undo.ID, undo.UpdatedAt)
		message = reopenedOutcome
	default:
		return approval.Outcome{}, refusal(ctx, notUndoable)
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if undone {
		t.logger.InfoContext(ctx, "looseend.undone", "principal_id", record.PrincipalID, "item_id", undo.ID, "tool", record.Tool)
		return outcome(ctx, message), nil
	}
	if _, found, err := t.service.Get(ctx, record.PrincipalID, undo.ID); err != nil {
		return approval.Outcome{}, err
	} else if !found {
		return approval.Outcome{}, refusal(ctx, goneRefusal)
	}
	return approval.Outcome{}, refusal(ctx, changedRefusal)
}

// afterOutsideContent reports whether an action was taken after outside content: a proposal the
// user confirmed on a card, which Act makes only then, or an unattended action, which no message
// of the user's asked for.
func afterOutsideContent(record approval.Record) bool {
	return record.Mode != approval.ModeDirect || strings.HasPrefix(record.RequestID, approval.UnattendedRequest)
}

// dueArgument is the item's due date as a track proposes it: the local date of an all-day item, or
// the instant with its local offset.
func (t *Tools) dueArgument(item Item) string {
	if item.DueAllDay {
		return LocalDate(item.DueAt, t.service.location)
	}
	return item.DueAt.In(t.service.location).Format(time.RFC3339)
}

// modelDue is the item's local due date, with its time unless it is all day, for the model.
func (t *Tools) modelDue(item Item) string {
	local := item.DueAt.In(t.service.location)
	if item.DueAllDay {
		return local.Format("2006-01-02 (Mon)")
	}
	return local.Format("2006-01-02 15:04 (Mon)")
}

// userDue is the item's local due date in the user's language.
func (t *Tools) userDue(ctx context.Context, item Item) string {
	local := item.DueAt.In(t.service.location)
	if item.DueAllDay {
		return locale.Date(locale.From(ctx), local, locale.DayYear)
	}
	return locale.Date(locale.From(ctx), local, locale.DayYearTime)
}

func kindLabel(ctx context.Context, kind Kind) string {
	return kindLabels[kind].In(locale.From(ctx))
}

// actionFailed keeps the English sentence for the record and the model, and the user's language
// for the user.
func actionFailed(ctx context.Context, reason string, message locale.Text, arguments ...any) *approval.ActionError {
	return &approval.ActionError{Reason: reason, Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

func outcome(ctx context.Context, message locale.Text, arguments ...any) approval.Outcome {
	return approval.Outcome{Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

func refusal(ctx context.Context, message locale.Text) *approval.UndoRefusal {
	return &approval.UndoRefusal{Reason: message.FormatIn(locale.English), UserReason: message.Format(ctx)}
}

func errorResult(text string) tools.Result { return tools.Result{Text: text, IsError: true} }
