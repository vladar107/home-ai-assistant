package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// The kinds of event a job can be anchored to. A calendar_event or trip anchor fires once, so its
// job is a one-time job whose instant the resolver moves; a calendar_match or place anchor repeats,
// so its job has neither a cron expression nor an instant.
const (
	AnchorCalendarEvent = "calendar_event"
	AnchorCalendarMatch = "calendar_match"
	AnchorTrip          = "trip"
	AnchorPlace         = "place"

	EdgeStart  = "start"
	EdgeEnd    = "end"
	EdgeArrive = "arrive"
	EdgeLeave  = "leave"

	// AnchorResolveInterval is how often the calendar and trip anchors of every enabled job are
	// read again.
	AnchorResolveInterval = 15 * time.Minute
	// AnchorMatchHorizon is how far ahead a calendar_match anchor looks for its next event.
	AnchorMatchHorizon = 14 * 24 * time.Hour

	anchorTick = time.Minute
	// anchorConfirmWithin is how close to its run an occurrence is read once more, so a run never
	// fires for an event cancelled since the last full pass.
	anchorConfirmWithin = 30 * time.Minute
	anchorCallTimeout   = 10 * time.Second
	// maximumAnchorPrincipals is how many principals' anchors are read at once.
	maximumAnchorPrincipals = 4
	// anchorFailureNotice is how many passes in a row must fail to read a job's anchor before its
	// owner is told, once.
	anchorFailureNotice = 3

	minimumEventOffset = -2880
	maximumEventOffset = 1440
	maximumPlaceOffset = 240
	maximumAnchorID    = 1024
	maximumCalendarID  = 256
	maximumStoredKey   = 512
	maximumAnchorTitle = 100
	maximumAnchorLabel = 200

	errorAnchorGone   = "anchor_gone"
	errorAnchorFailed = "anchor_failed"
	conditionUnmet    = "condition_unmet"
)

// ErrAnchorGone is what a resolver returns when the event a single-occurrence anchor names was
// cancelled or deleted.
var ErrAnchorGone = errors.New("the anchored event no longer exists")

// Anchor is the event a job runs relative to, OffsetMinutes before (negative) or after it. Label
// is the event's or trip's title as the resolver read it when the job was saved, for display.
type Anchor struct {
	Kind          string `json:"kind"`
	Account       string `json:"account,omitempty"`
	CalendarID    string `json:"calendar_id,omitempty"`
	EventID       string `json:"event_id,omitempty"`
	Title         string `json:"title,omitempty"`
	TripID        string `json:"trip_id,omitempty"`
	PlaceID       string `json:"place_id,omitempty"`
	Edge          string `json:"edge,omitempty"`
	Once          bool   `json:"once,omitempty"`
	OffsetMinutes int    `json:"offset_minutes"`
	Label         string `json:"label,omitempty"`
}

// Repeats reports whether the anchor fires for each of many events rather than once.
func (a Anchor) Repeats() bool { return a.Kind == AnchorCalendarMatch || a.Kind == AnchorPlace }

func (a Anchor) offset() time.Duration { return time.Duration(a.OffsetMinutes) * time.Minute }

// Condition is a job's condition as stored: the watch spec the watch service checks and its
// description as the user reads it.
type Condition struct {
	Spec        json.RawMessage `json:"spec"`
	Description string          `json:"description"`
}

// AnchorOccurrence is the event occurrence an anchor points at: its key, the instant of the edge
// the anchor names, and the event's title. A zero Key means no occurrence is known. Note is a line
// for the user about what the anchor needs, such as a live location shared for a place; it is told
// when the job is saved.
type AnchorOccurrence struct {
	Key   string
	At    time.Time
	Label string
	Note  string
}

// AnchorResolver finds the occurrence of one kind of anchor for its owner, with the owner's own
// credentials. A single-occurrence anchor's occurrence is returned wherever it now is, and
// ErrAnchorGone once it is cancelled; a repeating anchor's is the first whose run, its instant plus
// the offset, is after the given instant, and a zero occurrence when there is none.
// A repeating anchor never returns the occurrence whose key is exclude, the one it last ran for.
type AnchorResolver interface {
	ResolveAnchor(ctx context.Context, principalID string, anchor Anchor, after time.Time, exclude string) (AnchorOccurrence, error)
}

// AnchorGuard is a resolver that keeps what an anchor names from being deleted while a job naming
// it is stored with store, and returns ErrAnchorGone without storing once it is gone, such as a
// saved place deleted between a job's proposal and its confirmation.
type AnchorGuard interface {
	GuardAnchor(ctx context.Context, principalID string, anchor Anchor, store func() error) error
}

