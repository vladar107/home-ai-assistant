package trip

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolAdd    = "trip_add"
	toolList   = "trip_list"
	toolUpdate = "trip_update"

	actionCancel = "cancel"
	actionEndNow = "end_now"
	// actionMove moves the trip to the tab of the turn and leaves its content alone.
	actionMove = "move"

	// addTabLine and moveTabLine end a proposal whose trip is to use the tab it was made in.
	addTabLine  = "The trip takes this tab."
	moveTabLine = "The trip moves to this tab."

	// listedTrips keeps a listing well inside the broker's result limit; one trip is read in
	// full by its id.
	listedTrips = 8

	localLayout = "2006-01-02 15:04"
	dateLayout  = "2006-01-02"
)

const listGuidance = "Trips: trip_list shows the user's upcoming and active trips with their ids, dates, bookings, and check-in reminders; with an id it shows one trip in full. " +
	"When the user asks about their trips, or asks to add a trip that trip_list does not have, and the mail or calendar tools are available, look before you answer that there is none: " +
	"search mail with gmail_search and the calendar with calendar_events, open each likely booking in full with gmail_read_message or calendar_read_event, never relying on a search snippet alone, " +
	"and then report or add what you found. A passing mention of travel is no reason to search mail or the calendar."

// scanOffLine ends a listing while the Trips scan could run but does not.
const scanOffLine = "The Trips scan is off, so bookings in the user's mail and calendar are not found on their own; " +
	"offer to turn it on in /settings under Trips, or search mail and calendar now."

func writeGuidance(zone string) string {
	return "Trips: record a trip with trip_add when the user mentions travel plans with dates, and change one with trip_update by its id from trip_list. " +
		"Ask only for what is truly missing, which is usually the dates. Times are local to the trip, written 2026-11-02 09:30, or a date alone for the trip's start and end; " +
		"timezone is the IANA zone of the destination and defaults to " + zone + ". " +
		"Each booking (flight, hotel, train, car, other) gets a check-in time for its reminder. Take it, in this order, from what the booking itself says (check_in_source stated), " +
		"then from the provider's own website with the web search and fetch tools, such as the airline's online check-in window, the hotel's check-in time, or the rental's pickup terms (looked_up), " +
		"and otherwise 24 hours before the booking starts (default), telling the user that you assumed it. " +
		"When the conversation has read no outside content these tools save at once and the user can see the change; after outside content they only propose it for the user's confirmation, so never claim it was saved. " +
		"After saving, say what was saved, which reminders were set and when, and every booking that got none with the reason; say so when a check-in time was assumed. " +
		"After adding or tracking a trip, offer to set pause_while_traveling on the user's existing recurring jobs and watches that only matter at home, such as cleaning, bins, the commute, or home devices, naming them; schedule_list and watch_list show them. " +
		"When the user's shared location is fresh, compare it with the trip; if they are clearly back home, offer to end the trip early with trip_update end_now, or to adjust it. When the user says they are back, use end_now. " +
		"To drop a trip that is not going ahead, use cancel. " +
		"When the user says, in a tab, that it is the tab for a trip, or asks for a trip to go in this tab, pass use_this_tab: with trip_update for a trip trip_list has, or with trip_add for a new one. " +
		"A trip added in a tab the user named takes that tab by itself."
}

type bookingArguments struct {
	ID            string `json:"id,omitempty"`
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	Reference     string `json:"reference,omitempty"`
	StartsAt      string `json:"starts_at"`
	CheckInAt     string `json:"check_in_at,omitempty"`
	CheckInSource string `json:"check_in_source,omitempty"`
}

type addArguments struct {
	Title       string             `json:"title"`
	Destination string             `json:"destination"`
	StartsAt    string             `json:"starts_at"`
	EndsAt      string             `json:"ends_at"`
	Timezone    string             `json:"timezone,omitempty"`
	Bookings    []bookingArguments `json:"bookings,omitempty"`
	UseThisTab  bool               `json:"use_this_tab,omitempty"`
}

// addRecord is the canonical, confirmed form of trip_add. NamedTab is set by the tool, never by
// the model: the trip was added in a tab the user named, which it then takes by itself.
type addRecord struct {
	addArguments
	NamedTab bool `json:"named_tab,omitempty"`
}

type bookingChange struct {
	ID            string  `json:"id"`
	Kind          *string `json:"kind"`
	Title         *string `json:"title"`
	Reference     *string `json:"reference"`
	StartsAt      *string `json:"starts_at"`
	CheckInAt     *string `json:"check_in_at"`
	CheckInSource *string `json:"check_in_source"`
}

type updateArguments struct {
	ID               string             `json:"id"`
	Title            *string            `json:"title"`
	Destination      *string            `json:"destination"`
	StartsAt         *string            `json:"starts_at"`
	EndsAt           *string            `json:"ends_at"`
	Timezone         *string            `json:"timezone"`
	Cancel           bool               `json:"cancel"`
	EndNow           bool               `json:"end_now"`
	AddBookings      []bookingArguments `json:"add_bookings"`
	ChangeBookings   []bookingChange    `json:"change_bookings"`
	RemoveBookingIDs []string           `json:"remove_booking_ids"`
	UseThisTab       bool               `json:"use_this_tab"`
}

