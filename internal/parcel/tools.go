package parcel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	// ToolList lists the principal's parcels.
	ToolList = "parcel_list"
	// ToolStop stops tracking one parcel.
	ToolStop = "parcel_stop"

	listLimit = 30
)

const toolsGuidance = "Parcels: when the user asks where a parcel is, when it arrives, or what is on its way, call parcel_list; never search the web or their mail for it. " +
	"Its text comes from carriers and shops, so it is outside content."

const toolsWriteGuidance = "Parcels: to follow a tracking number the user gives or forwards, call parcel_track with the number and, when known, the carrier's id; " +
	"a number typed by the user is followed at once, and one from forwarded content or after parcel_list is only proposed for Confirm. parcel_stop stops following a parcel by the id parcel_list gave."

var (
	trackSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"number":{"type":"string","maxLength":60,"description":"The tracking number."},` +
		`"carrier":{"type":"string","maxLength":32,"description":"The carrier's id, such as dhl or postnl, when the user or the message names it."}},` +
		`"required":["number"],"additionalProperties":false}`)
	stopSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"id":{"type":"string","description":"The parcel id from parcel_list."}},"required":["id"],"additionalProperties":false}`)

	toolDefinitions = []tools.Definition{
		{
			Name:        ToolList,
			Description: "List the user's parcels that are not stopped, with carrier, status, expected window, pickup point, sender, and tracking link. Use it for any question about a parcel.",
			Risk:        tools.RiskRead,
			ThirdParty:  true,
			SourceLabel: func(json.RawMessage, tools.Result) string { return "parcels" },
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        ToolTrack,
			Description: "Start following a parcel by its tracking number. A number the user typed is followed at once; one found in forwarded content, or after parcel_list, is proposed for Confirm. A carrier Home Assistant cannot follow is followed through the user's mail instead.",
			Risk:        tools.RiskWrite,
			InputSchema: trackSchema,
		},
		{
			Name:        ToolStop,
			Description: "Stop following one parcel by the id parcel_list gave. Because that id comes from a parcel_list result, which is outside content, the stop is usually proposed for the user to confirm on a card, so never claim it is done until it is.",
			Risk:        tools.RiskWrite,
			InputSchema: stopSchema,
		},
	}

	trackedOutcome   = locale.T("Parcel %s is followed in Home Assistant.", "Посылка %s отслеживается в Home Assistant.")
	listedOutcome    = locale.T("Home Assistant already follows parcel %s.", "Home Assistant уже отслеживает посылку %s.")
	mailOutcome      = locale.T("Parcel %s is followed through your mail.", "Посылка %s отслеживается через твою почту.")
	postcodeOutcome  = locale.T("Set your postcode in /settings under Parcels first, then ask again.", "Сначала укажи почтовый индекс в /settings, в разделе «Посылки», потом повтори.")
	untrackedOutcome = locale.T("Parcel %s is no longer followed in Home Assistant.", "Посылка %s больше не отслеживается в Home Assistant.")
	stoppedOutcome   = locale.T("Stopped following the parcel.", "Отслеживание посылки остановлено.")
	goneOutcome      = locale.T("That parcel is gone.", "Такой посылки нет.")
	wantedOutcome    = locale.T("The user stopped following or declined this number since, so it was not added.", "Ты уже перестал отслеживать этот номер или отказался от него, поэтому он не добавлен.")
	alreadyOutcome   = locale.T("Parcel %s is already followed.", "Посылка %s уже отслеживается.")
	failedOutcome    = locale.T("The number could not be added to Home Assistant.", "Не удалось добавить номер в Home Assistant.")
	stopSummary      = locale.T("Stop following the parcel %s.", "Прекратить отслеживание посылки %s.")
)

// Actor is the approval service as the tools use it: it acts at once or proposes, and returns the
// record of a proposal it made.
type Actor interface {
	ActRecord(ctx context.Context, principalID string, proposal approval.Proposal) (string, *approval.Record, error)
}

