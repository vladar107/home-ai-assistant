package schedule

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	DefaultTickInterval = 30 * time.Second

	maximumConcurrentRuns = 2
	maximumNameRunes      = 60
	maximumNoticeRunes    = 2000
	runRetention          = 30 * 24 * time.Hour
	pruneInterval         = time.Hour
	deliveryStoreTimeout  = 5 * time.Second

	InterruptedText = "This run was interrupted by a restart and was not retried. It will run again at its next scheduled time."
	FailedText      = "This run could not be completed."
	QuietManualText = "Nothing to report right now."
	quietReason     = "quiet"
	MissedText      = "This one-time job did not run: the assistant was offline when it was due, and it is too late to run it now."
	// OneTimeInterruptedText replaces InterruptedText for a job that has no next scheduled time.
	OneTimeInterruptedText = "This one-time run was interrupted by a restart and was not retried."
)

// Chats resolves the principal's own private chat, which is the only place a run is delivered.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

type Config struct {
	// Zone is the IANA time zone a job gets when none is named.
	Zone         string
	TickInterval time.Duration
	Now          func() time.Time
	// Cards and Photos send the card a run staged with its report; nil leaves reports text only.
	Cards  conversation.Cards
	Photos conversation.PhotoDeliverer
	// Reminders sends reminders with their snooze buttons; nil sends them as plain text.
	Reminders ReminderDeliverer
	// Sessions removes old model sessions of finished runs and tasks; nil keeps them.
	Sessions SessionPruner
	// Travel is the trip lookup that jobs flagged to pause while traveling consult; without it
	// nobody is ever traveling.
	Travel Travel
	// Telemetry records run metrics.
	Telemetry Telemetry
	// Exposure makes reminders wait for confirmation once the conversation has read outside
	// content; without it reminders are always set at once.
	Exposure ExposureChecker
	// SafetyChecks labels scheduled reply metrics; without it they are labelled "unavailable".
	SafetyChecks SafetyChecks
	// Feeds are the tabs that runs are delivered to while the bot has tabs on; without them every
	// run goes to the main view.
	Feeds thread.Feeds
}

// SessionPruner removes the model session files that no conversation or open task refers to and
// that were last written before the cutoff.
type SessionPruner interface {
	PruneSessions(ctx context.Context, before time.Time) error
}

// ReminderDeliverer sends one reminder to its owner's chat with buttons that snooze the job, and
// returns the sent message's ID and the tab it landed in.
type ReminderDeliverer interface {
	SendReminder(ctx context.Context, chatID int64, jobID, text string) (int64, int64, error)
}

type Service struct {
	repository Repository
	chats      Chats
	deliverer  conversation.Deliverer
	cards      conversation.Cards
	photos     conversation.PhotoDeliverer
	reminders  ReminderDeliverer
	sessions   SessionPruner
	logger     *slog.Logger
	zone       string
	tick       time.Duration
	now        func() time.Time
	types      map[string]JobType
	proposer   Proposer
	exposure   ExposureChecker
	telemetry  Telemetry
	safety     SafetyChecks
	travel     Travel
	feeds      thread.Feeds
	startedAt  time.Time

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	wake     chan struct{}
	inflight map[string]bool
	running  int
	// executing holds the principals that have a bounded run executing.
	executing map[string]bool
	pruned    time.Time
	workers   sync.WaitGroup
}

// Run outcomes recorded in metrics, matching the chat reply outcomes: the scheduler delivered the
// report, delivered a failure report, could not deliver permanently, delivered a run that was due
// before this process started, or ended the run without a report.
const (
	runDelivered     = "delivered"
	runFailed        = "failed"
	runUndeliverable = "undeliverable"
	runRecovered     = "recovered"
	runSkipped       = "skipped"
	runQuiet         = "quiet"
)

// Telemetry records scheduled run metrics.
type Telemetry interface {
	RecordScheduleRun(ctx context.Context, principal, outcome string)
	// RecordScheduleTravelPaused counts an occurrence that moved forward unrun because its job
	// pauses while its owner travels.
	RecordScheduleTravelPaused(ctx context.Context, principal string)
	RecordReply(ctx context.Context, principal, kind, outcome, safetyChecks string, duration time.Duration)
}

// SafetyChecks reports whether a principal has opted in to the safety checks, for labelling the
// scheduled reply metrics.
type SafetyChecks interface {
	JudgmentsEnabled(ctx context.Context, principalID string) (bool, error)
}