// updateRecord is the canonical, confirmed form of trip_update: the trip's full replacement
// content, which may move it to the turn's tab too, or one of the actions that leave its content
// alone.
type updateRecord struct {
	ID string `json:"id"`
	// UpdatedAt is the trip's last update time when the change was proposed, so a confirmation
	// cannot overwrite what changed since.
	UpdatedAt   string             `json:"updated_at,omitempty"`
	Action      string             `json:"action,omitempty"`
	Title       string             `json:"title,omitempty"`
	Destination string             `json:"destination,omitempty"`
	StartsAt    string             `json:"starts_at,omitempty"`
	EndsAt      string             `json:"ends_at,omitempty"`
	Timezone    string             `json:"timezone,omitempty"`
	Bookings    []bookingArguments `json:"bookings,omitempty"`
	UseThisTab  bool               `json:"use_this_tab,omitempty"`
}

const bookingSchema = `{"type":"object","properties":{` +
	`"kind":{"type":"string","enum":["flight","hotel","train","car","other"]},` +
	`"title":{"type":"string","description":"A short title such as Flight to Example City; at most 80 characters, no links."},` +
	`"reference":{"type":"string","description":"The booking reference; at most 40 characters. Leave out when there is none."},` +
	`"starts_at":{"type":"string","description":"When the flight departs, the stay begins, or the pickup is, local to the trip: 2026-11-02 09:30."},` +
	`"check_in_at":{"type":"string","description":"When check-in or pickup opens, local to the trip: 2026-11-01 09:30. Leave out when unknown."},` +
	`"check_in_source":{"type":"string","enum":["stated","looked_up","default"],"description":"Where check_in_at came from; required with it."}},` +
	`"required":["kind","title","starts_at"],"additionalProperties":false}`

var definitions = []tools.Definition{
	{
		Name:        toolList,
		Description: "List the user's upcoming and active trips with ids, dates, destinations, bookings, and which bookings have no check-in reminder; with an id, read that one trip in full.",
		Risk:        tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"A trip's id, to read it in full."}},` +
			`"additionalProperties":false}`),
	},
	{
		Name:        toolAdd,
		Description: "Record a trip the user told you about, with its dates and bookings, and set a check-in reminder for each booking that has a check-in time.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"title":{"type":"string","description":"A short title, at most 80 characters, no links."},` +
			`"destination":{"type":"string","description":"Where the user is going, at most 80 characters, no links."},` +
			`"starts_at":{"type":"string","description":"When the trip starts, local to the trip: 2026-11-02 09:30, or a date alone."},` +
			`"ends_at":{"type":"string","description":"When the trip ends, local to the trip: 2026-11-06 18:00. A date alone means the end of that day."},` +
			`"timezone":{"type":"string","description":"The IANA time zone of the trip, such as Europe/Lisbon; the user's own zone when left out."},` +
			`"bookings":{"type":"array","maxItems":20,"items":` + bookingSchema + `},` +
			`"use_this_tab":{"type":"boolean","description":"The user wants the trip in the tab this conversation is in, such as when they say it is the tab for the trip."}},` +
			`"required":["title","destination","starts_at","ends_at"],"additionalProperties":false}`),
	},
	{
		Name:        toolUpdate,
		Description: "Change one trip by its id: its title, destination, or dates, add, change, or remove bookings, cancel it, end it now when the user is back, or move it to the tab this conversation is in. Give only what changes.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"The trip's id from trip_list."},` +
			`"title":{"type":"string"},` +
			`"destination":{"type":"string"},` +
			`"starts_at":{"type":"string","description":"The new start, local to the trip."},` +
			`"ends_at":{"type":"string","description":"The new end, local to the trip."},` +
			`"timezone":{"type":"string","description":"A new IANA time zone; the times given are read in it."},` +
			`"cancel":{"type":"boolean","description":"The trip is not going ahead; removes its reminders. Give nothing else with it."},` +
			`"end_now":{"type":"boolean","description":"The user is back: end the active trip now. Give nothing else with it."},` +
			`"add_bookings":{"type":"array","items":` + bookingSchema + `},` +
			`"change_bookings":{"type":"array","items":{"type":"object","properties":{` +
			`"id":{"type":"string","description":"The booking's id from trip_list."},` +
			`"kind":{"type":"string","enum":["flight","hotel","train","car","other"]},` +
			`"title":{"type":"string"},"reference":{"type":"string"},"starts_at":{"type":"string"},` +
			`"check_in_at":{"type":"string","description":"A new check-in time, or an empty string to clear it."},` +
			`"check_in_source":{"type":"string","enum":["stated","looked_up","default"]}},` +
			`"required":["id"],"additionalProperties":false}},` +
			`"remove_booking_ids":{"type":"array","items":{"type":"string"}},` +
			`"use_this_tab":{"type":"boolean","description":"The user says the tab this conversation is in is the trip's: its summary, reminders, notices, and news move here. Alone or with edits, not with cancel or end_now."}},` +
			`"required":["id"],"additionalProperties":false}`),
	},
}