// ToolLoop is the loop as the tools use it.
type ToolLoop interface {
	Stop(ctx context.Context, principalID, parcelID string) (Parcel, bool, error)
	// Poke asks for a read of the principal's sources without waiting for it.
	Poke(principalID string)
	// Release drops the Home Assistant parcel of a number removed from Home Assistant again.
	Release(ctx context.Context, principalID, carrier, number string) error
}

// HomeSource is the Home Assistant side the tools need: the principal's sources, and adding and
// removing a number. nil means Home Assistant has no token, and every number is followed by mail.
type HomeSource interface {
	Sources
	Track(ctx context.Context, principalID, number, carrier string) (AddedNumber, bool, error)
	Untrack(ctx context.Context, principalID, carrier, number string) error
}

// ToolsConfig is what the tools are built from.
type ToolsConfig struct {
	Repository Repository
	Proposals  Proposals
	Actor      Actor
	// Home is nil when Home Assistant is not connected, which must be a nil interface.
	Home    HomeSource
	Tracker ToolLoop
	// Location is the zone expected windows are shown in; nil means UTC.
	Location *time.Location
	Now      func() time.Time
	Logger   *slog.Logger
}

// Tools offers parcel_list, parcel_track, and parcel_stop while the principal has Parcels on, and
// executes what they proposed. Undoing parcel_track removes the number from Home Assistant and
// returns the number to proposed, not declined: the user only reversed an addition, so a later
// message about it may offer it again.
type Tools struct {
	cfg ToolsConfig
}

var (
	_ tools.Provider       = (*Tools)(nil)
	_ integration.Switched = (*Tools)(nil)
	_ approval.Executor    = (*Tools)(nil)
	_ approval.Undoer      = (*Tools)(nil)
	_ approval.Rejecter    = (*Tools)(nil)
)

// NewTools builds the tools.
func NewTools(cfg ToolsConfig) (*Tools, error) {
	switch {
	case cfg.Repository == nil || cfg.Actor == nil || cfg.Tracker == nil:
		return nil, errors.New("the parcel tools need a repository, an actor, and a tracker")
	case cfg.Logger == nil:
		return nil, errors.New("the parcel tools need a logger")
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Tools{cfg: cfg}, nil
}

// Switch is the Parcels switch.
func (t *Tools) Switch() integration.Switch { return Switch }

func (t *Tools) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Guidance: toolsGuidance, WriteGuidance: toolsWriteGuidance, Definitions: toolDefinitions}, nil
}

func (t *Tools) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case ToolList:
		return t.list(ctx, principalID)
	case ToolTrack:
		return t.track(ctx, principalID, call)
	case ToolStop:
		return t.stop(ctx, principalID, call)
	default:
		return tools.Result{Text: "unknown parcel tool", IsError: true}, nil
	}
}