// guardAnchor stores the job with store under its anchor's guard, if its resolver has one. A gone
// anchor is refused as an action error, which the tools tell the user.
func (s *Service) guardAnchor(ctx context.Context, job Job, store func() error) error {
	anchor := JobAnchor(job)
	if anchor == nil {
		return store()
	}
	guard, ok := s.anchors[anchor.Kind].(AnchorGuard)
	if !ok {
		return store()
	}
	err := guard.GuardAnchor(ctx, job.PrincipalID, *anchor, store)
	if errors.Is(err, ErrAnchorGone) {
		return &approval.ActionError{Reason: errorAnchorGone, Text: "What this job follows no longer exists, so it was not saved.", UserText: anchorGoneNotSaved.Format(ctx)}
	}
	return err
}

// Conditions checks job conditions: the watch service, whose code-checked kinds and agent turn
// a condition reuses. PrepareCondition validates a condition as the model gives it, in the fields
// watch_propose takes, and returns its stored spec and description; light refuses a condition
// that needs a model turn. CheckCondition checks a stored spec once at the instant.
type Conditions interface {
	PrepareCondition(ctx context.Context, principalID string, raw json.RawMessage, light bool) (json.RawMessage, string, error)
	// ValidateCondition checks a stored condition as a new one is checked, without looking anything
	// up, and returns it in its canonical form, as a restore stores it.
	ValidateCondition(spec json.RawMessage, light bool) (json.RawMessage, error)
	CheckCondition(ctx context.Context, principalID string, spec json.RawMessage, at time.Time) (ConditionResult, error)
}

// RegisterAnchor adds the resolver of one kind of anchor at wiring time, before the scheduler
// starts. A kind without one cannot be proposed.
func (s *Service) RegisterAnchor(kind string, resolver AnchorResolver) error {
	switch kind {
	case AnchorCalendarEvent, AnchorCalendarMatch, AnchorTrip, AnchorPlace:
	default:
		return fmt.Errorf("register anchor resolver: unknown kind %q", kind)
	}
	if resolver == nil {
		return fmt.Errorf("register anchor resolver: %q has no resolver", kind)
	}
	if _, exists := s.anchors[kind]; exists {
		return fmt.Errorf("register anchor resolver: %q is already registered", kind)
	}
	s.anchors[kind] = resolver
	return nil
}

// SetConditions sets what checks job conditions, at wiring time before the scheduler starts;
// without it no condition can be proposed, and a stored one is reported as not checked.
func (s *Service) SetConditions(conditions Conditions) {
	s.conditions = conditions
}

// SplitConfig separates a job's configuration into what its type's handler reads and the anchor
// and condition the scheduler reads, each nil when the job has none.
func SplitConfig(config json.RawMessage) (json.RawMessage, *Anchor, *Condition, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		return config, nil, nil, nil
	}
	rawAnchor, hasAnchor := fields["anchor"]
	rawCondition, hasCondition := fields["condition"]
	if !hasAnchor && !hasCondition {
		return config, nil, nil, nil
	}
	delete(fields, "anchor")
	delete(fields, "condition")
	handler, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, nil, err
	}
	var anchor *Anchor
	if hasAnchor {
		anchor = &Anchor{}
		if err := json.Unmarshal(rawAnchor, anchor); err != nil {
			return nil, nil, nil, fmt.Errorf("the job's anchor could not be read")
		}
	}
	var condition *Condition
	if hasCondition {
		condition = &Condition{}
		if err := json.Unmarshal(rawCondition, condition); err != nil || len(condition.Spec) == 0 || strings.TrimSpace(condition.Description) == "" {
			return nil, nil, nil, fmt.Errorf("the job's condition could not be read")
		}
	}
	return handler, anchor, condition, nil
}