// Travel reports whether a principal is on a trip at an instant.
type Travel interface {
	Traveling(ctx context.Context, principalID string, at time.Time) (bool, error)
}

// feed is the tab of the run's kind: reminders and Trips scans with the reminders, every other
// job's report with the reports.
func feed(run Run) string {
	if run.JobType == TypeReminder || run.JobType == TripScanType {
		return thread.FeedReminders
	}
	return thread.FeedReports
}

// inFeed returns a context whose sends go to the run's destination, or its feed tab when it has
// none. A tab that cannot be had leaves the run's messages in the main view, the resolver logs why,
// and the next delivery tries again.
func (s *Service) inFeed(ctx context.Context, run Run, chatID int64) context.Context {
	if s.feeds == nil {
		return ctx
	}
	var threadID int64
	var err error
	if run.Destination != "" {
		threadID, err = s.feeds.Destination(ctx, run.PrincipalID, chatID, run.Destination, feed(run))
	} else {
		threadID, err = s.feeds.Thread(ctx, run.PrincipalID, chatID, feed(run))
	}
	if err != nil {
		threadID = 0
	}
	return thread.WithDelivery(ctx, threadID)
}

// recordRun counts a finished run. A run that ended with a report also records the time from its
// scheduled occurrence to now, which survives a restart because the occurrence is stored.
func (s *Service) recordRun(ctx context.Context, run Run, outcome string) {
	if s.telemetry == nil {
		return
	}
	if outcome == runDelivered || outcome == runFailed {
		switch {
		case run.ScheduledFor.Before(s.startedAt):
			outcome = runRecovered
		case run.ErrorKind != "":
			outcome = runFailed
		}
	}
	s.telemetry.RecordScheduleRun(ctx, run.PrincipalID, outcome)
	if outcome == runSkipped || outcome == runQuiet {
		return
	}
	s.telemetry.RecordReply(ctx, run.PrincipalID, "scheduled", outcome, s.safetyLabel(ctx, run.PrincipalID), max(s.now().Sub(run.ScheduledFor), 0))
}

func (s *Service) safetyLabel(ctx context.Context, principalID string) string {
	if s.safety == nil {
		return "unavailable"
	}
	enabled, err := s.safety.JudgmentsEnabled(ctx, principalID)
	switch {
	case err != nil:
		return "unavailable"
	case enabled:
		return "on"
	default:
		return "off"
	}
}

// NewService builds the scheduler. A nil proposer leaves out the tools that create and remove jobs
// in conversation; /jobs still manages existing ones.
func NewService(cfg Config, repository Repository, chats Chats, deliverer conversation.Deliverer, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("schedule repository is required")
	}
	if chats == nil {
		return nil, fmt.Errorf("schedule chats are required")
	}
	if deliverer == nil {
		return nil, fmt.Errorf("schedule deliverer is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("schedule logger is required")
	}
	if (cfg.Cards == nil) != (cfg.Photos == nil) {
		return nil, fmt.Errorf("schedule cards and photos must be configured together")
	}
	if _, err := LoadZone(cfg.Zone); err != nil {
		return nil, fmt.Errorf("schedule default zone: %w", err)
	}
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository: repository,
		chats:      chats,
		deliverer:  deliverer,
		cards:      cfg.Cards,
		photos:     cfg.Photos,
		reminders:  cfg.Reminders,
		sessions:   cfg.Sessions,
		logger:     logger,
		zone:       cfg.Zone,
		tick:       tick,
		now:        now,
		startedAt:  now(),
		types:      make(map[string]JobType),
		proposer:   proposer,
		travel:     cfg.Travel,
		feeds:      cfg.Feeds,
		telemetry:  cfg.Telemetry,
		exposure:   cfg.Exposure,
		safety:     cfg.SafetyChecks,
		wake:       make(chan struct{}, 1),
		inflight:   make(map[string]bool),
		executing:  make(map[string]bool),
	}, nil
}

// Register adds a job type at wiring time, before the scheduler starts.
func (s *Service) Register(jobType JobType) error {
	if jobType.Name == "" || jobType.Handler == nil || jobType.CatchUp < 0 || jobType.MinimumInterval <= 0 {
		return fmt.Errorf("register job type: the type is incomplete")
	}
	if _, exists := s.types[jobType.Name]; exists {
		return fmt.Errorf("register job type: %q is already registered", jobType.Name)
	}
	s.types[jobType.Name] = jobType
	return nil
}

