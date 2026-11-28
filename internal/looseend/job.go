package looseend

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	jobCatchUp         = 12 * time.Hour
	jobMinimumInterval = time.Hour
	// maximumNudgeItems keeps one nudge message within Telegram's limits on text and buttons; more
	// items due on one day go out as further messages.
	maximumNudgeItems = 10
	// threadLookupTimeout bounds every thread lookup of one nudge message together, so slow mail
	// delays the message by at most that; a lookup that misses it only leaves its note out.
	threadLookupTimeout = 10 * time.Second
)

var (
	checkedText = locale.T("Checked your loose ends; the ones that need you are above.", "Проверил незакрытые дела; те, что требуют внимания, выше.")
	skippedNote = locale.T("Two Loose ends checks in a row could not read your new mail from %s to %s, so I skipped it and will not look for deadlines in it again; look through it yourself.", "Две проверки незакрытых дел подряд не смогли прочитать твою новую почту с %s по %s, поэтому она пропущена, и я не буду снова искать в ней сроки; просмотри её самостоятельно.")
)

// SwitchState reports whether a principal has Loose ends on; it is the switch's toggle.
type SwitchState interface {
	Enabled(ctx context.Context, principalID string) (bool, error)
}

// Reports keeps a nudge message as one of the job's reports; it is the scheduler.
type Reports interface {
	SaveSentReport(ctx context.Context, report schedule.Report) error
}

// Threads lists the messages of one Gmail thread; it is the Gmail service.
type Threads interface {
	ListThread(ctx context.Context, principalID, account, threadID string) ([]gmail.ThreadMessage, error)
}

// JobConfig is what the daily Loose ends run needs besides the service. Scanner is nil while no
// scan is wired; a run then only expires and nudges. Threads is nil while no mail is wired; sent
// promises are then nudged without saying whether the user wrote again.
type JobConfig struct {
	Switch  SwitchState
	Nudger  Nudger
	Scanner Scanner
	Threads Threads
	Reports Reports
	Now     func() time.Time
}