// JoinConfig adds an anchor and a condition to a handler's configuration; nil leaves either out.
func JoinConfig(handler json.RawMessage, anchor *Anchor, condition *Condition) (json.RawMessage, error) {
	if anchor == nil && condition == nil {
		return handler, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(handler, &fields); err != nil {
		return nil, fmt.Errorf("the job's configuration could not be read")
	}
	if anchor != nil {
		encoded, err := json.Marshal(anchor)
		if err != nil {
			return nil, err
		}
		fields["anchor"] = encoded
	}
	if condition != nil {
		encoded, err := json.Marshal(condition)
		if err != nil {
			return nil, err
		}
		fields["condition"] = encoded
	}
	return json.Marshal(fields)
}

// JobAnchor is the job's anchor, nil for a job that runs on a clock time.
func JobAnchor(job Job) *Anchor {
	_, anchor, _, err := SplitConfig(job.Config)
	if err != nil {
		return nil
	}
	return anchor
}

// JobCondition is the job's condition, nil when it has none.
func JobCondition(job Job) *Condition {
	_, _, condition, err := SplitConfig(job.Config)
	if err != nil {
		return nil
	}
	return condition
}

// normalizeAnchor checks an anchor as the model gives it and returns it in its canonical form,
// without a label, which only the resolver sets.
func normalizeAnchor(anchor Anchor) (Anchor, error) {
	normalized := Anchor{Kind: strings.TrimSpace(anchor.Kind), OffsetMinutes: anchor.OffsetMinutes}
	var err error
	field := func(value, name string, limit int, required bool) string {
		if err != nil {
			return ""
		}
		value = strings.TrimSpace(value)
		switch {
		case value == "" && required:
			err = fmt.Errorf("a %s anchor needs %s", normalized.Kind, name)
		case !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit || strings.IndexFunc(value, unicode.IsControl) >= 0:
			err = fmt.Errorf("the anchor's %s must be a single line of at most %d characters", name, limit)
		}
		return value
	}
	unused := func(values ...string) bool {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return false
			}
		}
		return true
	}
	switch normalized.Kind {
	case AnchorCalendarEvent:
		normalized.Account = field(anchor.Account, "account", maximumAnchorTitle, true)
		normalized.CalendarID = field(anchor.CalendarID, "calendar_id", maximumCalendarID, true)
		normalized.EventID = field(anchor.EventID, "event_id", maximumAnchorID, true)
		if !unused(anchor.Title, anchor.TripID, anchor.PlaceID, anchor.Edge) || anchor.Once {
			return Anchor{}, fmt.Errorf("a calendar_event anchor takes account, calendar_id, event_id, and offset_minutes only")
		}
	case AnchorCalendarMatch:
		normalized.Account = field(anchor.Account, "account", maximumAnchorTitle, false)
		normalized.CalendarID = field(anchor.CalendarID, "calendar_id", maximumCalendarID, false)
		normalized.Title = field(anchor.Title, "title", maximumAnchorTitle, true)
		if !unused(anchor.EventID, anchor.TripID, anchor.PlaceID, anchor.Edge) || anchor.Once {
			return Anchor{}, fmt.Errorf("a calendar_match anchor takes title, optionally account and calendar_id, and offset_minutes only")
		}
	case AnchorTrip:
		normalized.TripID = field(anchor.TripID, "trip_id", maximumAnchorTitle, true)
		normalized.Edge = strings.TrimSpace(anchor.Edge)
		if normalized.Edge != EdgeStart && normalized.Edge != EdgeEnd {
			return Anchor{}, fmt.Errorf("a trip anchor's edge must be start or end")
		}
		if !unused(anchor.Account, anchor.CalendarID, anchor.EventID, anchor.Title, anchor.PlaceID) || anchor.Once {
			return Anchor{}, fmt.Errorf("a trip anchor takes trip_id, edge, and offset_minutes only")
		}
	case AnchorPlace:
		normalized.PlaceID = field(anchor.PlaceID, "place_id", maximumAnchorTitle, true)
		normalized.Edge = strings.TrimSpace(anchor.Edge)
		normalized.Once = anchor.Once
		if err == nil && normalized.Edge != EdgeArrive && normalized.Edge != EdgeLeave {
			return Anchor{}, fmt.Errorf("a place anchor's edge must be arrive or leave")
		}
		if !unused(anchor.Account, anchor.CalendarID, anchor.EventID, anchor.Title, anchor.TripID) {
			return Anchor{}, fmt.Errorf("a place anchor takes place_id, edge, optionally once, and offset_minutes only")
		}
		if err == nil && (normalized.OffsetMinutes < 0 || normalized.OffsetMinutes > maximumPlaceOffset) {
			return Anchor{}, fmt.Errorf("a place anchor's offset_minutes must be between 0 and %d, since it runs on or after arriving or leaving", maximumPlaceOffset)
		}
		return normalized, err
	default:
		return Anchor{}, fmt.Errorf("the anchor's kind must be calendar_event, calendar_match, trip, or place")
	}
	if err != nil {
		return Anchor{}, err
	}
	if normalized.OffsetMinutes < minimumEventOffset || normalized.OffsetMinutes > maximumEventOffset {
		return Anchor{}, fmt.Errorf("offset_minutes must be between %d (two days before) and %d (a day after)", minimumEventOffset, maximumEventOffset)
	}
	return normalized, nil
}

// checkStoredAnchor checks an anchor as stored, with the label its resolver set.
func checkStoredAnchor(anchor Anchor) error {
	label := anchor.Label
	normalized, err := normalizeAnchor(anchor)
	if err != nil {
		return err
	}
	normalized.Label = label
	if normalized != anchor {
		return fmt.Errorf("the job's anchor is not in its stored form")
	}
	return nil
}

// Describe is the anchor as the user reads it, in English, such as "30 min before Dentist" or "1 h
// after the end of the trip Lisbon".
func (a Anchor) Describe() string { return a.DescribeIn(locale.English) }