// Start reports every run a previous process left running and begins ticking.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return fmt.Errorf("scheduler is already running")
	}
	s.mu.Unlock()

	recovered, err := s.repository.RecoverScheduledRuns(ctx, InterruptedText, "interrupted", s.now())
	if err != nil {
		return fmt.Errorf("recover scheduled runs: %w", err)
	}
	if recovered > 0 {
		s.logger.InfoContext(ctx, "schedule.runs.recovered", "count", recovered)
	}

	if err := s.MigrateHeadsUp(ctx); err != nil {
		return fmt.Errorf("migrate heads-up schedules: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	s.mu.Lock()
	s.cancel, s.done = cancel, done
	s.mu.Unlock()
	go s.loop(runCtx, done)
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	finished := make(chan struct{})
	go func() {
		<-done
		s.workers.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown scheduler: %w", ctx.Err())
	}
}

// Wake asks for a tick now instead of at the next interval.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		s.runTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Service) runTick(ctx context.Context) {
	now := s.now()
	if err := s.advance(ctx, now); err != nil && ctx.Err() == nil {
		s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "advance", "error", err)
	}
	if now.Sub(s.pruned) >= pruneInterval {
		if err := s.repository.PruneScheduledRuns(ctx, now.Add(-runRetention)); err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "prune", "error", err)
		} else {
			s.pruned = now
		}
		if err := s.repository.PruneJobReports(ctx, now.Add(-reportRetention)); err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "prune_reports", "error", err)
		}
		if s.cards != nil {
			if err := s.cards.Sweep(now.Add(-conversation.CardRetention)); err != nil && ctx.Err() == nil {
				s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "sweep_cards", "error", err)
			}
		}
		if s.sessions != nil {
			if err := s.sessions.PruneSessions(ctx, now.Add(-runRetention)); err != nil && ctx.Err() == nil {
				s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "prune_sessions", "error", err)
			}
		}
	}
	runs, err := s.repository.ScheduledRuns(ctx, StatusReady, StatusQueued)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "load_runs", "error", err)
		}
		return
	}
	for _, run := range runs {
		s.dispatch(ctx, run)
	}
}

// advance queues the latest occurrence of every due job, if it is recent enough for its type's
// catch-up window, and moves the job to its next occurrence after now. A one-time job always
// queues its occurrence and finishes, so a run too late to execute can still tell its owner.
func (s *Service) advance(ctx context.Context, now time.Time) error {
	jobs, err := s.repository.DueScheduledJobs(ctx, now)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		jobType, known := s.types[job.Type]
		schedule, err := JobSchedule(job)
		if err != nil || !known {
			// A job that can no longer be understood is paused rather than retried every tick.
			if _, pauseErr := s.repository.SetScheduledJobEnabled(ctx, job.PrincipalID, job.ID, false, job.NextRunAt, now); pauseErr != nil {
				return pauseErr
			}
			s.logger.WarnContext(ctx, "schedule.job.paused", "principal_id", job.PrincipalID, "job_id", job.ID, "reason", "invalid")
			continue
		}
		latest, next := schedule.Latest(job.NextRunAt, now)
		if next.IsZero() && !job.OneTime() {
			next = now.AddDate(100, 0, 0)
		}
		var run *Run
		if !latest.IsZero() && job.TravelPause && !job.OneTime() && s.travelingAt(ctx, job, latest) {
			s.logger.InfoContext(ctx, "schedule.run.travel_paused", "principal_id", job.PrincipalID, "job_id", job.ID)
			if s.telemetry != nil {
				s.telemetry.RecordScheduleTravelPaused(ctx, job.PrincipalID)
			}
		} else if !latest.IsZero() && (job.OneTime() || now.Sub(latest) <= jobType.CatchUp) {
			id, err := newRunID()
			if err != nil {
				return err
			}
			run = &Run{ID: id, JobID: job.ID, PrincipalID: job.PrincipalID, ScheduledFor: latest, Status: StatusQueued}
		} else {
			s.logger.InfoContext(ctx, "schedule.run.missed", "principal_id", job.PrincipalID, "job_id", job.ID)
		}
		if _, err := s.repository.AdvanceScheduledJob(ctx, job.ID, job.NextRunAt, next, run); err != nil {
			return err
		}
	}
	return nil
}