func (t *Tools) list(ctx context.Context, principalID string) (tools.Result, error) {
	parcels, err := t.cfg.Repository.ListParcels(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	// Open parcels come first and the newest of each kind before older ones, so the limit leaves
	// out the oldest finished parcels first.
	slices.SortStableFunc(parcels, func(a, b Parcel) int {
		if isOpen(a) != isOpen(b) {
			if isOpen(a) {
				return -1
			}
			return 1
		}
		return recency(b).Compare(recency(a))
	})
	var lines []string
	for _, held := range parcels {
		if held.Stopped() {
			continue
		}
		lines = append(lines, t.describe(held))
		if len(lines) == listLimit {
			break
		}
	}
	if len(lines) == 0 {
		return tools.Result{Text: "The user has no parcels being followed."}, nil
	}
	return tools.Result{Text: strings.Join(lines, "\n")}, nil
}

// recency orders finished parcels by delivery: a baseline stores a source's whole history at one
// moment, so its creation time says nothing about which arrived last.
func recency(held Parcel) time.Time {
	if !held.DeliveredAt.IsZero() {
		return held.DeliveredAt
	}
	return held.CreatedAt
}

func (t *Tools) describe(held Parcel) string {
	carrier := held.Carrier
	if known, ok := DefaultCarriers().ByID(held.Carrier); ok {
		carrier = known.Name
	}
	fields := []string{"id " + held.ID, carrier, "status " + string(held.Status)}
	if !held.WindowFrom.IsZero() {
		window := held.WindowFrom.In(t.cfg.Location).Format("2006-01-02 15:04")
		if !held.WindowTo.IsZero() {
			window += "-" + held.WindowTo.In(t.cfg.Location).Format("15:04")
		}
		fields = append(fields, "expected "+window)
	}
	if !held.DeliveredAt.IsZero() {
		fields = append(fields, "delivered "+held.DeliveredAt.In(t.cfg.Location).Format("2006-01-02 15:04"))
	}
	if point := oneLine(held.PickupPoint, maximumPickupRunes); point != "" {
		fields = append(fields, "pickup point "+point)
	}
	if title := oneLine(held.Title, maximumTitleRunes); title != "" {
		fields = append(fields, "what "+title)
	}
	if sender := oneLine(held.Sender, maximumSenderRunes); sender != "" {
		fields = append(fields, "from "+sender)
	}
	if held.Link != "" {
		fields = append(fields, held.Link)
	}
	return strings.Join(fields, "; ")
}

func (t *Tools) track(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments TrackArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	number, ok := NormalizeNumber(arguments.Number)
	if !ok {
		return tools.Result{Text: "That is not a tracking number: it must be 8 to 35 letters and digits. Nothing was added.", IsError: true}, nil
	}
	carrier := strings.ToLower(strings.TrimSpace(arguments.Carrier))
	if carrier != "" && !ValidCarrier(carrier) {
		return tools.Result{Text: "The carrier must be a short lower-case word such as dhl or postnl.", IsError: true}, nil
	}
	if carrier == "" {
		if found := t.cfg.Proposals.Carriers.Detect(number); len(found) == 1 {
			carrier = found[0].ID
		}
	}
	return t.propose(ctx, principalID, number, carrier)
}

// proposal is the check before a proposal, which a number the user asked for in their own message
// passes even when they declined it or left it unanswered before.
func (t *Tools) proposal(ctx context.Context, principalID, number, carrier string) (approval.Proposal, error) {
	if run := tools.RunFrom(ctx); run != nil && run.UserMessage() {
		return t.cfg.Proposals.Requested(ctx, principalID, number, carrier)
	}
	return t.cfg.Proposals.Proposal(ctx, principalID, number, carrier)
}

// followByMail follows the number through mail and tells the model what came of it.
func (t *Tools) followByMail(ctx context.Context, principalID, number, carrier string) (tools.Result, error) {
	followed, err := t.cfg.Proposals.FollowMail(ctx, principalID, number, carrier, "", "")
	if err != nil {
		return tools.Result{}, err
	}
	switch followed {
	case FollowedAlready:
		return tools.Result{Text: "This number is already one of the user's parcels. Nothing was added."}, nil
	case FollowedTombstoned:
		return tools.Result{Text: "The user stopped following this number earlier. Nothing was added."}, nil
	}
	t.cfg.Tracker.Poke(principalID)
	return tools.Result{Text: "Home Assistant cannot follow this number for the user, so it is followed through their mail. A card appears in their Parcels tab."}, nil
}

func (t *Tools) propose(ctx context.Context, principalID, number, carrier string) (tools.Result, error) {
	proposal, err := t.proposal(ctx, principalID, number, carrier)
	switch {
	case errors.Is(err, ErrNoIntegration):
		return t.followByMail(ctx, principalID, number, carrier)
	case errors.Is(err, ErrNotProposed):
		return tools.Result{Text: notProposedText(err)}, nil
	case err != nil:
		return tools.Result{}, err
	}
	text, record, err := t.cfg.Actor.ActRecord(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		t.cfg.Logger.WarnContext(ctx, "parcels.tool.proposal_failed", "principal_id", principalID, "error", err)
		return tools.Result{Text: "the parcel could not be recorded", IsError: true}, nil
	}
	if record != nil {
		if err := t.cfg.Proposals.Offered(ctx, principalID, number, carrier, record.ID); err != nil {
			t.cfg.Logger.WarnContext(ctx, "parcels.tool.offer_unrecorded", "principal_id", principalID, "error", err)
		}
	}
	return tools.Result{Text: text}, nil
}

func notProposedText(err error) string {
	switch {
	case errors.Is(err, errAlreadyAdded):
		return "This number is already followed in Home Assistant. Nothing was added."
	case errors.Is(err, errTombstoned):
		return "The user stopped following this number earlier. Nothing was added."
	case errors.Is(err, errPending):
		return "A proposal for this number is already waiting for the user's confirmation."
	case errors.Is(err, errDeclined):
		return "The user declined this number earlier. Nothing was added."
	case errors.Is(err, errUnanswered):
		return "This number was offered several times without an answer. Nothing was added."
	}
	return "This number cannot be followed. Nothing was added."
}

func (t *Tools) stop(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments struct {
		ID string `json:"id"`
	}
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	held, found, err := t.cfg.Repository.LoadParcel(ctx, principalID, strings.TrimSpace(arguments.ID))
	if err != nil {
		return tools.Result{}, err
	}
	if !found || held.Stopped() {
		return tools.Result{Text: "No parcel has that id. Use parcel_list to see the user's parcels.", IsError: true}, nil
	}
	canonical, err := json.Marshal(struct {
		ID string `json:"id"`
	}{held.ID})
	if err != nil {
		return tools.Result{}, err
	}
	label := held.Carrier
	if known, ok := DefaultCarriers().ByID(held.Carrier); ok {
		label = known.Name
	}
	if held.Title != "" {
		label += ", " + clip.Runes(Unlinked(held.Title), maximumTitleRunes)
	}
	text, _, err := t.cfg.Actor.ActRecord(ctx, principalID, approval.Proposal{Tool: ToolStop, Arguments: canonical, Summary: stopSummary.Format(ctx, label)})
	if err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		t.cfg.Logger.WarnContext(ctx, "parcels.tool.stop_failed", "principal_id", principalID, "error", err)
		return tools.Result{Text: "the parcel could not be stopped", IsError: true}, nil
	}
	return tools.Result{Text: text}, nil
}