var (
	anchorEvent        = locale.T("the event", "события")
	anchorAtStart      = locale.T("at the start of %s", "в начале %s")
	anchorBefore       = locale.T("%[1]s before %[2]s", "за %[1]s до %[2]s")
	anchorAfter        = locale.T("%[1]s after %[2]s", "через %[1]s после %[2]s")
	anchorEachEvent    = locale.T("each event titled %s", "каждого события %s")
	anchorTrip         = locale.T("the trip", "поездки")
	anchorTripNamed    = locale.T("the trip %s", "поездки %s")
	anchorTripAt       = locale.T("at the %[1]s of %[2]s", "в %[1]s %[2]s")
	anchorTripBefore   = locale.T("%[1]s before the %[2]s of %[3]s", "за %[1]s до %[2]s %[3]s")
	anchorTripAfter    = locale.T("%[1]s after the %[2]s of %[3]s", "через %[1]s после %[2]s %[3]s")
	anchorEdgeStartAt  = locale.T("start", "начале")
	anchorEdgeEndAt    = locale.T("end", "конце")
	anchorEdgeStartOf  = locale.T("start", "начала")
	anchorEdgeEndOf    = locale.T("end", "конца")
	anchorPlaceFallout = locale.T("the place", "этого места")
	anchorPlaceFallin  = locale.T("the place", "это место")
	anchorArrive       = locale.T("on arriving at %s", "при прибытии в %s")
	anchorLeave        = locale.T("on leaving %s", "при выходе из %s")
	anchorArriveAfter  = locale.T("%[1]s after arriving at %[2]s", "через %[1]s после прибытия в %[2]s")
	anchorLeaveAfter   = locale.T("%[1]s after leaving %[2]s", "через %[1]s после выхода из %[2]s")
	anchorOnce         = locale.T("%s, once", "%s, один раз")

	anchorGoneNotSaved = locale.T("What this job follows no longer exists, so it was not saved.", "То, за чем следит это задание, больше не существует, поэтому оно не сохранено.")

	offsetDays    = locale.N("%d day", "%d days", "%d день", "%d дня", "%d дней")
	offsetHours   = locale.T("%d h", "%d ч")
	offsetMinutes = locale.T("%d min", "%d мин")

	nextNever        = locale.T("never", "никогда")
	nextWaitsLeaving = locale.T("waits for leaving", "ждёт ухода")
	nextWaitsArrival = locale.T("waits for arrival", "ждёт прибытия")
	nextNoEvent      = locale.T("no matching event in the next 14 days", "нет подходящих событий в ближайшие 14 дней")
)

// quotedIn writes a title the user or an outside source chose: Go-quoted in English, in guillemets
// in Russian.
func quotedIn(language locale.Language, text string) string {
	if language == locale.Russian {
		return "«" + text + "»"
	}
	return strconv.Quote(text)
}

// labelIn writes an event's or a place's name, which English leaves bare and Russian sets off.
func labelIn(language locale.Language, label, fallback string) string {
	switch {
	case label == "":
		return fallback
	case language == locale.Russian:
		return quotedIn(language, label)
	}
	return label
}

// DescribeIn is Describe in a language.
func (a Anchor) DescribeIn(language locale.Language) string {
	offset := ""
	switch {
	case a.OffsetMinutes < 0:
		offset = describeOffsetIn(language, -a.OffsetMinutes)
	case a.OffsetMinutes > 0:
		offset = describeOffsetIn(language, a.OffsetMinutes)
	}
	switch a.Kind {
	case AnchorCalendarEvent:
		return a.relative(language, offset, labelIn(language, a.Label, anchorEvent.In(language)))
	case AnchorCalendarMatch:
		return a.relative(language, offset, anchorEachEvent.FormatIn(language, quotedIn(language, a.Title)))
	case AnchorTrip:
		name := anchorTrip.In(language)
		if a.Label != "" {
			name = anchorTripNamed.FormatIn(language, quotedIn(language, a.Label))
		}
		start := a.Edge == EdgeStart
		switch {
		case a.OffsetMinutes < 0:
			return anchorTripBefore.FormatIn(language, offset, tripEdge(language, start, false), name)
		case a.OffsetMinutes > 0:
			return anchorTripAfter.FormatIn(language, offset, tripEdge(language, start, false), name)
		}
		return anchorTripAt.FormatIn(language, tripEdge(language, start, true), name)
	case AnchorPlace:
		leaving := a.Edge == EdgeLeave
		fallback := anchorPlaceFallin.In(language)
		if leaving {
			fallback = anchorPlaceFallout.In(language)
		}
		name := labelIn(language, a.Label, fallback)
		var text string
		switch {
		case a.OffsetMinutes > 0 && leaving:
			text = anchorLeaveAfter.FormatIn(language, offset, name)
		case a.OffsetMinutes > 0:
			text = anchorArriveAfter.FormatIn(language, offset, name)
		case leaving:
			text = anchorLeave.FormatIn(language, name)
		default:
			text = anchorArrive.FormatIn(language, name)
		}
		if a.Once {
			text = anchorOnce.FormatIn(language, text)
		}
		return text
	}
	return a.Kind
}