// travelingAt reports whether the job's owner is on a trip at the occurrence. A failed lookup
// counts as not traveling, so a reminder is never lost to it.
func (s *Service) travelingAt(ctx context.Context, job Job, at time.Time) bool {
	if s.travel == nil {
		return false
	}
	traveling, err := s.travel.Traveling(ctx, job.PrincipalID, at)
	if err != nil {
		s.logger.WarnContext(ctx, "schedule.travel.failed", "principal_id", job.PrincipalID, "job_id", job.ID, "error", err)
		return false
	}
	return traveling
}

// dispatch starts one run in the background unless it is already in flight or, for a run still to
// execute that asks the model, the household's concurrent-run bound is reached or its owner already
// has one executing. A principal's scheduled turns wait behind their chat and each other, so a
// second one would only hold a slot another principal's run could use.
func (s *Service) dispatch(ctx context.Context, run Run) {
	s.mu.Lock()
	executing := run.Status == StatusQueued
	bounded := executing && !s.types[run.JobType].Light
	if s.inflight[run.ID] || (bounded && (s.running >= maximumConcurrentRuns || s.executing[run.PrincipalID])) {
		s.mu.Unlock()
		return
	}
	s.inflight[run.ID] = true
	if bounded {
		s.running++
		s.executing[run.PrincipalID] = true
	}
	s.workers.Add(1)
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, run.ID)
			s.mu.Unlock()
			s.workers.Done()
		}()
		if executing {
			var ok bool
			run, ok = s.execute(ctx, run)
			if bounded {
				s.releaseSlot(run.PrincipalID, ok)
			}
			if !ok {
				return
			}
		} else {
			current, ready := s.stillReady(ctx, run)
			if !ready {
				return
			}
			run = current
		}
		if !executing {
			// Whether a run executed before a restart or a failed delivery read private content
			// is unknown; only a reminder's text is known not to retell any, or to be outside
			// content.
			run.Private = run.JobType != TypeReminder
			run.External = run.JobType != TypeReminder
		}
		s.deliver(ctx, run)
	}()
}

// releaseSlot frees an executed run's place under the bounds before its report is delivered. After
// a report it asks for a tick, so a run held back by the bounds starts now rather than at the next
// interval. A run that failed to execute is left to the next interval, since an immediate tick
// would only retry it into the same failure.
func (s *Service) releaseSlot(principalID string, reported bool) {
	s.mu.Lock()
	s.running--
	delete(s.executing, principalID)
	s.mu.Unlock()
	if reported {
		s.Wake()
	}
}

// stillReady rereads a ready run once it is claimed and returns it as stored. The tick's snapshot
// can predate a delivery that finished or sent some of its chunks in the meantime, and delivering
// from it would send them twice.
func (s *Service) stillReady(ctx context.Context, run Run) (Run, bool) {
	ready, err := s.repository.ScheduledRuns(ctx, StatusReady)
	if err != nil {
		s.logRunError(ctx, run, "reload_run", err)
		return run, false
	}
	index := slices.IndexFunc(ready, func(current Run) bool { return current.ID == run.ID })
	if index < 0 {
		return run, false
	}
	return ready[index], true
}

