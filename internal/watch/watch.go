// Package watch checks a user's trains, buses, routes, stations, home devices, and anything else the
// assistant can look at during a daily window and tells them only when something changes. An
// evaluator registered per kind turns one check into a normalized state, the service compares it
// with the last state stored for that day's window, and a change into a triggering state becomes a
// durable alert. Every kind but agent checks in code, without a model; an agent watch runs one
// read-only model turn per check on its owner's own subscription.
package watch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
	"github.com/vladar107/home-ai-assistant/internal/weekday"
)

const (
	KindDeparture   = "departure"
	KindDisruptions = "disruptions"
	KindRoute       = "route"
	KindHome        = "home"
	KindAgent       = "agent"

	ModeTrain = "train"
	ModeBus   = "bus"

	// Departure triggers.
	TriggerCancelled       = "cancelled"
	TriggerPartlyCancelled = "partly_cancelled"
	TriggerDelay           = "delay"
	TriggerPlatform        = "platform"
	// Route triggers: a leg failing any departure trigger, or the arrival target no longer met.
	TriggerLeg     = "leg"
	TriggerArrival = "arrival"
	// Disruption triggers.
	TriggerNew     = "new"
	TriggerChanged = "changed"
	// Home triggers: the watched condition starting to hold, and ending after an alert.
	TriggerEnter = "enter"
	TriggerClear = "clear"
	// Agent trigger: the reported value changing while the condition keeps holding.
	TriggerChange = "change"

	MaximumWatchesPerPrincipal = 20
	// MaximumAgentWatchesPerPrincipal is how many of a principal's watches may be agent watches,
	// each of which costs a model turn per check.
	MaximumAgentWatchesPerPrincipal = 5
	// MaximumMessages caps what one window of one transit watch may send, alerts and notices
	// together; a home or agent watch may send MaximumHomeMessages.
	MaximumMessages     = 5
	MaximumHomeMessages = 10
	// FailureNoticeAfter is how many checks in a row must fail before the owner is told, once.
	FailureNoticeAfter = 3

	DefaultLead         = 90 * time.Minute
	DefaultDelayMinutes = 5
	CheckInterval       = 5 * time.Minute
	FinalCheckInterval  = 2 * time.Minute
	// FinalStretch is the end of the window where checks run at FinalCheckInterval.
	FinalStretch = 30 * time.Minute
	// HomeCheckInterval is how often a home watch is checked, all through its window.
	HomeCheckInterval = time.Minute
	// HomeAlertLifetime is how long a home alert stays deliverable after it is queued, since its
	// window may end at midnight.
	HomeAlertLifetime = 30 * time.Minute
	// AgentPollInterval is how often the service asks an agent watch's evaluator whether a check is
	// due or done; the watch's own interval_minutes sets how often a check actually runs.
	AgentPollInterval = time.Minute
	// DefaultAgentIntervalMinutes is how often an agent watch checks when the user names no pace.
	DefaultAgentIntervalMinutes = 60

	minimumLead          = 10 * time.Minute
	maximumLead          = 4 * time.Hour
	minimumFixedWindow   = 15 * time.Minute
	maximumFixedWindow   = 12 * time.Hour
	maximumDelayMinutes  = 120
	maximumNameRunes     = 80
	maximumLanguageRunes = 40
	maximumStations      = 10
	maximumStates        = 10
	maximumForMinutes    = 240
	maximumInstruction   = 600
	maximumCheckClause   = 300
	minimumAgentInterval = 30
	maximumAgentInterval = 1440
	endOfDay             = "24:00"
	startOfDay           = "00:00"
	dateLayout           = "2006-01-02"
	clockLayout          = "15:04"
	alertTypeAlert       = "alert"
	alertTypeRecovered   = "recovered"
	alertTypeFailure     = "failure"
	statusReady          = "ready"
	statusDelivered      = "delivered"
	statusDropped        = "dropped"
	statusFailed         = "failed"
	defaultCheckDeadline = 30 * time.Second
)

// ErrTooManyWatches reports that the principal already holds MaximumWatchesPerPrincipal watches.
var ErrTooManyWatches = errors.New("the principal already has the maximum number of watches")

// ErrSkip is returned by an evaluator that has nothing to check right now, such as when the owner
// has switched the integration off. The service then skips the check: it counts no failure and
// changes no state.
var ErrSkip = errors.New("the check is skipped")