// relative places a name by the anchor's offset: at its start, before it, or after it.
func (a Anchor) relative(language locale.Language, offset, name string) string {
	switch {
	case a.OffsetMinutes < 0:
		return anchorBefore.FormatIn(language, offset, name)
	case a.OffsetMinutes > 0:
		return anchorAfter.FormatIn(language, offset, name)
	}
	return anchorAtStart.FormatIn(language, name)
}

// tripEdge names the start or the end of a trip, in the case the phrase around it needs.
func tripEdge(language locale.Language, start, at bool) string {
	switch {
	case start && at:
		return anchorEdgeStartAt.In(language)
	case start:
		return anchorEdgeStartOf.In(language)
	case at:
		return anchorEdgeEndAt.In(language)
	}
	return anchorEdgeEndOf.In(language)
}

func describeOffsetIn(language locale.Language, minutes int) string {
	days, hours, rest := minutes/1440, minutes%1440/60, minutes%60
	var parts []string
	if days > 0 {
		parts = append(parts, offsetDays.FormatIn(language, days))
	}
	if hours > 0 {
		parts = append(parts, offsetHours.FormatIn(language, hours))
	}
	if rest > 0 {
		parts = append(parts, offsetMinutes.FormatIn(language, rest))
	}
	return strings.Join(parts, " ")
}

// waitingFrom tells the next run of a job with no next occurrence, which is set 100 years ahead,
// from any real one, which is at most a year ahead.
var waitingFrom = time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)

// waiting reports whether the job has no next occurrence: a repeating anchor that has not found
// its next event, or a place it waits to arrive at.
func waiting(job Job) bool { return !job.NextRunAt.Before(waitingFrom) }

// NextRunText is when the job runs next as the user reads it, in English: the time, or what it
// waits for.
func NextRunText(job Job, zone string) string { return NextRunTextIn(locale.English, job, zone) }

// NextRunTextIn is NextRunText in a language.
func NextRunTextIn(language locale.Language, job Job, zone string) string {
	if !waiting(job) {
		location, err := LoadZone(job.Zone)
		if err != nil {
			location, _ = LoadZone(zone)
		}
		return FormatOccurrenceIn(language, job.NextRunAt, location)
	}
	anchor := JobAnchor(job)
	switch {
	case anchor == nil:
		return nextNever.In(language)
	case anchor.Kind == AnchorPlace && anchor.Edge == EdgeLeave:
		return nextWaitsLeaving.In(language)
	case anchor.Kind == AnchorPlace:
		return nextWaitsArrival.In(language)
	default:
		return nextNoEvent.In(language)
	}
}

// resolvePrepared finds when a prepared anchored job first runs, with the owner's own
// credentials, and fills in its instant or next run, its occurrence, and the anchor's label.
func (s *Service) resolvePrepared(ctx context.Context, prepared *Prepared) error {
	anchor := prepared.Anchor
	if anchor == nil {
		return nil
	}
	resolver, known := s.anchors[anchor.Kind]
	if !known {
		return fmt.Errorf("%s anchors are not available", anchor.Kind)
	}
	now := s.now()
	callCtx, cancel := context.WithTimeout(ctx, anchorCallTimeout)
	occurrence, err := resolver.ResolveAnchor(callCtx, prepared.Job.PrincipalID, *anchor, now, "")
	cancel()
	if errors.Is(err, ErrAnchorGone) {
		return fmt.Errorf("that event no longer exists or was cancelled")
	}
	if err != nil {
		return err
	}
	job := &prepared.Job
	if label := strings.TrimSpace(occurrence.Label); label != "" && anchor.Kind != AnchorCalendarMatch {
		anchor.Label = clipLabel(label)
	}
	prepared.note = clipLabel(occurrence.Note)
	if anchor.Repeats() {
		job.NextRunAt, job.OccurrenceKey = now.AddDate(100, 0, 0), ""
		if occurrence.Key != "" {
			job.NextRunAt, job.OccurrenceKey = occurrence.At.Add(anchor.offset()).UTC(), StoredKey(occurrence.Key)
		}
	} else {
		if occurrence.Key == "" {
			return fmt.Errorf("that event could not be found")
		}
		at := occurrence.At.Add(anchor.offset()).UTC()
		if !at.After(now) {
			return fmt.Errorf("%s is already past", anchor.Describe())
		}
		if at.Sub(now) > maximumLeadTime {
			return fmt.Errorf("a one-time job can be set at most a year ahead")
		}
		job.RunAt, job.NextRunAt, job.OccurrenceKey = at, at, StoredKey(occurrence.Key)
		prepared.Schedule.Once = at
	}
	handler, _, condition, err := SplitConfig(job.Config)
	if err != nil {
		return err
	}
	if job.Config, err = JoinConfig(handler, anchor, condition); err != nil {
		return err
	}
	prepared.resolved = true
	return nil
}