// execute runs one queued occurrence and stores its reply, returning the ready run.
func (s *Service) execute(ctx context.Context, run Run) (Run, bool) {
	now := s.now()
	jobType, known := s.types[run.JobType]
	if !known {
		s.skip(ctx, run, StatusQueued, "unknown_type")
		return run, false
	}
	missed := !run.Manual && now.Sub(run.ScheduledFor) > jobType.CatchUp
	if missed && !run.JobOneTime {
		s.skip(ctx, run, StatusQueued, "stale")
		return run, false
	}
	chatID, found, err := s.chats.ResolveActiveChat(ctx, run.PrincipalID)
	if err != nil {
		s.logRunError(ctx, run, "resolve_chat", err)
		return run, false
	} else if !found {
		s.skip(ctx, run, StatusQueued, "no_chat")
		return run, false
	}
	location, err := LoadZone(run.JobZone)
	if err != nil {
		location, _ = LoadZone(s.zone)
	}
	started, err := s.repository.TransitionScheduledRun(ctx, run.ID, StatusQueued, StatusRunning, "", "", now)
	if err != nil || !started {
		if err != nil {
			s.logRunError(ctx, run, "start_run", err)
		}
		return run, false
	}

	began := time.Now()
	result := Result{Text: MissedText, ErrorKind: "missed"}
	if !missed {
		// Files and pictures the run sends go to the tab its report will.
		result, err = jobType.Handler.Run(s.inFeed(ctx, run, chatID), Occurrence{
			RunID:        run.ID,
			PrincipalID:  run.PrincipalID,
			JobID:        run.JobID,
			JobName:      run.JobName,
			Config:       run.Config,
			ScheduledFor: run.ScheduledFor,
			Location:     location,
			Manual:       run.Manual,
		})
	}
	if ctx.Err() != nil {
		return run, false
	}
	if err != nil {
		result = Result{Text: FailedText, ErrorKind: "handler"}
	}
	if result.Quiet && result.ErrorKind == "" {
		if !run.Manual {
			s.skip(ctx, run, StatusRunning, quietReason)
			return run, false
		}
		result = Result{Text: QuietManualText}
	}
	if strings.TrimSpace(result.Text) == "" {
		result = Result{Text: FailedText, ErrorKind: "empty"}
	}
	saved, err := s.repository.TransitionScheduledRun(ctx, run.ID, StatusRunning, StatusReady, result.Text, result.ErrorKind, s.now())
	if err != nil || !saved {
		if err != nil {
			s.logRunError(ctx, run, "store_reply", err)
		}
		return run, false
	}
	outcome := "success"
	if result.ErrorKind != "" {
		outcome = "error"
	}
	s.logger.InfoContext(ctx, "schedule.run.completed",
		"principal_id", run.PrincipalID, "job_id", run.JobID, "job_type", run.JobType,
		"manual", run.Manual, "outcome", outcome, "error_kind", result.ErrorKind,
		"duration_ms", time.Since(began).Milliseconds(),
	)
	run.Status, run.Reply, run.ErrorKind, run.Private, run.External = StatusReady, result.Text, result.ErrorKind, result.Private, result.External
	return run, true
}

// deliver sends a ready run to its owner's own chat under the job's name. A transient failure
// leaves the run ready for the next tick; a permanent one, or an owner who no longer has a chat,
// ends it.
func (s *Service) deliver(ctx context.Context, run Run) {
	chatID, found, err := s.chats.ResolveActiveChat(ctx, run.PrincipalID)
	if err != nil {
		s.logRunError(ctx, run, "resolve_chat", err)
		return
	}
	if !found {
		s.skip(ctx, run, StatusReady, "no_chat")
		return
	}
	sendCtx := s.inFeed(ctx, run, chatID)
	if run.JobType == TypeReminder && s.reminders != nil && run.ErrorKind == "" {
		messageID, threadID, err := s.reminders.SendReminder(sendCtx, chatID, run.JobID, deliveryText(run))
		s.finishDelivery(ctx, run, []Sent{{MessageID: messageID, ThreadID: threadID}}, err)
		return
	}
	chunks := conversation.SplitReply(deliveryText(run))
	// The card goes before the first chunk, so a delivery that already sent a chunk is past it. A
	// failed run's text explains the failure, so a card it staged would illustrate nothing.
	sent := min(run.DeliveredChunks, len(chunks))
	if sent == 0 && s.cards != nil && run.ErrorKind == "" {
		remaining, _, err := conversation.SendCard(sendCtx, s.logger, s.cards, s.photos, run.PrincipalID, outbox.ScheduledTurn(run.ID), chatID, 0, chunks)
		if err != nil {
			if ctx.Err() == nil {
				s.logRunError(ctx, run, "deliver", err)
			}
			return
		}
		sent = len(chunks) - len(remaining)
	}
	messages := slices.Clone(run.Delivered)
	for index := sent; index < len(chunks); index++ {
		messageID, threadID, err := s.deliverer.SendTrackedReply(sendCtx, chatID, 0, chunks[index])
		if err != nil {
			s.finishDelivery(ctx, run, nil, err)
			return
		}
		messages = append(messages, Sent{MessageID: messageID, ThreadID: threadID})
		storeCtx, cancel := deliveryStoreContext(ctx)
		if err := s.repository.SaveScheduledRunProgress(storeCtx, run.ID, index+1, messages, s.now()); err != nil {
			s.logRunError(ctx, run, "store_progress", err)
		}
		cancel()
	}
	s.finishDelivery(ctx, run, messages, nil)
}

// deliveryStoreContext records what a delivery already sent. It outlives a shutdown that has begun,
// because a send that went through but was not recorded is sent again by the next process.
func deliveryStoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), deliveryStoreTimeout)
}