// kindTriggers lists each kind's triggers in the order they are shown.
var kindTriggers = map[string][]string{
	KindDeparture:   {TriggerCancelled, TriggerPartlyCancelled, TriggerDelay, TriggerPlatform},
	KindRoute:       {TriggerLeg, TriggerArrival},
	KindDisruptions: {TriggerNew, TriggerChanged},
	KindHome:        {TriggerEnter, TriggerClear},
	KindAgent:       {TriggerEnter, TriggerChange, TriggerClear},
}

var triggerDescriptions = map[string]string{
	TriggerCancelled:       "a cancellation",
	TriggerPartlyCancelled: "a partial cancellation",
	TriggerDelay:           "a delay of %d minutes or more",
	TriggerPlatform:        "a platform change",
	TriggerLeg:             "a leg cancelled, %d or more minutes late, or moved to another platform",
	TriggerArrival:         "the arrival target no longer being met",
	TriggerNew:             "a new disruption or engineering work",
	TriggerChanged:         "a change to one already known",
	TriggerEnter:           "the condition starting to hold",
	TriggerClear:           "the condition ending",
	TriggerChange:          "the value changing while it holds",
}

// Target is what a watch looks at. A departure watch is a train (From, To, optional Via) or a bus
// (Stop, Line); a disruption watch has Stations; a route watch names a saved route; a home watch
// has the Entity ID of a Home Assistant entity and, as resolved, its friendly Name; an agent watch
// has only a short Name the model gives it.
type Target struct {
	Mode     string   `json:"mode,omitempty"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Via      string   `json:"via,omitempty"`
	Stop     string   `json:"stop,omitempty"`
	Line     string   `json:"line,omitempty"`
	Stations []string `json:"stations,omitempty"`
	Route    string   `json:"route,omitempty"`
	Entity   string   `json:"entity,omitempty"`
	Name     string   `json:"name,omitempty"`
}

// Condition is what a home watch waits for: the entity being in one of State, or its numeric value
// being Above or Below a threshold. Exactly one of the three is set. ForMinutes, when not zero, is
// how long it must have held before it counts.
type Condition struct {
	State      []string `json:"state,omitempty"`
	Above      *float64 `json:"above,omitempty"`
	Below      *float64 `json:"below,omitempty"`
	ForMinutes int      `json:"for_minutes,omitempty"`
}

func (c Condition) numeric() bool { return c.Above != nil || c.Below != nil }

// Describe is the condition as the user reads it, such as "open for 10 min" or "above -15".
func (c Condition) Describe() string {
	var text string
	switch {
	case len(c.State) > 0:
		text = strings.Join(c.State, " or ")
	case c.Above != nil:
		text = "above " + strconv.FormatFloat(*c.Above, 'f', -1, 64)
	case c.Below != nil:
		text = "below " + strconv.FormatFloat(*c.Below, 'f', -1, 64)
	}
	if c.ForMinutes > 0 {
		text += fmt.Sprintf(" for %d min", c.ForMinutes)
	}
	return text
}

// Check is what an agent watch asks a model turn to find out: the Instruction says what to look at
// and how, and the Condition when the user wants to hear about it. A check runs every
// IntervalMinutes, and the watch is deleted after Until, a local date, when one is set.
type Check struct {
	Instruction     string `json:"instruction"`
	Condition       string `json:"condition"`
	IntervalMinutes int    `json:"interval_minutes"`
	Until           string `json:"until,omitempty"`
}

func (c Check) interval() time.Duration { return time.Duration(c.IntervalMinutes) * time.Minute }

// Spec is a watch as the user confirms it. Start and End bound its daily window as local HH:MM
// times; for a departure or a route End is the planned departure, and a home or agent watch may end
// at "24:00", the end of the day. A watch runs on Days, or once on Date and then expires. Language,
// such as Russian, is what its messages are rewritten into before they are sent; empty means the
// English text as is.
type Spec struct {
	Kind         string     `json:"kind"`
	Target       Target     `json:"target"`
	Condition    *Condition `json:"condition,omitempty"`
	Check        *Check     `json:"check,omitempty"`
	Triggers     []string   `json:"triggers"`
	DelayMinutes int        `json:"delay_minutes,omitempty"`
	Days         []string   `json:"days,omitempty"`
	Date         string     `json:"date,omitempty"`
	Start        string     `json:"start"`
	End          string     `json:"end"`
	Zone         string     `json:"zone"`
	Language     string     `json:"language,omitempty"`
}

// Watch is one principal's stored watch. MutedOn is the local date "Mute today" silenced.
type Watch struct {
	ID          string
	PrincipalID string
	Spec
	Enabled bool
	MutedOn string
	// TravelPause keeps the watch from being checked or alerting while its owner is on a trip.
	TravelPause bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Occurrence is one day's window of a watch, as an evaluator receives it.
type Occurrence struct {
	// Date is the window's local date, YYYY-MM-DD.
	Date  string
	Start time.Time
	// End closes the window; for a departure or a route it is the planned departure.
	End time.Time
	// Cache answers identical upstream queries once within one check cycle.
	Cache *Cache
	// Previous is the State of the window's last successful check, empty before the first. A home
	// watch whose window directly follows the previous day's, from 00:00 after a 24:00 end, starts
	// from that day's last State; any other window starts empty.
	Previous string
	// Since is when the watched condition started holding, as the previous check reported it; zero
	// when it was not holding. It is carried across midnight like Previous.
	Since time.Time
	// Checked is when the watch was last checked, successfully or not; zero before its first check.
	// An agent watch's window starts from the previous window's Previous, Since, and Checked
	// whatever the gap, since what it watches changes over days rather than within one.
	Checked time.Time
}

// Evaluator checks the watches of one kind. It must be safe to call from one goroutine at a time
// and should route its upstream queries through Cached with the occurrence's cache.
type Evaluator interface {
	Evaluate(ctx context.Context, principalID string, watch Watch, occurrence Occurrence) (Observation, error)
}

// Resolver is an evaluator that checks a watch's target for its owner before the watch is
// proposed, such as that the owner's integration is on and its names mean one station or stop. It
// returns the spec with the names as the service knows them.
type Resolver interface {
	Resolve(ctx context.Context, principalID string, spec Spec) (Spec, error)
}

// Observation is what one check saw. The service compares State with the previous check's State
// and alerts only when it changes to one where Triggered is set; it sends "back to normal" when it
// later changes to one where it is not, if an alert went out before.
type Observation struct {
	// State is the normalized result, such as "cancelled" or "delay>=5|platform=7b". Identical
	// upstream data must give an identical State, and what should not alert on its own, such as a
	// delay growing by a minute, must be normalized away. It must not be empty.
	State string
	// Triggered reports whether State matches one of the watch's triggers.
	Triggered bool
	// Headline is the alert's first line, such as "The 07:42 to Zwolle is cancelled."; for a state
	// that is not triggered it may say what is normal again.
	Headline string
	// Details are further lines of the alert, such as the reason or an alternative.
	Details []string
	// Silent stores a changed State without any message, for a change the watch's triggers leave
	// out, such as one of several disruptions ending, while what was alerted on still holds.
	Silent bool
	// Since is when the watched condition started holding, zero when it does not hold. The service
	// stores it with every successful check and hands it back as Occurrence.Since.
	Since time.Time
	// Private is set when the check read content that must stay private, such as Notion pages, so
	// a turn that is told about the alert becomes private too.
	Private bool
}

// UpstreamError marks a check that failed because an outside service did, for metrics.
type UpstreamError struct {
	Service string
	Err     error
}

func (e *UpstreamError) Error() string { return e.Service + ": " + e.Err.Error() }
func (e *UpstreamError) Unwrap() error { return e.Err }

// State is what the service remembers about one window of one watch between checks.
type State struct {
	WatchID     string
	PrincipalID string
	Occurrence  string
	Key         string
	// Alerted is set while the last alert sent has not been followed by "back to normal".
	Alerted        bool
	Messages       int
	Failures       int
	FailureNoticed bool
	// Since is Observation.Since of the last successful check.
	Since     time.Time
	CheckedAt time.Time
}

// Alert is one message a watch owes its owner. Kind, Enabled, MutedOn, TravelPause, and Language are
// read from the watch with it.
type Alert struct {
	ID          string
	WatchID     string
	PrincipalID string
	Occurrence  string
	Sequence    int
	Type        string
	Text        string
	ExpiresAt   time.Time
	Kind        string
	Enabled     bool
	MutedOn     string
	TravelPause bool
	Language    string
	// Private is Observation.Private of the check that queued the alert.
	Private bool
	// Label is the watch's Label when the alert is read for delivery.
	Label string
}

// AlertMessage is a delivered alert as it reached its owner: the Telegram message it was sent as,
// the tab it landed in, and its watch and text as they were then.
type AlertMessage struct {
	PrincipalID string
	MessageID   int64
	ThreadID    int64
	WatchID     string
	WatchLabel  string
	Text        string
	// Private is true when the text may retell private content the check read, and External when
	// a model wrote it from what it read outside the household.
	Private  bool
	External bool
	SentAt   time.Time
}

type Repository interface {
	// CreateWatch stores a new watch unless the principal already holds limit watches, which
	// returns ErrTooManyWatches. A watch whose ID exists is left alone and reported as not created.
	CreateWatch(ctx context.Context, watch Watch, limit int) (bool, error)
	ListWatches(ctx context.Context, principalID string) ([]Watch, error)
	LoadWatch(ctx context.Context, principalID, watchID string) (Watch, bool, error)
	// AllWatches returns every principal's watches, for the check loop.
	AllWatches(ctx context.Context) ([]Watch, error)
	// UpdateWatch replaces a watch's definition and travel flag and forgets its window states,
	// keeping its owner, pause, and mute. A watch changed since expectedUpdatedAt is left alone and reported as not updated.
	UpdateWatch(ctx context.Context, watch Watch, expectedUpdatedAt time.Time) (bool, error)
	SetWatchEnabled(ctx context.Context, principalID, watchID string, enabled bool, now time.Time) (bool, error)
	MuteWatch(ctx context.Context, principalID, watchID, date string) (bool, error)
	SetWatchTravelPause(ctx context.Context, principalID, watchID string, on bool) (bool, error)
	DeleteWatch(ctx context.Context, principalID, watchID string) (bool, error)
	// DeleteUnchangedWatch deletes the watch only while it was last changed at updatedAt.
	DeleteUnchangedWatch(ctx context.Context, principalID, watchID string, updatedAt time.Time) (bool, error)
	LoadWatchState(ctx context.Context, watchID, occurrence string) (State, bool, error)
	// LastWatchAlertSequence returns the highest sequence queued for a window, 0 if none, so a
	// window whose state was reset by an edit continues numbering after the alerts it already sent.
	LastWatchAlertSequence(ctx context.Context, watchID, occurrence string) (int, error)
	// LatestWatchStateBefore returns the watch's newest window state for a date before occurrence.
	LatestWatchStateBefore(ctx context.Context, watchID, occurrence string) (State, bool, error)
	// SaveWatchCheck stores a window's state after a check and, when alert is set, queues it in the
	// same transaction. An alert already queued under the same sequence is not queued again.
	SaveWatchCheck(ctx context.Context, state State, alert *Alert, now time.Time) error
	// ReadyWatchAlerts returns the alerts still to be sent, oldest first, with their watch's fields.
	ReadyWatchAlerts(ctx context.Context) ([]Alert, error)
	// FinishWatchAlert moves a ready alert to delivered, dropped, or failed, forgetting its text.
	FinishWatchAlert(ctx context.Context, alertID, status, reason string, now time.Time) (bool, error)
	// PruneWatchHistory removes window states and finished alerts last changed before the instant.
	PruneWatchHistory(ctx context.Context, before time.Time) error
	// SaveWatchAlertMessage keeps a delivered alert's message, then drops the principal's oldest
	// beyond keep; the job report pruning removes old ones.
	SaveWatchAlertMessage(ctx context.Context, message AlertMessage, keep int) error
	// WatchAlertByMessage returns the principal's alert that was sent as the given message.
	WatchAlertByMessage(ctx context.Context, principalID string, messageID int64) (AlertMessage, bool, error)
}

// Label names a watch the way the user reads it in alerts and lists.
func (s Spec) Label() string {
	target := s.Target
	switch s.Kind {
	case KindDeparture:
		if target.Mode == ModeBus {
			return fmt.Sprintf("%s bus %s from %s", s.End, target.Line, target.Stop)
		}
		label := fmt.Sprintf("%s train %s → %s", s.End, target.From, target.To)
		if target.Via != "" {
			label += " via " + target.Via
		}
		return label
	case KindDisruptions:
		return "Disruptions at " + strings.Join(target.Stations, ", ")
	case KindRoute:
		return fmt.Sprintf("Route %q, leaving %s", target.Route, s.End)
	case KindHome:
		name := target.Name
		if name == "" {
			name = target.Entity
		}
		if s.Condition == nil {
			return name
		}
		return name + ": " + s.Condition.Describe()
	case KindAgent:
		return target.Name
	default:
		return s.Kind
	}
}

// DescribeDays is when a watch runs, such as "Mon–Fri" or "once, Mon 5 Oct".
func (s Spec) DescribeDays() string {
	days := s.describeDays()
	if s.Check != nil && s.Check.Until != "" {
		until, err := time.Parse(dateLayout, s.Check.Until)
		if err != nil {
			return days + ", until " + s.Check.Until
		}
		return days + ", until " + until.Format("Mon 2 Jan")
	}
	return days
}

func (s Spec) describeDays() string {
	if s.Date != "" {
		day, err := time.Parse(dateLayout, s.Date)
		if err != nil {
			return "once, " + s.Date
		}
		return "once, " + day.Format("Mon 2 Jan")
	}
	return weekday.Describe(s.Days)
}

// DescribeTriggers lists what the watch alerts on.
func (s Spec) DescribeTriggers() string {
	described := make([]string, 0, len(s.Triggers))
	for _, trigger := range s.Triggers {
		text := triggerDescriptions[trigger]
		if strings.Contains(text, "%d") {
			text = fmt.Sprintf(text, s.DelayMinutes)
		}
		described = append(described, text)
	}
	return strings.Join(described, ", ")
}

// DescribeWindow is the daily window, such as "07:00–09:30", or "all day" for a whole-day one.
func (s Spec) DescribeWindow() string {
	if s.Start == startOfDay && s.End == endOfDay {
		return "all day"
	}
	return s.Start + "–" + s.End
}

// checkEvery is how often the watch's kind is checked, as the user reads it.
func (s Spec) checkEvery() string {
	switch s.Kind {
	case KindHome:
		return "checked every minute"
	case KindAgent:
		if s.Check != nil {
			return fmt.Sprintf("checked every %d minutes, each check a model turn on your own subscription", s.Check.IntervalMinutes)
		}
	}
	return fmt.Sprintf("checked every %d minutes and every %d in the last %d", int(CheckInterval.Minutes()), int(FinalCheckInterval.Minutes()), int(FinalStretch.Minutes()))
}

// Describe is the watch's definition as the user confirms it.
func (s Spec) Describe() string {
	var lines []string
	if s.Kind == KindAgent && s.Check != nil {
		lines = append(lines, "Checks: "+s.Check.Instruction, "Condition: "+s.Check.Condition)
	}
	lines = append(lines,
		"Days: "+s.DescribeDays(),
		fmt.Sprintf("Window: %s (%s), %s", s.DescribeWindow(), s.Zone, s.checkEvery()),
		"Tells you about: "+s.DescribeTriggers(),
	)
	if s.Language != "" {
		lines = append(lines, "Messages in: "+s.Language)
	}
	return strings.Join(lines, "\n")
}

// OccurrenceAt returns the window of the local day that contains now, if the watch runs that day.
func (s Spec) OccurrenceAt(now time.Time) (Occurrence, bool) {
	location, err := localtime.LoadZone(s.Zone)
	if err != nil {
		return Occurrence{}, false
	}
	local := now.In(location)
	date := local.Format(dateLayout)
	if s.Date != "" {
		if s.Date != date {
			return Occurrence{}, false
		}
	} else if !slices.Contains(s.Days, weekday.Name(local.Weekday())) {
		return Occurrence{}, false
	}
	if s.Check != nil && s.Check.Until != "" && date > s.Check.Until {
		return Occurrence{}, false
	}
	start, startErr := clockOn(local, s.Start)
	end, endErr := clockOn(local, s.End)
	if startErr != nil || endErr != nil {
		return Occurrence{}, false
	}
	return Occurrence{Date: date, Start: start, End: end}, true
}

// Expired reports whether a one-off watch's window, or the last day of an agent watch with an
// end date, has ended.
func (s Spec) Expired(now time.Time) bool {
	last := s.Date
	if s.Check != nil && s.Check.Until != "" {
		last = s.Check.Until
	}
	if last == "" {
		return false
	}
	location, err := localtime.LoadZone(s.Zone)
	if err != nil {
		return false
	}
	day, err := time.ParseInLocation(dateLayout, last, location)
	if err != nil {
		return false
	}
	end, err := clockOn(day, s.End)
	return err == nil && now.After(end)
}

// allDay reports whether a kind is watched through whole days by default rather than around a
// departure or within a short fixed window.
func allDay(kind string) bool { return kind == KindHome || kind == KindAgent }

// normalize validates a spec and puts it in its canonical form: defaults filled in, triggers and
// days in their fixed order, and names trimmed. now rejects a one-off window already over.
func normalize(spec Spec, now time.Time) (Spec, error) {
	allowed, known := kindTriggers[spec.Kind]
	if !known {
		return Spec{}, fmt.Errorf("the kind must be departure, disruptions, route, home, or agent")
	}
	target, err := normalizeTarget(spec.Kind, spec.Target)
	if err != nil {
		return Spec{}, err
	}
	spec.Target = target
	if spec.Language, err = normalizeLanguage(spec.Language); err != nil {
		return Spec{}, err
	}
	if spec.Condition, err = normalizeCondition(spec.Kind, spec.Condition); err != nil {
		return Spec{}, err
	}
	if spec.Check, err = normalizeCheck(spec.Kind, spec.Check); err != nil {
		return Spec{}, err
	}

	given := uniqueStrings(spec.Triggers)
	if len(given) == 0 {
		given = allowed
		if spec.Kind == KindAgent || spec.Kind == KindHome && !spec.Condition.numeric() {
			given = []string{TriggerEnter}
		}
	}
	triggers := make([]string, 0, len(allowed))
	for _, trigger := range allowed {
		if slices.Contains(given, trigger) {
			triggers = append(triggers, trigger)
		}
	}
	if len(triggers) != len(given) {
		return Spec{}, fmt.Errorf("a %s watch alerts only on: %s", spec.Kind, strings.Join(allowed, ", "))
	}
	spec.Triggers = triggers
	if slices.Contains(triggers, TriggerDelay) || slices.Contains(triggers, TriggerLeg) {
		if spec.DelayMinutes == 0 {
			spec.DelayMinutes = DefaultDelayMinutes
		}
		if spec.DelayMinutes < 1 || spec.DelayMinutes > maximumDelayMinutes {
			return Spec{}, fmt.Errorf("the delay threshold must be between 1 and %d minutes", maximumDelayMinutes)
		}
	} else {
		spec.DelayMinutes = 0
	}

	location, err := localtime.LoadZone(spec.Zone)
	if err != nil {
		return Spec{}, err
	}
	if allDay(spec.Kind) {
		if spec.Start == "" {
			spec.Start = startOfDay
		}
		if spec.End == "" {
			spec.End = endOfDay
		}
	}
	start, startErr := clockOffset(spec.Start, false)
	end, endErr := clockOffset(spec.End, allDay(spec.Kind))
	if startErr != nil || endErr != nil {
		return Spec{}, fmt.Errorf("times must be local HH:MM, such as 07:42")
	}
	spec.Start, spec.End = formatOffset(start), formatOffset(end)
	length := end - start
	switch spec.Kind {
	case KindDisruptions:
		if length < minimumFixedWindow || length > maximumFixedWindow {
			return Spec{}, fmt.Errorf("the window must run forward within one day, between %d minutes and %d hours long", int(minimumFixedWindow.Minutes()), int(maximumFixedWindow.Hours()))
		}
	case KindHome, KindAgent:
		if length < minimumFixedWindow {
			return Spec{}, fmt.Errorf("the window must run forward within one day, at least %d minutes long, and may end at 24:00", int(minimumFixedWindow.Minutes()))
		}
	default:
		if length < minimumLead || length > maximumLead {
			return Spec{}, fmt.Errorf("checking must start between %d and %d minutes before the departure, on the same day", int(minimumLead.Minutes()), int(maximumLead.Minutes()))
		}
	}

	if allDay(spec.Kind) && len(spec.Days) == 0 && spec.Date == "" {
		spec.Days = weekday.All()
	}
	switch hasDays, hasDate := len(spec.Days) > 0, spec.Date != ""; {
	case hasDays == hasDate:
		return Spec{}, fmt.Errorf("give either the weekdays the watch runs on or one date for a one-off watch")
	case hasDate:
		day, err := time.ParseInLocation(dateLayout, strings.TrimSpace(spec.Date), location)
		if err != nil {
			return Spec{}, fmt.Errorf("the date must be YYYY-MM-DD")
		}
		spec.Date = day.Format(dateLayout)
		if windowEnd, _ := clockOn(day, spec.End); !windowEnd.After(now) {
			return Spec{}, fmt.Errorf("that window is already over")
		}
	default:
		days, err := weekday.Normalize(spec.Days)
		if err != nil {
			return Spec{}, err
		}
		spec.Days = days
	}
	if spec.Check != nil && spec.Check.Until != "" {
		if spec.Date != "" {
			return Spec{}, fmt.Errorf("give either one date for a one-off watch or the last day with until, not both")
		}
		day, err := time.ParseInLocation(dateLayout, spec.Check.Until, location)
		if err != nil {
			return Spec{}, fmt.Errorf("until must be YYYY-MM-DD")
		}
		spec.Check.Until = day.Format(dateLayout)
		if lastEnd, _ := clockOn(day, spec.End); !lastEnd.After(now) {
			return Spec{}, fmt.Errorf("that last day is already over")
		}
	}
	return spec, nil
}

func normalizeTarget(kind string, target Target) (Target, error) {
	var normalized Target
	var err error
	field := func(value, name string, required bool) string {
		if err != nil {
			return ""
		}
		var cleaned string
		cleaned, err = cleanName(value, name, required)
		return cleaned
	}
	switch kind {
	case KindDeparture:
		normalized.Mode = strings.ToLower(strings.TrimSpace(target.Mode))
		switch normalized.Mode {
		case ModeTrain:
			if target.Stop != "" || target.Line != "" {
				return Target{}, fmt.Errorf("a train watch takes from, to, and optionally via, not a stop or line")
			}
			normalized.From = field(target.From, "from", true)
			normalized.To = field(target.To, "to", true)
			normalized.Via = field(target.Via, "via", false)
		case ModeBus:
			if target.From != "" || target.To != "" || target.Via != "" {
				return Target{}, fmt.Errorf("a bus watch takes a stop and a line, not stations")
			}
			normalized.Stop = field(target.Stop, "stop", true)
			normalized.Line = field(target.Line, "line", true)
		default:
			return Target{}, fmt.Errorf("a departure watch's mode must be train or bus")
		}
	case KindDisruptions:
		if len(target.Stations) == 0 || len(target.Stations) > maximumStations {
			return Target{}, fmt.Errorf("a disruption watch takes between 1 and %d stations", maximumStations)
		}
		for _, station := range target.Stations {
			if cleaned := field(station, "station", true); err == nil && !slices.Contains(normalized.Stations, cleaned) {
				normalized.Stations = append(normalized.Stations, cleaned)
			}
		}
	case KindRoute:
		normalized.Route = field(target.Route, "route", true)
	case KindHome:
		normalized.Entity = field(target.Entity, "entity", false)
		normalized.Name = field(target.Name, "name", false)
		if err == nil && normalized.Entity == "" && normalized.Name == "" {
			return Target{}, fmt.Errorf("a home watch needs the name of the device or sensor to watch")
		}
	case KindAgent:
		normalized.Name = field(target.Name, "name", true)
	}
	return normalized, err
}

// normalizeCondition checks a home watch's condition and refuses one on any other kind.
func normalizeCondition(kind string, condition *Condition) (*Condition, error) {
	if kind != KindHome {
		if condition != nil {
			return nil, fmt.Errorf("only a home watch takes a condition")
		}
		return nil, nil
	}
	if condition == nil {
		return nil, fmt.Errorf("a home watch needs a condition: state, above, or below")
	}
	normalized := Condition{ForMinutes: condition.ForMinutes}
	set := 0
	if len(condition.State) > 0 {
		set++
	}
	if condition.Above != nil {
		set++
	}
	if condition.Below != nil {
		set++
	}
	if set != 1 {
		return nil, fmt.Errorf("give exactly one of state, above, or below in the condition")
	}
	for _, threshold := range []*float64{condition.Above, condition.Below} {
		if threshold != nil && (math.IsNaN(*threshold) || math.IsInf(*threshold, 0)) {
			return nil, fmt.Errorf("the threshold must be a number")
		}
	}
	if condition.Above != nil {
		above := *condition.Above
		normalized.Above = &above
	}
	if condition.Below != nil {
		below := *condition.Below
		normalized.Below = &below
	}
	if len(condition.State) > maximumStates {
		return nil, fmt.Errorf("the condition takes at most %d states", maximumStates)
	}
	for _, state := range condition.State {
		cleaned, err := cleanName(state, "state", true)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(normalized.State, cleaned) {
			normalized.State = append(normalized.State, cleaned)
		}
	}
	if normalized.ForMinutes < 0 || normalized.ForMinutes > maximumForMinutes {
		return nil, fmt.Errorf("for_minutes must be between 0 and %d", maximumForMinutes)
	}
	return &normalized, nil
}

// normalizeCheck checks an agent watch's check and refuses one on any other kind.
func normalizeCheck(kind string, check *Check) (*Check, error) {
	if kind != KindAgent {
		if check != nil {
			return nil, fmt.Errorf("only an agent watch takes a check")
		}
		return nil, nil
	}
	if check == nil {
		return nil, fmt.Errorf("an agent watch needs a check: an instruction and a condition")
	}
	instruction, err := cleanText(check.Instruction, "the check's instruction", maximumInstruction)
	if err != nil {
		return nil, err
	}
	condition, err := cleanText(check.Condition, "the check's condition", maximumCheckClause)
	if err != nil {
		return nil, err
	}
	normalized := Check{Instruction: instruction, Condition: condition, IntervalMinutes: check.IntervalMinutes, Until: strings.TrimSpace(check.Until)}
	if normalized.IntervalMinutes == 0 {
		normalized.IntervalMinutes = DefaultAgentIntervalMinutes
	}
	if normalized.IntervalMinutes < minimumAgentInterval || normalized.IntervalMinutes > maximumAgentInterval {
		return nil, fmt.Errorf("interval_minutes must be between %d and %d", minimumAgentInterval, maximumAgentInterval)
	}
	return &normalized, nil
}

// cleanText accepts between 1 and limit characters, keeping line breaks but no other control
// characters.
func cleanText(value, name string, limit int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	invalid := func(r rune) bool { return unicode.IsControl(r) && r != '\n' }
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit || strings.IndexFunc(value, invalid) >= 0 {
		return "", fmt.Errorf("%s must be at most %d characters", name, limit)
	}
	return value, nil
}

// cleanName accepts a single line of at most maximumNameRunes characters.
func cleanName(value, name string, required bool) (string, error) {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		if required {
			return "", fmt.Errorf("%s is required", name)
		}
		return "", nil
	}
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumNameRunes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must be a single line of at most %d characters", name, maximumNameRunes)
	}
	return value, nil
}

// normalizeLanguage keeps a language name such as Russian, capitalized; English means none, since
// the messages are written in it.
func normalizeLanguage(value string) (string, error) {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" || strings.EqualFold(value, "english") || strings.EqualFold(value, "en") {
		return "", nil
	}
	valid := func(r rune) bool { return unicode.IsLetter(r) || r == ' ' || r == '-' }
	if utf8.RuneCountInString(value) > maximumLanguageRunes || strings.IndexFunc(value, func(r rune) bool { return !valid(r) }) >= 0 {
		return "", fmt.Errorf("language must be a language's name, such as Russian")
	}
	first, size := utf8.DecodeRuneInString(value)
	return string(unicode.ToUpper(first)) + value[size:], nil
}

func uniqueStrings(values []string) []string {
	var unique []string
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !slices.Contains(unique, value) {
			unique = append(unique, value)
		}
	}
	return unique
}

// clockOn is the local HH:MM time on the day of the given instant, in that instant's zone; 24:00
// is midnight at the end of that day.
func clockOn(day time.Time, clock string) (time.Time, error) {
	if clock == endOfDay {
		return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location()), nil
	}
	parsed, err := time.Parse(clockLayout, clock)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(day.Year(), day.Month(), day.Day(), parsed.Hour(), parsed.Minute(), 0, 0, day.Location()), nil
}

// clockOffset is an HH:MM time as the time since midnight; endOfDay allows 24:00.
func clockOffset(clock string, endOfDayAllowed bool) (time.Duration, error) {
	clock = strings.TrimSpace(clock)
	if endOfDayAllowed && clock == endOfDay {
		return 24 * time.Hour, nil
	}
	parsed, err := time.Parse(clockLayout, clock)
	if err != nil {
		return 0, err
	}
	return time.Duration(parsed.Hour())*time.Hour + time.Duration(parsed.Minute())*time.Minute, nil
}

func formatOffset(offset time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(offset.Hours()), int(offset.Minutes())%60)
}