// Proposer is the core approval service as the trip tools see it: it acts at once in a clean
// chat turn and proposes for confirmation otherwise.
type Proposer interface {
	Act(context.Context, string, approval.Proposal) (string, error)
}

// ScanState says whether the principal's Trips scan is off although it could run, because Google
// mail or calendar is readable; the application implements it.
type ScanState interface {
	ScanOff(ctx context.Context, principalID string) (bool, error)
}

// ToolsConfig configures the trip tools.
type ToolsConfig struct {
	// Zone is the IANA time zone a trip gets when none is named; the scheduler's default zone.
	Zone string
}

// Tools offers the trip tools to the model.
type Tools struct {
	service  *Service
	proposer Proposer
	scans    ScanState
	zone     string
	logger   *slog.Logger
}

var _ tools.Provider = (*Tools)(nil)
var _ approval.Executor = (*Tools)(nil)

func NewTools(cfg ToolsConfig, service *Service, proposer Proposer, logger *slog.Logger) (*Tools, error) {
	if service == nil {
		return nil, fmt.Errorf("trip tools require the trip service")
	}
	if logger == nil {
		return nil, fmt.Errorf("trip tools require a logger")
	}
	if _, err := schedule.LoadZone(cfg.Zone); err != nil {
		return nil, fmt.Errorf("trip tools zone: %w", err)
	}
	return &Tools{service: service, proposer: proposer, zone: cfg.Zone, logger: logger}, nil
}

// SetScanState lets trip_list say when the Trips scan is off; nil says nothing. Call it before the
// tools are used.
func (t *Tools) SetScanState(scans ScanState) { t.scans = scans }

// Contribute offers trip_list everywhere and the write tools on chat turns when there is an
// approval service.
func (t *Tools) Contribute(ctx context.Context, _ string) (tools.Contribution, error) {
	contribution := tools.Contribution{Guidance: listGuidance, Definitions: definitions[:1]}
	if _, chat := outbox.ChatRequest(tools.TurnFrom(ctx)); chat && t.proposer != nil {
		contribution.WriteGuidance = writeGuidance(t.zone)
		contribution.Definitions = definitions
	}
	return contribution, nil
}

func (t *Tools) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	switch call.Name {
	case toolList:
		return t.executeList(ctx, principalID, call)
	case toolAdd, toolUpdate:
		if _, chat := outbox.ChatRequest(tools.TurnFrom(ctx)); !chat || t.proposer == nil {
			return tools.Result{Text: "trips can be changed in a chat turn only", IsError: true}, nil
		}
		if call.Name == toolAdd {
			return t.executeAdd(ctx, principalID, call)
		}
		return t.executeUpdate(ctx, principalID, call)
	default:
		return tools.Result{Text: "unknown trip tool", IsError: true}, nil
	}
}

func (t *Tools) executeAdd(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments addArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(arguments.Timezone) == "" {
		arguments.Timezone = t.zone
	}
	draft, text := draftFromAdd(arguments)
	if text != "" {
		return tools.Result{Text: text, IsError: true}, nil
	}
	draft.Source = SourceChat
	normalized, text := normalizeDraft(draft, nil, false)
	if text != "" {
		return tools.Result{Text: text, IsError: true}, nil
	}
	record := addRecord{addArguments: addFromDraft(normalized)}
	summary := "Add a trip:\n" + describeDraft(normalized)
	threadID := thread.From(ctx)
	// A trip that has already ended is given no tab, so it cannot use this one.
	ended := !normalized.EndsAt.After(t.service.Now())
	switch {
	case arguments.UseThisTab && ended:
		return tools.Result{Text: "That trip has already ended, so it cannot use a tab; add it without use_this_tab.", IsError: true}, nil
	case arguments.UseThisTab:
		check, err := t.service.CheckTab(ctx, principalID, "", threadID, false)
		if err != nil {
			return tools.Result{}, err
		}
		if check.Refusal != "" {
			return tools.Result{Text: check.Refusal, IsError: true}, nil
		}
		record.UseThisTab, summary = true, summary+"\n"+addTabLine
	case threadID > 0 && !ended:
		check, err := t.service.CheckTab(ctx, principalID, "", threadID, true)
		if err != nil {
			t.logger.WarnContext(ctx, "trip.tab.check_failed", "principal_id", principalID, "error", err)
		}
		if err == nil && check.Refusal == "" {
			record.NamedTab, summary = true, summary+"\n"+addTabLine
		}
	}
	canonical, err := json.Marshal(record)
	if err != nil {
		return tools.Result{}, err
	}
	return t.act(ctx, principalID, approval.Proposal{
		Tool: toolAdd, Arguments: canonical, Summary: summary,
		Intent: approval.Intent{Verb: "add", TargetKind: "trip", TargetName: normalized.Title},
	})
}