// finishDelivery records how sending a ready run ended, keeping a delivered run's report with the
// messages it was sent as. A transient failure leaves it ready.
func (s *Service) finishDelivery(ctx context.Context, run Run, messages []Sent, err error) {
	if err != nil {
		if isPermanent(err) {
			if _, err := s.repository.TransitionScheduledRun(ctx, run.ID, StatusReady, StatusFailed, "", "undeliverable", s.now()); err != nil {
				s.logRunError(ctx, run, "store_delivery", err)
			} else {
				s.removeCard(run)
				s.recordRun(ctx, run, runUndeliverable)
			}
			s.logger.WarnContext(ctx, "schedule.run.undeliverable", "principal_id", run.PrincipalID, "job_id", run.JobID)
			return
		}
		if ctx.Err() == nil {
			s.logRunError(ctx, run, "deliver", err)
		}
		return
	}
	storeCtx, cancel := deliveryStoreContext(ctx)
	defer cancel()
	// The report is kept before the run is marked delivered, which clears its text, so a crash
	// between the two cannot lose it.
	s.saveReport(storeCtx, run, slices.DeleteFunc(messages, func(message Sent) bool { return message.MessageID <= 0 }))
	if _, err := s.repository.TransitionScheduledRun(storeCtx, run.ID, StatusReady, StatusDelivered, "", run.ErrorKind, s.now()); err != nil {
		s.logRunError(ctx, run, "store_delivery", err)
		return
	}
	s.removeCard(run)
	s.recordRun(ctx, run, runDelivered)
	s.logger.InfoContext(ctx, "schedule.run.delivered", "principal_id", run.PrincipalID, "job_id", run.JobID)
}

func deliveryText(run Run) string {
	if run.JobType == TypeReminder && run.ErrorKind == "" {
		return "⏰ " + reportText(run)
	}
	return "**Scheduled: " + run.JobName + "**\n\n" + reportText(run)
}

// reportText is what a run tells its owner, without the heading its message carries.
func reportText(run Run) string {
	if run.JobOneTime && run.ErrorKind == "interrupted" {
		return OneTimeInterruptedText
	}
	return strings.TrimSpace(run.Reply)
}

func (s *Service) skip(ctx context.Context, run Run, from, reason string) {
	if _, err := s.repository.TransitionScheduledRun(ctx, run.ID, from, StatusSkipped, "", reason, s.now()); err != nil {
		s.logRunError(ctx, run, "store_skip", err)
		return
	}
	s.removeCard(run)
	outcome := runSkipped
	if reason == quietReason {
		outcome = runQuiet
	}
	s.recordRun(ctx, run, outcome)
	s.logger.InfoContext(ctx, "schedule.run.skipped", "principal_id", run.PrincipalID, "job_id", run.JobID, "reason", reason)
}

func (s *Service) removeCard(run Run) {
	if s.cards != nil {
		s.cards.Remove(run.PrincipalID, outbox.ScheduledTurn(run.ID))
	}
}

func (s *Service) logRunError(ctx context.Context, run Run, stage string, err error) {
	s.logger.WarnContext(ctx, "schedule.run.retry", "principal_id", run.PrincipalID, "job_id", run.JobID, "stage", stage, "error", err)
}

// Jobs lists the principal's own jobs.
func (s *Service) Jobs(ctx context.Context, principalID string) ([]Job, error) {
	return s.repository.ListScheduledJobs(ctx, principalID)
}

// SetEnabled pauses or resumes one of the principal's jobs. A resumed job continues from its next
// occurrence after now; nothing missed while it was paused runs.
func (s *Service) SetEnabled(ctx context.Context, principalID, jobID string, enabled bool) (bool, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, jobID)
	if err != nil || !found {
		return false, err
	}
	now := s.now()
	next := job.NextRunAt
	if enabled {
		schedule, err := JobSchedule(job)
		if err != nil {
			return false, err
		}
		if next = schedule.Next(now); next.IsZero() {
			return false, fmt.Errorf("the schedule never runs again")
		}
	}
	changed, err := s.repository.SetScheduledJobEnabled(ctx, principalID, jobID, enabled, next, now)
	if err == nil && changed {
		s.logger.InfoContext(ctx, "schedule.job.changed", "principal_id", principalID, "job_id", jobID, "enabled", enabled)
	}
	return changed, err
}