func (t *Tools) ActionTools() []string { return []string{ToolTrack, ToolStop} }

type undoPayload struct {
	Carrier string `json:"carrier"`
	Number  string `json:"number"`
}

// ExecuteApproved runs a confirmed or direct action.
func (t *Tools) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case ToolTrack:
		return t.executeTrack(ctx, record)
	case ToolStop:
		var arguments struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
			return approval.Outcome{}, actionFailed(ctx, "malformed", goneOutcome)
		}
		_, found, err := t.cfg.Tracker.Stop(ctx, record.PrincipalID, arguments.ID)
		if err != nil {
			return approval.Outcome{}, err
		}
		if !found {
			return approval.Outcome{}, actionFailed(ctx, "not_found", goneOutcome)
		}
		return approval.Outcome{Text: stoppedOutcome.FormatIn(locale.English), UserText: stoppedOutcome.Format(ctx)}, nil
	}
	return approval.Outcome{}, actionFailed(ctx, "unavailable", failedOutcome)
}

func (t *Tools) executeTrack(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	var arguments TrackArguments
	if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
		return approval.Outcome{}, actionFailed(ctx, "malformed", failedOutcome)
	}
	number, ok := NormalizeNumber(arguments.Number)
	if !ok {
		return approval.Outcome{}, actionFailed(ctx, "invalid", failedOutcome)
	}
	principalID := record.PrincipalID
	if err := t.cfg.Proposals.StillWanted(ctx, principalID, number, arguments.Carrier); errors.Is(err, ErrNotProposed) {
		return approval.Outcome{}, actionFailed(ctx, "not_wanted", wantedOutcome)
	} else if err != nil {
		return approval.Outcome{}, err
	}
	done := func(message locale.Text, undo json.RawMessage) approval.Outcome {
		return approval.Outcome{Text: message.FormatIn(locale.English, number), UserText: message.Format(ctx, number), Undo: undo}
	}
	var added AddedNumber
	var fresh bool
	var err error
	if t.cfg.Home == nil {
		err = ErrFollowedByMail
	} else {
		added, fresh, err = t.cfg.Home.Track(ctx, principalID, number, arguments.Carrier)
	}
	switch {
	case errors.Is(err, ErrFollowedByMail):
		followed, err := t.cfg.Proposals.FollowMail(ctx, principalID, number, arguments.Carrier, "", "")
		switch {
		case err != nil:
			return approval.Outcome{}, err
		case followed == FollowedTombstoned:
			return approval.Outcome{}, actionFailed(ctx, "not_wanted", wantedOutcome)
		case followed == FollowedAlready:
			return done(alreadyOutcome, nil), nil
		}
		t.cfg.Tracker.Poke(principalID)
		return done(mailOutcome, nil), nil
	case errors.Is(err, ErrPostcodeMissing):
		return approval.Outcome{}, actionFailed(ctx, "postcode", postcodeOutcome)
	case errors.Is(err, ErrInvalidNumber):
		return approval.Outcome{}, actionFailed(ctx, "invalid", failedOutcome)
	case err != nil:
		return approval.Outcome{}, err
	}
	if err := t.cfg.Repository.SetProposalOutcome(ctx, principalID, number, OutcomeAdded, t.cfg.Now()); err != nil {
		t.cfg.Logger.WarnContext(ctx, "parcels.tool.outcome_unrecorded", "principal_id", principalID, "error", err)
	}
	t.cfg.Tracker.Poke(principalID)
	if !fresh {
		return done(listedOutcome, nil), nil
	}
	undo, err := json.Marshal(undoPayload{Carrier: added.Carrier, Number: number})
	if err != nil {
		return approval.Outcome{}, err
	}
	return done(trackedOutcome, undo), nil
}