func clipLabel(label string) string {
	label = strings.Join(strings.FieldsFunc(label, unicode.IsControl), " ")
	label = strings.Join(strings.Fields(label), " ")
	if runes := []rune(label); len(runes) > maximumAnchorLabel {
		label = string(runes[:maximumAnchorLabel])
	}
	return label
}

// prepareJob is Prepare for a job that may be anchored: an anchored job also has its first
// occurrence found.
func (s *Service) prepareJob(ctx context.Context, principalID, name, jobType string, timing Timing, config json.RawMessage) (Prepared, error) {
	prepared, err := s.Prepare(principalID, name, jobType, timing, config)
	if err != nil {
		return Prepared{}, err
	}
	if err := s.resolvePrepared(ctx, &prepared); err != nil {
		return Prepared{}, err
	}
	return prepared, nil
}

// prepareCondition turns the condition the model gave into its stored form; raw "null" removes
// the job's condition.
func (s *Service) prepareCondition(ctx context.Context, principalID string, raw json.RawMessage, light bool) (*Condition, error) {
	if s.conditions == nil {
		return nil, fmt.Errorf("conditions are not available, because watches are off")
	}
	spec, description, err := s.conditions.PrepareCondition(ctx, principalID, raw, light)
	if err != nil {
		return nil, err
	}
	return &Condition{Spec: spec, Description: description}, nil
}

// anchorState is what the resolver pass remembers about one job between passes.
type anchorState struct {
	resolvedAt time.Time
	// confirmed is the next run last confirmed within anchorConfirmWithin of it.
	confirmed time.Time
	failures  int
	noticed   bool
}

// wakeAnchors asks the resolver pass to read the job's anchor at once, such as after a run of a
// repeating anchor, whose next occurrence the serial claim loop does not look up itself.
func (s *Service) wakeAnchors(jobID string) {
	s.mu.Lock()
	s.anchorForced[jobID] = true
	s.mu.Unlock()
	s.WakeAnchors()
}

// WakeAnchors asks for a resolver pass now instead of at the next minute.
func (s *Service) WakeAnchors() {
	select {
	case s.anchorWake <- struct{}{}:
	default:
	}
}

func (s *Service) anchorLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	defer s.anchorWorkers.Wait()
	ticker := time.NewTicker(anchorTick)
	defer ticker.Stop()
	for {
		s.resolveAnchors(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.anchorWake:
		}
	}
}

// anchorWork is one job a pass reads, with what the pass remembers about it.
type anchorWork struct {
	job    Job
	anchor Anchor
	state  *anchorState
}

// resolveAnchors is one pass over the enabled anchored jobs. A job is read again every
// AnchorResolveInterval, when it was just saved or run, and once more as its run comes within
// anchorConfirmWithin. A job already due is the claim loop's, a snoozed reminder keeps the time it
// was snoozed to, and a place anchor is moved by arrivals rather than read. Each principal's jobs
// are read in a worker of their own, at most maximumAnchorPrincipals at once, so one owner's slow
// calendar holds up only that owner's jobs; a principal whose worker is still reading is left to a
// later pass.
func (s *Service) resolveAnchors(ctx context.Context) {
	// The forced jobs are taken before the jobs are read, so a wake that comes in between is
	// either in this pass's list or kept for the next.
	s.mu.Lock()
	forced := s.anchorForced
	s.anchorForced = make(map[string]bool)
	s.mu.Unlock()
	jobs, err := s.repository.AnchoredScheduledJobs(ctx)
	if err != nil {
		s.keepForced(forced)
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "schedule.anchor.failed", "stage", "load_jobs", "error", err)
		}
		return
	}
	s.mu.Lock()
	busy := make(map[string]bool, len(s.anchorBusy))
	for principal := range s.anchorBusy {
		busy[principal] = true
	}
	s.mu.Unlock()
	seen := make(map[string]bool, len(jobs))
	work := make(map[string][]anchorWork)
	var principals []string
	var deferred []string
	for _, job := range jobs {
		seen[job.ID] = true
		anchor := JobAnchor(job)
		now := s.now()
		if anchor == nil || !job.Enabled || !job.NextRunAt.After(now) || anchor.Kind == AnchorPlace || (!anchor.Repeats() && job.OccurrenceKey == "") {
			continue
		}
		if busy[job.PrincipalID] {
			if forced[job.ID] {
				deferred = append(deferred, job.ID)
			}
			continue
		}
		state := s.anchorStates[job.ID]
		if state == nil {
			state = &anchorState{}
			s.anchorStates[job.ID] = state
		}
		confirm := !waiting(job) && job.NextRunAt.Sub(now) <= anchorConfirmWithin && !state.confirmed.Equal(job.NextRunAt)
		if !forced[job.ID] && !confirm && now.Sub(state.resolvedAt) < AnchorResolveInterval {
			continue
		}
		if len(work[job.PrincipalID]) == 0 {
			principals = append(principals, job.PrincipalID)
		}
		work[job.PrincipalID] = append(work[job.PrincipalID], anchorWork{job: job, anchor: *anchor, state: state})
	}
	for id := range s.anchorStates {
		if !seen[id] {
			delete(s.anchorStates, id)
		}
	}
	for _, principal := range principals {
		s.mu.Lock()
		started := len(s.anchorBusy) < maximumAnchorPrincipals
		if started {
			s.anchorBusy[principal] = true
		}
		s.mu.Unlock()
		if !started {
			for _, item := range work[principal] {
				if forced[item.job.ID] {
					deferred = append(deferred, item.job.ID)
				}
			}
			continue
		}
		s.anchorWorkers.Add(1)
		go s.resolvePrincipal(ctx, principal, work[principal])
	}
	if len(deferred) > 0 {
		forcedLater := make(map[string]bool, len(deferred))
		for _, id := range deferred {
			forcedLater[id] = true
		}
		s.keepForced(forcedLater)
	}
}

