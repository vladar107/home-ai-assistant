// Package watch checks a user's trains, buses, routes, and stations during a daily window and
// tells them only when something changes. Checks are code: an evaluator registered per kind turns
// one check into a normalized state, the service compares it with the last state stored for that
// day's window, and a change into a triggering state becomes a durable alert. No model runs per
// check; the model only proposes, changes, and explains watches.
package watch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	KindDeparture   = "departure"
	KindDisruptions = "disruptions"
	KindRoute       = "route"

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

	MaximumWatchesPerPrincipal = 20
	// MaximumMessages caps what one window of one watch may send, alerts and notices together.
	MaximumMessages = 5
	// FailureNoticeAfter is how many checks in a row must fail before the owner is told, once.
	FailureNoticeAfter = 3

	DefaultLead         = 90 * time.Minute
	DefaultDelayMinutes = 5
	CheckInterval       = 5 * time.Minute
	FinalCheckInterval  = 2 * time.Minute
	// FinalStretch is the end of the window where checks run at FinalCheckInterval.
	FinalStretch = 30 * time.Minute

	minimumLead          = 10 * time.Minute
	maximumLead          = 4 * time.Hour
	minimumFixedWindow   = 15 * time.Minute
	maximumFixedWindow   = 12 * time.Hour
	maximumDelayMinutes  = 120
	maximumNameRunes     = 80
	maximumStations      = 10
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

// kindTriggers lists each kind's triggers in the order they are shown.
var kindTriggers = map[string][]string{
	KindDeparture:   {TriggerCancelled, TriggerPartlyCancelled, TriggerDelay, TriggerPlatform},
	KindRoute:       {TriggerLeg, TriggerArrival},
	KindDisruptions: {TriggerNew, TriggerChanged},
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
}

// weekdays are in the order they are shown, Monday first.
var weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func weekdayName(day time.Weekday) string { return weekdays[(int(day)+6)%7] }

// Target is what a watch looks at. A departure watch is a train (From, To, optional Via) or a bus
// (Stop, Line); a disruption watch has Stations; a route watch names a saved route.
type Target struct {
	Mode     string   `json:"mode,omitempty"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Via      string   `json:"via,omitempty"`
	Stop     string   `json:"stop,omitempty"`
	Line     string   `json:"line,omitempty"`
	Stations []string `json:"stations,omitempty"`
	Route    string   `json:"route,omitempty"`
}

// Spec is a watch as the user confirms it. Start and End bound its daily window as local HH:MM
// times; for a departure or a route End is the planned departure. A watch runs on Days, or once on
// Date and then expires.
type Spec struct {
	Kind         string   `json:"kind"`
	Target       Target   `json:"target"`
	Triggers     []string `json:"triggers"`
	DelayMinutes int      `json:"delay_minutes,omitempty"`
	Days         []string `json:"days,omitempty"`
	Date         string   `json:"date,omitempty"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Zone         string   `json:"zone"`
}

// Watch is one principal's stored watch. MutedOn is the local date "Mute today" silenced.
type Watch struct {
	ID          string
	PrincipalID string
	Spec
	Enabled   bool
	MutedOn   string
	CreatedAt time.Time
	UpdatedAt time.Time
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
	// Previous is the State of the window's last successful check, empty before the first.
	Previous string
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
	CheckedAt      time.Time
}

// Alert is one message a watch owes its owner. Kind, Label, Enabled, and MutedOn are read from
// the watch with it.
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
}

type Repository interface {
	// CreateWatch stores a new watch unless the principal already holds limit watches, which
	// returns ErrTooManyWatches. A watch whose ID exists is left alone and reported as not created.
	CreateWatch(ctx context.Context, watch Watch, limit int) (bool, error)
	ListWatches(ctx context.Context, principalID string) ([]Watch, error)
	LoadWatch(ctx context.Context, principalID, watchID string) (Watch, bool, error)
	// AllWatches returns every principal's watches, for the check loop.
	AllWatches(ctx context.Context) ([]Watch, error)
	// UpdateWatch replaces a watch's definition and forgets its window states, keeping its owner,
	// pause, and mute. A watch changed since expectedUpdatedAt is left alone and reported as not updated.
	UpdateWatch(ctx context.Context, watch Watch, expectedUpdatedAt time.Time) (bool, error)
	SetWatchEnabled(ctx context.Context, principalID, watchID string, enabled bool, now time.Time) (bool, error)
	MuteWatch(ctx context.Context, principalID, watchID, date string) (bool, error)
	DeleteWatch(ctx context.Context, principalID, watchID string) (bool, error)
	LoadWatchState(ctx context.Context, watchID, occurrence string) (State, bool, error)
	// SaveWatchCheck stores a window's state after a check and, when alert is set, queues it in the
	// same transaction. An alert already queued under the same sequence is not queued again.
	SaveWatchCheck(ctx context.Context, state State, alert *Alert, now time.Time) error
	// ReadyWatchAlerts returns the alerts still to be sent, oldest first, with their watch's fields.
	ReadyWatchAlerts(ctx context.Context) ([]Alert, error)
	// FinishWatchAlert moves a ready alert to delivered, dropped, or failed, forgetting its text.
	FinishWatchAlert(ctx context.Context, alertID, status, reason string, now time.Time) (bool, error)
	// PruneWatchHistory removes window states and finished alerts last changed before the instant.
	PruneWatchHistory(ctx context.Context, before time.Time) error
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
	default:
		return s.Kind
	}
}