// SetTravelPause flags or unflags one of the principal's recurring jobs to stay silent while the
// principal is traveling.
func (s *Service) SetTravelPause(ctx context.Context, principalID, jobID string, on bool) (bool, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, jobID)
	if err != nil || !found || !TravelPausable(job) || !job.FinishedAt.IsZero() {
		return false, err
	}
	changed, err := s.repository.SetScheduledJobTravelPause(ctx, principalID, jobID, on)
	if err == nil && changed {
		s.logger.InfoContext(ctx, "schedule.job.changed", "principal_id", principalID, "job_id", jobID, "travel_pause", on)
	}
	return changed, err
}

// SetDestination gives one of the principal's jobs a destination when it has none, and reports
// whether it did. Nothing else about the job changes.
func (s *Service) SetDestination(ctx context.Context, principalID, jobID, destination string) (bool, error) {
	changed, err := s.repository.SetScheduledJobDestination(ctx, principalID, jobID, destination)
	if err == nil && changed {
		s.logger.InfoContext(ctx, "schedule.job.changed", "principal_id", principalID, "job_id", jobID, "destination", true)
	}
	return changed, err
}

// TravelPausedNames returns the names of the principal's enabled recurring jobs flagged to pause
// while traveling.
func (s *Service) TravelPausedNames(ctx context.Context, principalID string) ([]string, error) {
	jobs, err := s.repository.ListScheduledJobs(ctx, principalID)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, job := range jobs {
		if job.TravelPause && job.Enabled && !job.OneTime() {
			names = append(names, job.Name)
		}
	}
	return names, nil
}

func (s *Service) Delete(ctx context.Context, principalID, jobID string) (bool, error) {
	deleted, err := s.repository.DeleteScheduledJob(ctx, principalID, jobID)
	if err == nil && deleted {
		s.logger.InfoContext(ctx, "schedule.job.deleted", "principal_id", principalID, "job_id", jobID)
	}
	return deleted, err
}

// RunNow queues one run of the principal's job outside its schedule, leaving the schedule alone.
// A non-empty trigger names the request, such as a button press, so a retried request queues no
// second run.
func (s *Service) RunNow(ctx context.Context, principalID, jobID, trigger string) (bool, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, jobID)
	if err != nil || !found {
		return false, err
	}
	id, err := newRunID()
	if err != nil {
		return false, err
	}
	if trigger != "" {
		digest := sha256.Sum256([]byte(job.ID + "\x00" + trigger))
		id = hex.EncodeToString(digest[:8])
	}
	if err := s.repository.QueueScheduledRun(ctx, Run{
		ID: id, JobID: job.ID, PrincipalID: principalID, ScheduledFor: s.now(), Manual: true, Status: StatusQueued,
	}); err != nil {
		return false, err
	}
	s.Wake()
	return true, nil
}

// Busy is true while the principal's job has a run queued or running.
func (s *Service) Busy(ctx context.Context, principalID, jobID string) (bool, error) {
	runs, err := s.repository.ScheduledRuns(ctx, StatusQueued, StatusRunning)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.PrincipalID == principalID && run.JobID == jobID {
			return true, nil
		}
	}
	return false, nil
}

// Prepared is a validated job ready to be proposed or stored, with its parsed schedule.
type Prepared struct {
	Job      Job
	Schedule Schedule
}

// Timing is when a job runs: a five-field cron expression, or At, a local date and time such as
// 2026-10-04 08:30 for a job that runs once. Zone is empty for the default zone.
type Timing struct {
	Cron string
	At   string
	Zone string
}

// Prepare validates a job the principal asked for against its type's rules. It is used both when
// the job is proposed and again when the confirmed proposal is executed.
func (s *Service) Prepare(principalID, name, jobType string, timing Timing, config json.RawMessage) (Prepared, error) {
	registered, known := s.types[jobType]
	if !known {
		return Prepared{}, fmt.Errorf("that kind of job is not available")
	}
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maximumNameRunes || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return Prepared{}, fmt.Errorf("the name must be a single line of at most %d characters", maximumNameRunes)
	}
	zone := strings.TrimSpace(timing.Zone)
	if zone == "" {
		zone = s.zone
	}
	now := s.now()
	var schedule Schedule
	var err error
	switch hasCron, hasAt := strings.TrimSpace(timing.Cron) != "", strings.TrimSpace(timing.At) != ""; {
	case hasCron == hasAt:
		return Prepared{}, fmt.Errorf("give either a cron expression for a recurring job or a date and time for a one-time job")
	case hasAt:
		if schedule, err = ParseOnce(timing.At, zone, now); err != nil {
			return Prepared{}, err
		}
	case registered.Once:
		return Prepared{}, fmt.Errorf("this kind of job runs once; give a date and time instead of a cron expression")
	default:
		if schedule, err = ParseSchedule(timing.Cron, zone); err != nil {
			return Prepared{}, err
		}
		if err := schedule.CheckInterval(now, registered.MinimumInterval); err != nil {
			return Prepared{}, err
		}
	}
	if err := registered.Handler.Validate(config); err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Job: Job{
			PrincipalID: principalID,
			Name:        name,
			Type:        jobType,
			Cron:        schedule.Expression,
			RunAt:       schedule.Once,
			Zone:        zone,
			Config:      config,
			Enabled:     true,
			NextRunAt:   schedule.Next(now),
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		Schedule: schedule,
	}, nil
}