// resolvePrincipal reads one principal's due anchors in turn and asks for another pass once done,
// when jobs were left waiting for it.
func (s *Service) resolvePrincipal(ctx context.Context, principalID string, work []anchorWork) {
	defer s.anchorWorkers.Done()
	for _, item := range work {
		if ctx.Err() != nil {
			break
		}
		s.resolveJob(ctx, item.job, item.anchor, item.state, s.now())
	}
	s.mu.Lock()
	delete(s.anchorBusy, principalID)
	pending := len(s.anchorForced) > 0
	s.mu.Unlock()
	if pending && ctx.Err() == nil {
		s.WakeAnchors()
	}
}

// keepForced puts forced jobs back for the next pass.
func (s *Service) keepForced(forced map[string]bool) {
	s.mu.Lock()
	for id := range forced {
		s.anchorForced[id] = true
	}
	s.mu.Unlock()
}

// resolveJob reads one job's anchor and moves the job to where its occurrence now is. A
// single-occurrence job whose event is gone is finished with one line to its owner, and a job
// whose anchor cannot be read keeps its time and tells its owner once after several passes.
func (s *Service) resolveJob(ctx context.Context, job Job, anchor Anchor, state *anchorState, now time.Time) {
	resolver, known := s.anchors[anchor.Kind]
	if !known {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, anchorCallTimeout)
	occurrence, err := resolver.ResolveAnchor(callCtx, job.PrincipalID, anchor, now, job.LastOccurrenceKey)
	cancel()
	if ctx.Err() != nil {
		return
	}
	state.resolvedAt = now
	if errors.Is(err, ErrAnchorGone) && !anchor.Repeats() {
		s.anchorNotice(ctx, job, anchorGoneText(job, anchor), errorAnchorGone, true)
		return
	}
	if err != nil {
		state.failures++
		s.logger.WarnContext(ctx, "schedule.anchor.failed", "principal_id", job.PrincipalID, "job_id", job.ID, "kind", anchor.Kind, "failures", state.failures, "error", err)
		if state.failures >= anchorFailureNotice && !state.noticed {
			state.noticed = true
			s.anchorNotice(ctx, job, anchorFailedText(anchor), errorAnchorFailed, false)
		}
		return
	}
	state.failures, state.noticed = 0, false
	runAt, next, key := job.RunAt, job.NextRunAt, StoredKey(occurrence.Key)
	switch {
	case anchor.Repeats() && occurrence.Key == "":
		if waiting(job) {
			return
		}
		next = now.AddDate(100, 0, 0)
	case anchor.Repeats():
		next = occurrence.At.Add(anchor.offset()).UTC()
	default:
		runAt = occurrence.At.Add(anchor.offset()).UTC()
		next = runAt
	}
	if next.Sub(now) <= anchorConfirmWithin {
		state.confirmed = next
	}
	if next.Equal(job.NextRunAt) && key == job.OccurrenceKey {
		return
	}
	moved, err := s.repository.MoveScheduledJobOccurrence(ctx, job.ID, job.NextRunAt, runAt, next, key)
	if err != nil {
		s.logger.WarnContext(ctx, "schedule.anchor.failed", "principal_id", job.PrincipalID, "job_id", job.ID, "stage", "move", "error", err)
		return
	}
	if moved {
		s.logger.InfoContext(ctx, "schedule.anchor.moved", "principal_id", job.PrincipalID, "job_id", job.ID, "kind", anchor.Kind)
		if !next.After(s.now()) {
			s.Wake()
		}
	}
}