// DescribeDays is when a watch runs, such as "weekdays" or "once, Mon 5 Oct".
func (s Spec) DescribeDays() string {
	if s.Date != "" {
		day, err := time.Parse(dateLayout, s.Date)
		if err != nil {
			return "once, " + s.Date
		}
		return "once, " + day.Format("Mon 2 Jan")
	}
	switch strings.Join(s.Days, ",") {
	case "mon,tue,wed,thu,fri":
		return "weekdays"
	case "mon,tue,wed,thu,fri,sat,sun":
		return "every day"
	case "sat,sun":
		return "weekends"
	}
	names := make([]string, 0, len(s.Days))
	for _, day := range s.Days {
		names = append(names, strings.ToUpper(day[:1])+day[1:])
	}
	return strings.Join(names, ", ")
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

// Describe is the watch's definition as the user confirms it.
func (s Spec) Describe() string {
	lines := []string{
		"Days: " + s.DescribeDays(),
		fmt.Sprintf("Window: %s–%s (%s), checked every %d minutes and every %d in the last %d",
			s.Start, s.End, s.Zone, int(CheckInterval.Minutes()), int(FinalCheckInterval.Minutes()), int(FinalStretch.Minutes())),
		"Tells you about: " + s.DescribeTriggers(),
	}
	return strings.Join(lines, "\n")
}

// OccurrenceAt returns the window of the local day that contains now, if the watch runs that day.
func (s Spec) OccurrenceAt(now time.Time) (Occurrence, bool) {
	location, err := loadZone(s.Zone)
	if err != nil {
		return Occurrence{}, false
	}
	local := now.In(location)
	date := local.Format(dateLayout)
	if s.Date != "" {
		if s.Date != date {
			return Occurrence{}, false
		}
	} else if !slices.Contains(s.Days, weekdayName(local.Weekday())) {
		return Occurrence{}, false
	}
	start, startErr := clockOn(local, s.Start)
	end, endErr := clockOn(local, s.End)
	if startErr != nil || endErr != nil {
		return Occurrence{}, false
	}
	return Occurrence{Date: date, Start: start, End: end}, true
}

// Expired reports whether a one-off watch's window has ended.
func (s Spec) Expired(now time.Time) bool {
	if s.Date == "" {
		return false
	}
	location, err := loadZone(s.Zone)
	if err != nil {
		return false
	}
	day, err := time.ParseInLocation(dateLayout, s.Date, location)
	if err != nil {
		return false
	}
	end, err := clockOn(day, s.End)
	return err == nil && now.After(end)
}

// normalize validates a spec and puts it in its canonical form: defaults filled in, triggers and
// days in their fixed order, and names trimmed. now rejects a one-off window already over.
func normalize(spec Spec, now time.Time) (Spec, error) {
	allowed, known := kindTriggers[spec.Kind]
	if !known {
		return Spec{}, fmt.Errorf("the kind must be departure, disruptions, or route")
	}
	target, err := normalizeTarget(spec.Kind, spec.Target)
	if err != nil {
		return Spec{}, err
	}
	spec.Target = target

	given := uniqueStrings(spec.Triggers)
	if len(given) == 0 {
		given = allowed
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

	location, err := loadZone(spec.Zone)
	if err != nil {
		return Spec{}, err
	}
	start, startErr := time.Parse(clockLayout, spec.Start)
	end, endErr := time.Parse(clockLayout, spec.End)
	if startErr != nil || endErr != nil {
		return Spec{}, fmt.Errorf("times must be local HH:MM, such as 07:42")
	}
	spec.Start, spec.End = start.Format(clockLayout), end.Format(clockLayout)
	length := end.Sub(start)
	if spec.Kind == KindDisruptions {
		if length < minimumFixedWindow || length > maximumFixedWindow {
			return Spec{}, fmt.Errorf("the window must run forward within one day, between %d minutes and %d hours long", int(minimumFixedWindow.Minutes()), int(maximumFixedWindow.Hours()))
		}
	} else if length < minimumLead || length > maximumLead {
		return Spec{}, fmt.Errorf("checking must start between %d and %d minutes before the departure, on the same day", int(minimumLead.Minutes()), int(maximumLead.Minutes()))
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
		given := uniqueStrings(spec.Days)
		days := make([]string, 0, len(weekdays))
		for _, day := range weekdays {
			if slices.Contains(given, day) {
				days = append(days, day)
			}
		}
		if len(days) != len(given) {
			return Spec{}, fmt.Errorf("days must be mon, tue, wed, thu, fri, sat, or sun")
		}
		spec.Days = days
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
	}
	return normalized, err
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

func loadZone(zone string) (*time.Location, error) {
	if zone == "" || zone == "Local" {
		return nil, fmt.Errorf("the time zone must be an IANA name such as Europe/Amsterdam")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("the time zone %q is not known", zone)
	}
	return location, nil
}

// clockOn is the local HH:MM time on the day of the given instant, in that instant's zone.
func clockOn(day time.Time, clock string) (time.Time, error) {
	parsed, err := time.Parse(clockLayout, clock)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(day.Year(), day.Month(), day.Day(), parsed.Hour(), parsed.Minute(), 0, 0, day.Location()), nil
}