// JobType is the job type of the daily Loose ends run.
func (s *Service) JobType(cfg JobConfig) (schedule.JobType, error) {
	if cfg.Switch == nil || cfg.Nudger == nil || cfg.Reports == nil {
		return schedule.JobType{}, fmt.Errorf("the Loose ends job needs a switch, a nudger, and reports")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return schedule.JobType{
		Name:            schedule.LooseEndScanType,
		Handler:         job{service: s, config: cfg},
		CatchUp:         jobCatchUp,
		MinimumInterval: jobMinimumInterval,
	}, nil
}

// JobConfigJSON is the stored configuration of a Loose ends job, which holds nothing.
func JobConfigJSON() json.RawMessage { return json.RawMessage(`{}`) }

type job struct {
	service *Service
	config  JobConfig
}

func (job) Validate(config json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	var empty struct{}
	if err := decoder.Decode(&empty); err != nil {
		return fmt.Errorf("the Loose ends job takes no settings")
	}
	return nil
}

// Run expires what is past due, sends the day's nudges, runs the scan, and then nudges what the
// scan found already inside its lead time. The nudges come before the scan, so a slow or failed
// scan never holds them back.
func (j job) Run(ctx context.Context, occurrence schedule.Occurrence) (schedule.Result, error) {
	s, principalID := j.service, occurrence.PrincipalID
	enabled, err := j.config.Switch.Enabled(ctx, principalID)
	if err != nil {
		return schedule.Result{}, err
	}
	if !enabled {
		return schedule.Result{Quiet: true}, nil
	}
	// A switch turned on other than from the settings page, as a restore does, has no scan state
	// yet. It is started right after the switch was read, so a switch turned off meanwhile is
	// unlikely to be left with one.
	var startErr error
	if j.config.Scanner != nil {
		_, startErr = s.StartScan(ctx, principalID)
	}
	now := j.config.Now()
	expired, err := s.Expire(ctx, principalID, now)
	if err != nil {
		return schedule.Result{}, err
	}
	sent := &nudges{occurrence: occurrence}
	nudged, nudgeErr := j.nudge(ctx, sent, now)
	scanErr := startErr
	if scanErr == nil {
		scanErr = j.scan(ctx, principalID, now)
	}
	if ctx.Err() != nil {
		return schedule.Result{}, ctx.Err()
	}
	var found int
	// Items left over by a failed nudge would come back here as found by the scan.
	if nudgeErr == nil {
		found, nudgeErr = j.nudge(ctx, sent, j.config.Now())
	}
	// Pruning only tidies tombstones of every principal, so its failure costs no run anything.
	if _, err := s.Prune(ctx, now); err != nil && ctx.Err() == nil {
		s.logger.WarnContext(ctx, "looseend.prune.failed", "principal_id", principalID, "job_id", occurrence.JobID, "error", err)
	}
	s.logger.InfoContext(ctx, "looseend.run.done", "principal_id", principalID, "job_id", occurrence.JobID,
		"expired", expired, "nudged", nudged, "found_due", found, "nudge_failed", nudgeErr != nil, "scan_failed", scanErr != nil)
	switch {
	case nudgeErr != nil:
		if ctx.Err() != nil {
			return schedule.Result{}, ctx.Err()
		}
		s.logger.WarnContext(ctx, "looseend.nudge.failed", "principal_id", principalID, "job_id", occurrence.JobID, "error", nudgeErr)
		return schedule.Result{Text: schedule.FailedText, ErrorKind: "nudge"}, nil
	case scanErr != nil:
		s.logger.WarnContext(ctx, "looseend.scan.failed", "principal_id", principalID, "job_id", occurrence.JobID, "error", scanErr)
		return j.scanFailure(ctx, occurrence, scanErr), nil
	case occurrence.Manual && nudged+found > 0:
		// A run the user asked for says what it sent, where the scheduler would otherwise add that
		// there is nothing to report.
		return schedule.Result{Text: checkedText.Format(ctx)}, nil
	}
	return schedule.Result{Quiet: true}, nil
}

// scanFailure is what a failed scan tells the user: the turn's own failure, that new mail it could
// not read twice in a row is skipped, or that the run could not be completed.
func (j job) scanFailure(ctx context.Context, occurrence schedule.Occurrence, err error) schedule.Result {
	var failed *FailedError
	if !errors.As(err, &failed) {
		return schedule.Result{Text: schedule.FailedText, ErrorKind: "scan"}
	}
	kind := cmp.Or(failed.Kind, "scan")
	if failed.SkippedFrom.IsZero() || failed.SkippedUntil.IsZero() {
		return schedule.Result{Text: cmp.Or(failed.Text, schedule.FailedText), ErrorKind: kind}
	}
	location := cmp.Or(occurrence.Location, j.service.Location())
	language := locale.From(ctx)
	return schedule.Result{ErrorKind: kind, Text: skippedNote.Format(ctx,
		locale.Date(language, failed.SkippedFrom.In(location), locale.DayTime), locale.Date(language, failed.SkippedUntil.In(location), locale.DayTime))}
}

func (j job) scan(ctx context.Context, principalID string, now time.Time) error {
	if j.config.Scanner == nil {
		return nil
	}
	return j.config.Scanner.Scan(ctx, principalID, now)
}

// nudges numbers the messages one run sends, each its own report.
type nudges struct {
	occurrence schedule.Occurrence
	count      int
}

// nudge sends the principal's items due at now, in messages of at most maximumNudgeItems, keeps
// each message as a report, and records the items it sent as nudged. It returns how many items it
// sent.
func (j job) nudge(ctx context.Context, sent *nudges, now time.Time) (int, error) {
	principalID := sent.occurrence.PrincipalID
	due, err := j.service.Due(ctx, principalID, now)
	if err != nil {
		return 0, err
	}
	total := 0
	for batch := range slices.Chunk(due, maximumNudgeItems) {
		lookups, cancel := context.WithTimeout(ctx, threadLookupTimeout)
		items := make([]DueItem, 0, len(batch))
		for _, item := range batch {
			items = append(items, DueItem{Item: item, WroteAgain: j.wroteAgain(lookups, principalID, item)})
		}
		cancel()
		text, message, err := j.config.Nudger.Nudge(ctx, principalID, items)
		if err != nil {
			return total, err
		}
		ids := make([]string, 0, len(batch))
		outside := false
		for _, item := range batch {
			ids = append(ids, item.ID)
			outside = outside || item.Outside
		}
		if err := j.service.MarkNudged(ctx, principalID, ids, now); err != nil {
			return total, err
		}
		total += len(batch)
		sent.count++
		j.report(ctx, sent, text, outside, message)
	}
	return total, nil
}

// wroteAgain is when the user last sent a message in a sent promise's thread after the promise, or
// zero when they did not. A thanks keeps no promise, so the item is nudged either way; a thread that
// cannot be read only leaves the note out.
func (j job) wroteAgain(ctx context.Context, principalID string, item Item) time.Time {
	if item.Source != SourceSent || item.ThreadRef == "" || j.config.Threads == nil {
		return time.Time{}
	}
	messages, err := j.config.Threads.ListThread(ctx, principalID, item.Account, item.ThreadRef)
	if err != nil {
		j.service.logger.InfoContext(ctx, "looseend.nudge.thread_unreadable", "principal_id", principalID, "item_id", item.ID, "error", err)
		return time.Time{}
	}
	at := slices.IndexFunc(messages, func(message gmail.ThreadMessage) bool { return message.ID == item.SourceRef })
	if at < 0 {
		return time.Time{}
	}
	var latest time.Time
	for _, message := range messages {
		if message.Sent && message.ID != item.SourceRef && message.Received.After(messages[at].Received) && message.Received.After(latest) {
			latest = message.Received
		}
	}
	return latest
}

// report keeps one nudge message as a report. A failure only costs later turns the report, so it is
// logged rather than failing the run, which would send the message again.
func (j job) report(ctx context.Context, sent *nudges, text string, outside bool, message schedule.Sent) {
	occurrence := sent.occurrence
	report := schedule.Report{
		ID: nudgeReportID(occurrence.RunID, sent.count), PrincipalID: occurrence.PrincipalID,
		JobID: occurrence.JobID, JobName: occurrence.JobName, JobType: schedule.LooseEndScanType,
		Text: text, External: outside, Destination: occurrence.Destination,
	}
	if message.MessageID > 0 {
		report.Messages = []schedule.Sent{message}
	}
	if err := j.config.Reports.SaveSentReport(ctx, report); err != nil && ctx.Err() == nil {
		j.service.logger.WarnContext(ctx, "looseend.report.save_failed", "principal_id", occurrence.PrincipalID, "job_id", occurrence.JobID, "error", err)
	}
}

// nudgeReportID is the report ID of a run's nth nudge message, shaped like a run's own so it is
// stored and read as any report is.
func nudgeReportID(runID string, n int) string {
	sum := sha256.Sum256([]byte("loose-end-nudge\x00" + runID + "\x00" + strconv.Itoa(n)))
	return hex.EncodeToString(sum[:8])
}