// anchorNotice queues one line about the job for its owner's Reminders tab and, with finish, ends
// the job in the same transaction.
func (s *Service) anchorNotice(ctx context.Context, job Job, text, errorKind string, finish bool) {
	id, err := newRunID()
	if err != nil {
		return
	}
	now := s.now()
	run := Run{ID: id, JobID: job.ID, PrincipalID: job.PrincipalID, ScheduledFor: now, Status: StatusReady, Reply: text, ErrorKind: errorKind}
	queued, err := s.repository.QueueAnchorNotice(ctx, run, finish, job.NextRunAt, now)
	if err != nil {
		s.logger.WarnContext(ctx, "schedule.anchor.failed", "principal_id", job.PrincipalID, "job_id", job.ID, "stage", "notice", "error", err)
		return
	}
	if queued {
		s.logger.InfoContext(ctx, "schedule.anchor.notice", "principal_id", job.PrincipalID, "job_id", job.ID, "error_kind", errorKind, "finished", finish)
		s.Wake()
	}
}

func anchorGoneText(job Job, anchor Anchor) string {
	thing := "the reminder"
	if job.Type != TypeReminder {
		thing = "the job"
	}
	what := "Event"
	if anchor.Kind == AnchorTrip {
		what = "Trip"
	}
	return what + " cancelled, so " + thing + " is gone."
}

func anchorFailedText(anchor Anchor) string {
	source := "the calendar"
	if anchor.Kind == AnchorTrip {
		source = "the trip"
	}
	return "I can't read " + source + " this job follows right now, such as because the calendar was switched off in /settings. It keeps the last time I found, and I'll keep trying."
}

// enabledOccurrence is where an anchored job resumes: its occurrence read now, or no next run
// for a place it waits for and a match not yet found.
func (s *Service) enabledOccurrence(ctx context.Context, job Job, anchor Anchor, now time.Time) (time.Time, time.Time, string, error) {
	if anchor.Kind == AnchorPlace {
		return job.RunAt, now.AddDate(100, 0, 0), "", nil
	}
	resolver, known := s.anchors[anchor.Kind]
	if !known {
		return time.Time{}, time.Time{}, "", fmt.Errorf("%s anchors are not available", anchor.Kind)
	}
	callCtx, cancel := context.WithTimeout(ctx, anchorCallTimeout)
	occurrence, err := resolver.ResolveAnchor(callCtx, job.PrincipalID, anchor, now, job.LastOccurrenceKey)
	cancel()
	if errors.Is(err, ErrAnchorGone) {
		return time.Time{}, time.Time{}, "", fmt.Errorf("the event this job follows no longer exists")
	}
	if err != nil {
		return time.Time{}, time.Time{}, "", err
	}
	if anchor.Repeats() {
		if occurrence.Key == "" {
			return job.RunAt, now.AddDate(100, 0, 0), "", nil
		}
		return job.RunAt, occurrence.At.Add(anchor.offset()).UTC(), StoredKey(occurrence.Key), nil
	}
	at := occurrence.At.Add(anchor.offset()).UTC()
	if !at.After(now) {
		return time.Time{}, time.Time{}, "", fmt.Errorf("the schedule never runs again")
	}
	return at, at, StoredKey(occurrence.Key), nil
}

// StoredKey keeps an occurrence key within what the store holds: a long one, such as a calendar
// and event ID that are both long, is kept as its digest, which still tells occurrences apart.
func StoredKey(key string) string {
	if len(key) <= maximumStoredKey {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PlaceEvent makes each of the principal's enabled jobs anchored to arriving at or leaving the place
// due at the event plus its offset. A job that already waits to run for an earlier one keeps that
// run, and a job's own next run is never moved for another principal's place. The claim loop then
// runs it as any due job.
func (s *Service) PlaceEvent(ctx context.Context, principalID, placeID, edge string, at time.Time) error {
	jobs, err := s.repository.ListScheduledJobs(ctx, principalID)
	if err != nil {
		return err
	}
	due := false
	for _, job := range jobs {
		anchor := JobAnchor(job)
		if !job.Enabled || !job.AnchorRepeats || anchor == nil || anchor.Kind != AnchorPlace || anchor.PlaceID != placeID || anchor.Edge != edge || !waiting(job) {
			continue
		}
		next := at.Add(anchor.offset()).UTC()
		key := StoredKey(placeID + ":" + edge + ":" + at.UTC().Format(time.RFC3339Nano))
		moved, err := s.repository.MoveScheduledJobOccurrence(ctx, job.ID, job.NextRunAt, job.RunAt, next, key)
		if err != nil {
			return err
		}
		if moved {
			s.logger.InfoContext(ctx, "schedule.anchor.moved", "principal_id", principalID, "job_id", job.ID, "kind", AnchorPlace)
			due = due || !next.After(s.now())
		}
	}
	if due {
		s.Wake()
	}
	return nil
}