func (t *Tools) executeUpdate(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments updateArguments
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	id := strings.TrimSpace(arguments.ID)
	existing, found, err := t.service.Trip(ctx, principalID, id)
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: unknownTripText, IsError: true}, nil
	}
	if existing.Status != StatusSuggested && existing.Status != StatusPlanned {
		return tools.Result{Text: "That trip is " + existing.Status + " and cannot be changed.", IsError: true}, nil
	}
	edits := arguments.Title != nil || arguments.Destination != nil || arguments.StartsAt != nil || arguments.EndsAt != nil ||
		arguments.Timezone != nil || len(arguments.AddBookings) > 0 || len(arguments.ChangeBookings) > 0 || len(arguments.RemoveBookingIDs) > 0
	record := updateRecord{ID: existing.ID, UpdatedAt: existing.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	var summary string
	switch {
	case arguments.Cancel && arguments.EndNow:
		return tools.Result{Text: "Give either cancel or end_now, not both.", IsError: true}, nil
	case (arguments.Cancel || arguments.EndNow) && arguments.UseThisTab:
		return tools.Result{Text: "use_this_tab does not go with cancel or end_now.", IsError: true}, nil
	case (arguments.Cancel || arguments.EndNow) && edits:
		return tools.Result{Text: "cancel and end_now take no other change; make the edits in a separate call.", IsError: true}, nil
	case arguments.Cancel && existing.Status == StatusSuggested:
		record.Action, summary = actionCancel, fmt.Sprintf("Dismiss the suggested trip %q; it will not be offered again.", existing.Title)
	case arguments.Cancel:
		record.Action, summary = actionCancel, fmt.Sprintf("Cancel the trip %q and remove its check-in reminders.", existing.Title)
	case arguments.EndNow && !existing.Active(t.service.Now()):
		return tools.Result{Text: "That trip is not under way, so it cannot be ended now. Cancel it instead if it is not going ahead.", IsError: true}, nil
	case arguments.EndNow:
		record.Action, summary = actionEndNow, fmt.Sprintf("End the trip %q now.", existing.Title)
	case !edits && !arguments.UseThisTab:
		return tools.Result{Text: "Give a change: new fields, bookings to add, change, or remove, cancel, end_now, or use_this_tab.", IsError: true}, nil
	case !edits:
		move, result := t.checkMove(ctx, principalID, existing)
		if !move {
			return result, nil
		}
		record.Action, summary = actionMove, fmt.Sprintf("Move the trip %q to this tab.", existing.Title)
	default:
		if arguments.UseThisTab {
			move, result := t.checkMove(ctx, principalID, existing)
			if !move && result.IsError {
				return result, nil
			}
			record.UseThisTab = move
		}
		draft, text := applyUpdate(existing, arguments)
		if text != "" {
			return tools.Result{Text: text, IsError: true}, nil
		}
		draft.Source = existing.Source
		normalized, text := normalizeDraft(draft, existing.Bookings, true)
		if text != "" {
			return tools.Result{Text: text, IsError: true}, nil
		}
		add := addFromDraft(normalized)
		record.Title, record.Destination, record.StartsAt, record.EndsAt, record.Timezone, record.Bookings = add.Title, add.Destination, add.StartsAt, add.EndsAt, add.Timezone, add.Bookings
		summary = fmt.Sprintf("Change the trip %q to:\n%s", existing.Title, describeDraft(normalized))
		if record.UseThisTab {
			summary += "\n" + moveTabLine
		}
	}
	canonical, err := json.Marshal(record)
	if err != nil {
		return tools.Result{}, err
	}
	verb := "change"
	switch record.Action {
	case actionCancel:
		verb = "cancel"
	case actionMove:
		verb = "move"
	}
	return t.act(ctx, principalID, approval.Proposal{
		Tool: toolUpdate, Arguments: canonical, Summary: summary,
		Intent: approval.Intent{Verb: verb, TargetKind: "trip", TargetName: existing.Title},
	})
}

// checkMove reports whether the trip can move to the turn's tab; otherwise the result says why, or,
// when the tab is already the trip's, that nothing changes.
func (t *Tools) checkMove(ctx context.Context, principalID string, existing Trip) (bool, tools.Result) {
	if text := tabMoveRefusal(existing, t.service.Now()); text != "" {
		return false, tools.Result{Text: text, IsError: true}
	}
	check, err := t.service.CheckTab(ctx, principalID, existing.ID, thread.From(ctx), false)
	switch {
	case err != nil:
		t.logger.WarnContext(ctx, "trip.tab.check_failed", "principal_id", principalID, "trip_id", existing.ID, "error", err)
		return false, tools.Result{Text: "the tab could not be checked", IsError: true}
	case check.Refusal != "":
		return false, tools.Result{Text: check.Refusal, IsError: true}
	case check.Already:
		return false, tools.Result{Text: fmt.Sprintf("The trip %q already uses this tab; nothing changed.", existing.Title)}
	}
	return true, tools.Result{}
}

func (t *Tools) act(ctx context.Context, principalID string, proposal approval.Proposal) (tools.Result, error) {
	text, err := t.proposer.Act(ctx, principalID, proposal)
	if err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}, nil
		}
		t.logger.WarnContext(ctx, "trip.proposal.failed", "principal_id", principalID, "tool", proposal.Tool, "error", err)
		return tools.Result{Text: "the change could not be recorded", IsError: true}, nil
	}
	return tools.Result{Text: text}, nil
}