// Create stores a prepared job under the given ID.
func (s *Service) Create(ctx context.Context, id string, prepared Prepared) (bool, error) {
	job := prepared.Job
	job.ID = id
	created, err := s.repository.CreateScheduledJob(ctx, job, job.Limit())
	if err == nil && created {
		s.logger.InfoContext(ctx, "schedule.job.created", "principal_id", job.PrincipalID, "job_id", id, "job_type", job.Type)
	}
	return created, err
}

// Update replaces one of the principal's jobs with a prepared one under the same ID, keeping its
// pause state, and its destination unless the prepared job names one. The prepared job's
// TravelPause is stored, so a caller that does not mean to change it copies the current one. A job
// changed since expectedUpdatedAt is reported as not updated.
func (s *Service) Update(ctx context.Context, current Job, expectedUpdatedAt time.Time, prepared Prepared) (bool, error) {
	job := prepared.Job
	job.ID, job.Enabled, job.CreatedAt = current.ID, current.Enabled, current.CreatedAt
	if job.Destination == "" {
		job.Destination = current.Destination
	}
	updated, err := s.repository.UpdateScheduledJob(ctx, job, expectedUpdatedAt)
	if err == nil && updated {
		s.logger.InfoContext(ctx, "schedule.job.updated", "principal_id", job.PrincipalID, "job_id", job.ID, "job_type", job.Type)
	}
	return updated, err
}

// Snooze is how far a delivered reminder is moved.
type Snooze string

const (
	SnoozeTenMinutes Snooze = "10m"
	SnoozeHour       Snooze = "1h"
	// SnoozeTomorrow moves the reminder to its own local time on the next day that is still ahead.
	SnoozeTomorrow Snooze = "tomorrow"
)

// Snooze makes one of the principal's delivered reminders pending again. A reminder that was
// cancelled, pruned, or is already pending is reported as not snoozed, so a second press of the
// same button cannot move it twice.
func (s *Service) Snooze(ctx context.Context, principalID, jobID string, snooze Snooze) (Job, bool, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, jobID)
	if err != nil || !found || job.Type != TypeReminder || job.FinishedAt.IsZero() {
		return Job{}, false, err
	}
	location, err := LoadZone(job.Zone)
	if err != nil {
		return Job{}, false, err
	}
	now := s.now()
	var at time.Time
	switch snooze {
	case SnoozeTenMinutes:
		at = now.Add(10 * time.Minute)
	case SnoozeHour:
		at = now.Add(time.Hour)
	case SnoozeTomorrow:
		local := job.RunAt.In(location)
		for days := 1; !at.After(now); days++ {
			at = time.Date(local.Year(), local.Month(), local.Day()+days, local.Hour(), local.Minute(), 0, 0, location)
		}
	default:
		return Job{}, false, fmt.Errorf("unknown snooze %q", snooze)
	}
	at = at.Truncate(time.Second).UTC()
	revived, err := s.repository.ReviveScheduledJob(ctx, principalID, jobID, TypeReminder, at, now)
	if err != nil || !revived {
		return Job{}, false, err
	}
	s.logger.InfoContext(ctx, "schedule.reminder.snoozed", "principal_id", principalID, "job_id", jobID, "snooze", string(snooze))
	job.RunAt, job.NextRunAt, job.FinishedAt = at, at, time.Time{}
	return job, true, nil
}

// DefaultZone is the zone a job gets when none is named.
func (s *Service) DefaultZone() string { return s.zone }

// Now is the scheduler's clock.
func (s *Service) Now() time.Time { return s.now() }

func isPermanent(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
}

func newRunID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate run identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