// Undo removes the number a direct parcel_track added and sets it back to proposed.
func (t *Tools) Undo(ctx context.Context, record approval.Record, undo json.RawMessage) (approval.Outcome, error) {
	var payload undoPayload
	if err := json.Unmarshal(undo, &payload); err != nil || t.cfg.Home == nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the undo record is unreadable", UserReason: cannotUndo.Format(ctx)}
	}
	if err := t.cfg.Home.Untrack(ctx, record.PrincipalID, payload.Carrier, payload.Number); err != nil {
		return approval.Outcome{}, err
	}
	if err := t.cfg.Repository.SetProposalOutcome(ctx, record.PrincipalID, payload.Number, OutcomeProposed, t.cfg.Now()); err != nil {
		t.cfg.Logger.WarnContext(ctx, "parcels.tool.outcome_unrecorded", "principal_id", record.PrincipalID, "error", err)
	}
	if err := t.cfg.Tracker.Release(ctx, record.PrincipalID, payload.Carrier, payload.Number); err != nil {
		t.cfg.Logger.WarnContext(ctx, "parcels.tool.release_failed", "principal_id", record.PrincipalID, "error", errorKind(err))
	}
	return approval.Outcome{Text: untrackedOutcome.FormatIn(locale.English, payload.Number), UserText: untrackedOutcome.Format(ctx, payload.Number)}, nil
}

var cannotUndo = locale.T("This cannot be undone.", "Это нельзя отменить.")

// Rejected records that the user declined the proposed number.
func (t *Tools) Rejected(ctx context.Context, record approval.Record) error {
	if record.Tool != ToolTrack {
		return nil
	}
	var arguments TrackArguments
	if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
		return err
	}
	number, ok := NormalizeNumber(arguments.Number)
	if !ok {
		return nil
	}
	return t.cfg.Repository.SetProposalOutcome(ctx, record.PrincipalID, number, OutcomeDeclined, t.cfg.Now())
}

func actionFailed(ctx context.Context, reason string, message locale.Text) *approval.ActionError {
	return &approval.ActionError{Reason: reason, Text: message.FormatIn(locale.English), UserText: message.Format(ctx)}
}