func (t *Tools) executeList(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments struct {
		ID string `json:"id"`
	}
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	now := t.service.Now()
	if id := strings.TrimSpace(arguments.ID); id != "" {
		trip, found, err := t.service.Trip(ctx, principalID, id)
		if err != nil {
			return tools.Result{}, err
		}
		if !found {
			return tools.Result{Text: unknownTripText, IsError: true}, nil
		}
		if trip.Status == StatusSuggested {
			return tools.Result{Text: "That trip is not tracked yet; the user decides on its card."}, nil
		}
		return tools.Result{Text: renderTrip(trip, now)}, nil
	}
	all, err := t.service.Trips(ctx, principalID)
	if err != nil {
		return tools.Result{}, err
	}
	var trips []Trip
	for _, trip := range all {
		if trip.Status != StatusSuggested {
			trips = append(trips, trip)
		}
	}
	lines := []string{"The user has no upcoming or active trips."}
	if len(trips) > 0 {
		lines = []string{fmt.Sprintf("%d upcoming or active trips, earliest first:", len(trips))}
	}
	for index, trip := range trips {
		if index == listedTrips {
			lines = append(lines, fmt.Sprintf("%d later trips are not shown.", len(trips)-listedTrips))
			break
		}
		lines = append(lines, renderTrip(trip, now))
	}
	// Only a chat turn can offer the switch; a scheduled turn would remind a user who chose it off.
	if _, chat := outbox.ChatRequest(tools.TurnFrom(ctx)); chat && t.scans != nil {
		off, err := t.scans.ScanOff(ctx, principalID)
		if err != nil {
			t.logger.WarnContext(ctx, "trip.scan_state.failed", "principal_id", principalID, "error", err)
		}
		if off && err == nil {
			lines = append(lines, scanOffLine)
		}
	}
	return tools.Result{Text: strings.Join(lines, "\n")}, nil
}

func renderTrip(trip Trip, now time.Time) string {
	location := localtime.SavedZone(trip.Zone)
	state := trip.Status
	if trip.Active(now) {
		state = "active"
	}
	origin := "source: " + trip.Source
	lines := []string{fmt.Sprintf("[%s] %q, %q (%s; %s): %s to %s, %s", trip.ID, trip.Title, trip.Destination, state, origin,
		trip.StartsAt.In(location).Format(localLayout), trip.EndLabel(localLayout, dateLayout), location)}
	for _, booking := range trip.Bookings {
		line := fmt.Sprintf("  - [%s] %s: %q", booking.ID, booking.Kind, booking.Title)
		if booking.Reference != "" {
			line += fmt.Sprintf(" (%q)", booking.Reference)
		}
		line += ", starts " + booking.StartsAt.In(location).Format(localLayout)
		if booking.CheckInAt.IsZero() {
			line += "; no check-in time, no reminder"
		} else {
			line += fmt.Sprintf("; check-in %s (%s)", booking.CheckInAt.In(location).Format(localLayout), sourceText(booking.CheckInSource))
			if reason := reminderGap(trip, booking, now); reason != "" {
				line += "; no reminder: " + reason
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// reminderGap says why a booking with a check-in time has no reminder, as far as the stored
// trip shows; a reminder the scheduler had no room for is not visible here.
func reminderGap(trip Trip, booking Booking, now time.Time) string {
	at := booking.CheckInAt.Truncate(time.Minute)
	switch {
	case trip.Status != StatusPlanned:
		return "the trip is not tracked yet"
	case !at.After(now):
		return unsetText(UnsetPast)
	case !at.Before(trip.EndsAt):
		return unsetText(UnsetAfterEnd)
	case at.Sub(now) > reminderHorizon:
		return unsetText(UnsetFar)
	}
	return ""
}

func sourceText(source string) string {
	switch source {
	case CheckInStated:
		return "stated in the booking"
	case CheckInLookedUp:
		return "looked up on the provider's site"
	case CheckInDefault:
		return "assumed: 24 hours before the booking starts"
	}
	return source
}

func unsetText(reason string) string {
	switch reason {
	case UnsetPast:
		return "check-in is already open"
	case UnsetCap:
		return "the user has reached the limit of scheduled jobs"
	case UnsetFar:
		return "check-in is more than a year away"
	case UnsetAfterEnd:
		return "check-in opens after the trip ends"
	case UnsetInvalid:
		return "the scheduler refused the time"
	}
	return reason
}

// describeDraft renders a draft for a proposal and for the model: the trip and each booking in the
// trip's zone.
func describeDraft(draft Draft) string {
	location := localtime.SavedZone(draft.Zone)
	lines := []string{fmt.Sprintf("%s, %s: %s to %s (%s)", draft.Title, draft.Destination,
		draft.StartsAt.In(location).Format(localLayout), endLabel(draft.StartsAt, draft.EndsAt, location, localLayout, dateLayout), location)}
	for _, booking := range draft.Bookings {
		line := fmt.Sprintf("- %s: %s", booking.Kind, booking.Title)
		if booking.Reference != "" {
			line += " (" + booking.Reference + ")"
		}
		line += ", starts " + booking.StartsAt.In(location).Format(localLayout)
		if !booking.CheckInAt.IsZero() {
			line += fmt.Sprintf(", check-in %s (%s)", booking.CheckInAt.In(location).Format(localLayout), sourceText(booking.CheckInSource))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// describeSaved tells what a write stored, which reminders were set, and which bookings got none.
func describeSaved(verb string, result Result) string {
	trip := result.Trip
	location := localtime.SavedZone(trip.Zone)
	unset := make(map[string]string, len(result.Unset))
	for _, item := range result.Unset {
		unset[item.BookingID] = unsetText(item.Reason)
	}
	parts := []string{fmt.Sprintf("%s the trip [%s] %s, %s: %s to %s (%s).", verb, trip.ID, trip.Title, trip.Destination,
		trip.StartsAt.In(location).Format(localLayout), trip.EndLabel(localLayout, dateLayout), location)}
	var set, missed, none []string
	for _, booking := range trip.Bookings {
		switch {
		case booking.CheckInAt.IsZero():
			none = append(none, booking.Title)
		case unset[booking.ID] != "":
			missed = append(missed, fmt.Sprintf("%s (%s)", booking.Title, unset[booking.ID]))
		case trip.Status == StatusPlanned:
			set = append(set, fmt.Sprintf("%s at %s (%s)", booking.Title, booking.CheckInAt.In(location).Format(localLayout), sourceText(booking.CheckInSource)))
		}
	}
	if len(set) > 0 {
		parts = append(parts, "Reminders set: "+strings.Join(set, "; ")+".")
	}
	if len(missed) > 0 {
		parts = append(parts, "No reminder for: "+strings.Join(missed, "; ")+".")
	}
	if len(none) > 0 {
		parts = append(parts, "No check-in time, so no reminder: "+strings.Join(none, "; ")+".")
	}
	return strings.Join(parts, " ")
}

func (t *Tools) ActionTools() []string {
	return []string{toolAdd, toolUpdate}
}

// ExecuteApproved makes the confirmed change. The content is validated again by the service, and
// the trip can have changed since the proposal.
func (t *Tools) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	malformed := &approval.ActionError{Reason: "malformed", Text: "The confirmed trip change could not be read; nothing was changed."}
	var result Result
	var err error
	var text string
	switch record.Tool {
	case toolAdd:
		var confirmed addRecord
		if tools.DecodeArguments(record.Arguments, &confirmed) != nil {
			return approval.Outcome{}, malformed
		}
		draft, reason := draftFromAdd(confirmed.addArguments)
		if reason != "" {
			return approval.Outcome{}, malformed
		}
		var claim TabClaim
		switch {
		case confirmed.UseThisTab:
			check, err := t.service.CheckTab(ctx, record.PrincipalID, "", record.ThreadID, false)
			if err != nil {
				return approval.Outcome{}, err
			}
			if check.Refusal != "" {
				return approval.Outcome{}, &approval.ActionError{Reason: "rejected", Text: check.Refusal + " Nothing was changed."}
			}
			claim = TabClaim{ThreadID: record.ThreadID}
		case confirmed.NamedTab:
			claim = TabClaim{ThreadID: record.ThreadID, Named: true}
		}
		if result, err = t.service.AddInTab(ctx, record.PrincipalID, draft, claim); err == nil && result.Rejection == "" {
			text = describeSaved("Saved", result)
			switch move := result.Move; {
			case move != nil && move.Refusal == "":
				text += " " + tabUsedText
			case move != nil:
				text += " It does not use this tab: " + move.Refusal
			}
		}
	case toolUpdate:
		var confirmed updateRecord
		if tools.DecodeArguments(record.Arguments, &confirmed) != nil {
			return approval.Outcome{}, malformed
		}
		var expected time.Time
		if confirmed.UpdatedAt != "" {
			var parseErr error
			if expected, parseErr = time.Parse(time.RFC3339Nano, confirmed.UpdatedAt); parseErr != nil {
				return approval.Outcome{}, malformed
			}
		}
		switch confirmed.Action {
		case actionCancel:
			if result, err = t.service.Cancel(ctx, record.PrincipalID, confirmed.ID, expected); err == nil && result.Rejection == "" {
				text = fmt.Sprintf("Cancelled the trip %q and removed its check-in reminders.", result.Trip.Title)
				if result.Trip.Status == StatusDismissed {
					text = fmt.Sprintf("Dismissed the suggested trip %q.", result.Trip.Title)
				}
			}
		case actionEndNow:
			if result, err = t.service.EndNow(ctx, record.PrincipalID, confirmed.ID, expected); err == nil && result.Rejection == "" {
				text = fmt.Sprintf("Ended the trip %q now; paused jobs resume.", result.Trip.Title)
			}
		case actionMove:
			if result, err = t.service.UseTab(ctx, record.PrincipalID, confirmed.ID, record.ThreadID); err == nil && result.Rejection == "" {
				text = movedText(result)
			}
		case "":
			draft, reason := draftFromAdd(addArguments{Title: confirmed.Title, Destination: confirmed.Destination, StartsAt: confirmed.StartsAt,
				EndsAt: confirmed.EndsAt, Timezone: confirmed.Timezone, Bookings: confirmed.Bookings})
			if reason != "" {
				return approval.Outcome{}, malformed
			}
			// The trip moves first, so the update's summary goes to the taken tab and no tab is made
			// for a trip whose tab is gone only to be left behind.
			moved := ""
			if confirmed.UseThisTab {
				if result, err = t.service.UseTab(ctx, record.PrincipalID, confirmed.ID, record.ThreadID); err != nil {
					return approval.Outcome{}, err
				}
				if result.Rejection != "" {
					return approval.Outcome{}, &approval.ActionError{Reason: "rejected", Text: result.Rejection + " Nothing was changed."}
				}
				moved = movedText(result)
			}
			result, err = t.service.Update(ctx, record.PrincipalID, confirmed.ID, expected, draft)
			switch {
			case moved != "" && err != nil:
				t.logger.WarnContext(ctx, "trip.update.failed", "principal_id", record.PrincipalID, "trip_id", confirmed.ID, "error", err)
				return approval.Outcome{}, &approval.ActionError{Reason: "rejected", Text: moved + " The other changes could not be saved."}
			case moved != "" && result.Rejection != "":
				return approval.Outcome{}, &approval.ActionError{Reason: "rejected", Text: moved + " The other changes were not made: " + result.Rejection}
			case err == nil && result.Rejection == "":
				text = describeSaved("Updated", result)
				if moved != "" {
					text += " " + moved
				}
			}
		default:
			return approval.Outcome{}, malformed
		}
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
	switch {
	case err != nil:
		return approval.Outcome{}, err
	case result.Rejection != "":
		return approval.Outcome{}, &approval.ActionError{Reason: "rejected", Text: result.Rejection + " Nothing was changed."}
	}
	return approval.Outcome{Text: text}, nil
}

// tabUsedText tells the model that a trip uses the tab of the turn.
const tabUsedText = "It uses this tab: its summary is pinned here, and its reminders, notices, and news arrive here."

// movedText tells what moving a trip to the turn's tab did.
func movedText(result Result) string {
	if result.Move != nil && result.Move.Already {
		return fmt.Sprintf("The trip %q already uses this tab.", result.Trip.Title)
	}
	text := fmt.Sprintf("Moved the trip %q to this tab: its summary is pinned here, and its reminders, notices, and news arrive here.", result.Trip.Title)
	if result.Move != nil && result.Move.Left {
		text += " The tab it had stays as an ordinary tab with its messages, which the user can delete."
	}
	return text
}

// draftFromAdd reads the local times of the arguments in their zone.
func draftFromAdd(arguments addArguments) (Draft, string) {
	zone := strings.TrimSpace(arguments.Timezone)
	location, err := schedule.LoadZone(zone)
	if err != nil {
		return Draft{}, "The time zone must be an IANA name such as Europe/Lisbon."
	}
	startsAt, err := parseLocal(arguments.StartsAt, location, false)
	if err != nil {
		return Draft{}, "starts_at must be a local time such as 2026-11-02 09:30, or a date."
	}
	endsAt, err := parseLocal(arguments.EndsAt, location, true)
	if err != nil {
		return Draft{}, "ends_at must be a local time such as 2026-11-06 18:00, or a date."
	}
	bookings, text := bookingsFrom(arguments.Bookings, location)
	if text != "" {
		return Draft{}, text
	}
	return Draft{Title: arguments.Title, Destination: arguments.Destination, StartsAt: startsAt, EndsAt: endsAt, Zone: zone, Bookings: bookings}, ""
}

func bookingsFrom(arguments []bookingArguments, location *time.Location) ([]Booking, string) {
	bookings := make([]Booking, 0, len(arguments))
	for _, item := range arguments {
		startsAt, err := parseLocal(item.StartsAt, location, false)
		if err != nil || len(strings.TrimSpace(item.StartsAt)) <= len(dateLayout) {
			return nil, "A booking's starts_at must be a local date and time such as 2026-11-02 09:30."
		}
		booking := Booking{ID: strings.TrimSpace(item.ID), Kind: item.Kind, Title: item.Title, Reference: item.Reference, StartsAt: startsAt, CheckInSource: item.CheckInSource}
		if strings.TrimSpace(item.CheckInAt) != "" {
			booking.CheckInAt, err = parseLocal(item.CheckInAt, location, false)
			if err != nil || len(strings.TrimSpace(item.CheckInAt)) <= len(dateLayout) {
				return nil, "A booking's check_in_at must be a local date and time such as 2026-11-01 09:30."
			}
		}
		bookings = append(bookings, booking)
	}
	return bookings, ""
}

// addFromDraft is the canonical argument form of a validated draft; booking IDs are kept for the
// caller to use or drop.
func addFromDraft(draft Draft) addArguments {
	location := localtime.SavedZone(draft.Zone)
	arguments := addArguments{
		Title: draft.Title, Destination: draft.Destination, Timezone: draft.Zone,
		StartsAt: draft.StartsAt.In(location).Format(localLayout), EndsAt: draft.EndsAt.In(location).Format(localLayout),
	}
	for _, booking := range draft.Bookings {
		item := bookingArguments{ID: booking.ID, Kind: booking.Kind, Title: booking.Title, Reference: booking.Reference,
			StartsAt: booking.StartsAt.In(location).Format(localLayout), CheckInSource: booking.CheckInSource}
		if !booking.CheckInAt.IsZero() {
			item.CheckInAt = booking.CheckInAt.In(location).Format(localLayout)
		}
		arguments.Bookings = append(arguments.Bookings, item)
	}
	return arguments
}

// applyUpdate builds the full replacement draft of the trip from the changes asked for.
func applyUpdate(existing Trip, arguments updateArguments) (Draft, string) {
	draft := Draft{Title: existing.Title, Destination: existing.Destination, StartsAt: existing.StartsAt, EndsAt: existing.EndsAt, Zone: existing.Zone}
	if arguments.Timezone != nil {
		draft.Zone = strings.TrimSpace(*arguments.Timezone)
	}
	location, err := schedule.LoadZone(draft.Zone)
	if err != nil {
		return Draft{}, "The time zone must be an IANA name such as Europe/Lisbon."
	}
	if arguments.Title != nil {
		draft.Title = *arguments.Title
	}
	if arguments.Destination != nil {
		draft.Destination = *arguments.Destination
	}
	if arguments.StartsAt != nil {
		if draft.StartsAt, err = parseLocal(*arguments.StartsAt, location, false); err != nil {
			return Draft{}, "starts_at must be a local time such as 2026-11-02 09:30, or a date."
		}
	}
	if arguments.EndsAt != nil {
		if draft.EndsAt, err = parseLocal(*arguments.EndsAt, location, true); err != nil {
			return Draft{}, "ends_at must be a local time such as 2026-11-06 18:00, or a date."
		}
	}
	removed := make(map[string]bool, len(arguments.RemoveBookingIDs))
	for _, id := range arguments.RemoveBookingIDs {
		removed[strings.TrimSpace(id)] = true
	}
	changes := make(map[string]bookingChange, len(arguments.ChangeBookings))
	for _, change := range arguments.ChangeBookings {
		id := strings.TrimSpace(change.ID)
		if _, repeated := changes[id]; repeated || removed[id] {
			return Draft{}, "A booking id is repeated across the changes."
		}
		changes[id] = change
	}
	known := make(map[string]bool, len(existing.Bookings))
	for _, booking := range existing.Bookings {
		known[booking.ID] = true
		if removed[booking.ID] {
			continue
		}
		if change, changed := changes[booking.ID]; changed {
			var text string
			if booking, text = changeBooking(booking, change, location); text != "" {
				return Draft{}, text
			}
		}
		draft.Bookings = append(draft.Bookings, booking)
	}
	for id := range removed {
		if !known[id] {
			return Draft{}, "A booking id is unknown. Use trip_list to find the ids."
		}
	}
	for id := range changes {
		if !known[id] {
			return Draft{}, "A booking id is unknown. Use trip_list to find the ids."
		}
	}
	added, text := bookingsFrom(arguments.AddBookings, location)
	if text != "" {
		return Draft{}, text
	}
	for _, booking := range added {
		if booking.ID != "" {
			return Draft{}, "A new booking takes no id."
		}
	}
	draft.Bookings = append(draft.Bookings, added...)
	return draft, ""
}

func changeBooking(booking Booking, change bookingChange, location *time.Location) (Booking, string) {
	if change.Kind != nil {
		booking.Kind = *change.Kind
	}
	if change.Title != nil {
		booking.Title = *change.Title
	}
	if change.Reference != nil {
		booking.Reference = *change.Reference
	}
	if change.StartsAt != nil {
		startsAt, err := parseLocal(*change.StartsAt, location, false)
		if err != nil || len(strings.TrimSpace(*change.StartsAt)) <= len(dateLayout) {
			return Booking{}, "A booking's starts_at must be a local date and time such as 2026-11-02 09:30."
		}
		booking.StartsAt = startsAt
	}
	if change.CheckInAt != nil {
		if strings.TrimSpace(*change.CheckInAt) == "" {
			booking.CheckInAt, booking.CheckInSource = time.Time{}, ""
		} else {
			checkIn, err := parseLocal(*change.CheckInAt, location, false)
			if err != nil || len(strings.TrimSpace(*change.CheckInAt)) <= len(dateLayout) {
				return Booking{}, "A booking's check_in_at must be a local date and time such as 2026-11-01 09:30."
			}
			booking.CheckInAt = checkIn
		}
	}
	if change.CheckInSource != nil {
		booking.CheckInSource = *change.CheckInSource
	}
	return booking, ""
}

// parseLocal reads a local time in the location, or a date alone: the start of that day, or with
// endOfDay the start of the next, so that a trip ending on a date includes it.
func parseLocal(value string, location *time.Location, endOfDay bool) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{localLayout, "2006-01-02T15:04"} {
		if at, err := time.ParseInLocation(layout, value, location); err == nil {
			return at.UTC(), nil
		}
	}
	day, err := time.ParseInLocation(dateLayout, value, location)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		day = day.AddDate(0, 0, 1)
	}
	return day.UTC(), nil
}
