package schedule

import (
	"context"
	"crypto/rand"
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
)

const (
	DefaultTickInterval = 30 * time.Second

	maximumConcurrentRuns = 2
	maximumNameRunes      = 60
	maximumNoticeRunes    = 2000
	runRetention          = 30 * 24 * time.Hour
	pruneInterval         = time.Hour

	InterruptedText = "This run was interrupted by a restart and was not retried. It will run again at its next scheduled time."
	FailedText      = "This run could not be completed."
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
}

// ReminderDeliverer sends one reminder to its owner's chat with buttons that snooze the job.
type ReminderDeliverer interface {
	SendReminder(ctx context.Context, chatID int64, jobID, text string) error
}

type Service struct {
	repository Repository
	chats      Chats
	deliverer  conversation.Deliverer
	cards      conversation.Cards
	photos     conversation.PhotoDeliverer
	reminders  ReminderDeliverer
	logger     *slog.Logger
	zone       string
	tick       time.Duration
	now        func() time.Time
	types      map[string]JobType
	proposer   Proposer
	telemetry  Telemetry
	safety     SafetyChecks
	startedAt  time.Time

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	wake     chan struct{}
	inflight map[string]bool
	running  int
	notices  map[string][]string
	pruned   time.Time
	workers  sync.WaitGroup
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
)

// Telemetry records scheduled run metrics.
type Telemetry interface {
	RecordScheduleRun(ctx context.Context, principal, outcome string)
	RecordReply(ctx context.Context, principal, kind, outcome, safetyChecks string, duration time.Duration)
}

// SafetyChecks reports whether a principal has opted in to the safety checks, for labelling the
// scheduled reply metrics.
type SafetyChecks interface {
	JudgmentsEnabled(ctx context.Context, principalID string) (bool, error)
}

// SetTelemetry sets where run metrics are recorded, at wiring time before the scheduler starts.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	s.telemetry = telemetry
}

// SetSafetyChecks sets the opt-in lookup that labels scheduled reply metrics; without it they
// are labelled "unavailable".
func (s *Service) SetSafetyChecks(checks SafetyChecks) {
	s.safety = checks
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
	if outcome == runSkipped {
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
		logger:     logger,
		zone:       cfg.Zone,
		tick:       tick,
		now:        now,
		startedAt:  now(),
		types:      make(map[string]JobType),
		proposer:   proposer,
		wake:       make(chan struct{}, 1),
		inflight:   make(map[string]bool),
		notices:    make(map[string][]string),
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
		if s.cards != nil {
			if err := s.cards.Sweep(now.Add(-conversation.CardRetention)); err != nil && ctx.Err() == nil {
				s.logger.WarnContext(ctx, "schedule.tick.failed", "stage", "sweep_cards", "error", err)
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
		if !latest.IsZero() && (job.OneTime() || now.Sub(latest) <= jobType.CatchUp) {
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

// dispatch starts one run in the background unless it is already in flight or, for a run still to
// execute, the household's concurrent-run bound is reached.
func (s *Service) dispatch(ctx context.Context, run Run) {
	s.mu.Lock()
	if s.inflight[run.ID] || (run.Status == StatusQueued && s.running >= maximumConcurrentRuns) {
		s.mu.Unlock()
		return
	}
	s.inflight[run.ID] = true
	executing := run.Status == StatusQueued
	if executing {
		s.running++
	}
	s.workers.Add(1)
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, run.ID)
			if executing {
				s.running--
			}
			s.mu.Unlock()
			s.workers.Done()
		}()
		if executing {
			var ok bool
			if run, ok = s.execute(ctx, run); !ok {
				return
			}
		} else if !s.stillReady(ctx, run) {
			return
		}
		s.deliver(ctx, run)
	}()
}

// stillReady rereads a ready run once it is claimed. The tick's snapshot can predate a delivery
// that finished in the meantime, and delivering from it would send the report twice.
func (s *Service) stillReady(ctx context.Context, run Run) bool {
	ready, err := s.repository.ScheduledRuns(ctx, StatusReady)
	if err != nil {
		s.logRunError(ctx, run, "reload_run", err)
		return false
	}
	return slices.ContainsFunc(ready, func(current Run) bool { return current.ID == run.ID })
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
	if _, found, err := s.chats.ResolveActiveChat(ctx, run.PrincipalID); err != nil {
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
		result, err = jobType.Handler.Run(ctx, Occurrence{
			RunID:        run.ID,
			PrincipalID:  run.PrincipalID,
			JobID:        run.JobID,
			JobName:      run.JobName,
			Config:       run.Config,
			ScheduledFor: run.ScheduledFor,
			Location:     location,
		})
	}
	if ctx.Err() != nil {
		return run, false
	}
	if err != nil {
		result = Result{Text: FailedText, ErrorKind: "handler"}
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
	run.Status, run.Reply, run.ErrorKind = StatusReady, result.Text, result.ErrorKind
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
	if run.JobType == TypeReminder && s.reminders != nil && run.ErrorKind == "" {
		s.finishDelivery(ctx, run, s.reminders.SendReminder(ctx, chatID, run.JobID, deliveryText(run)))
		return
	}
	chunks := conversation.SplitReply(deliveryText(run))
	// A failed run's text explains the failure, so a card it staged would illustrate nothing.
	if s.cards != nil && run.ErrorKind == "" {
		chunks, _, err = conversation.SendCard(ctx, s.logger, s.cards, s.photos, run.PrincipalID, outbox.ScheduledTurn(run.ID), chatID, 0, chunks)
		if err != nil {
			if ctx.Err() == nil {
				s.logRunError(ctx, run, "deliver", err)
			}
			return
		}
	}
	for _, chunk := range chunks {
		if err := s.deliverer.SendReply(ctx, chatID, 0, chunk); err != nil {
			s.finishDelivery(ctx, run, err)
			return
		}
	}
	s.finishDelivery(ctx, run, nil)
}

// finishDelivery records how sending a ready run ended. A transient failure leaves it ready.
func (s *Service) finishDelivery(ctx context.Context, run Run, err error) {
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
	if _, err := s.repository.TransitionScheduledRun(ctx, run.ID, StatusReady, StatusDelivered, "", run.ErrorKind, s.now()); err != nil {
		s.logRunError(ctx, run, "store_delivery", err)
		return
	}
	s.removeCard(run)
	s.remember(run)
	s.recordRun(ctx, run, runDelivered)
	s.logger.InfoContext(ctx, "schedule.run.delivered", "principal_id", run.PrincipalID, "job_id", run.JobID)
}

func deliveryText(run Run) string {
	if run.JobOneTime && run.ErrorKind == "interrupted" {
		run.Reply = OneTimeInterruptedText
	}
	if run.JobType == TypeReminder && run.ErrorKind == "" {
		return "⏰ " + strings.TrimSpace(run.Reply)
	}
	return "**Scheduled: " + run.JobName + "**\n\n" + strings.TrimSpace(run.Reply)
}

func (s *Service) skip(ctx context.Context, run Run, from, reason string) {
	if _, err := s.repository.TransitionScheduledRun(ctx, run.ID, from, StatusSkipped, "", reason, s.now()); err != nil {
		s.logRunError(ctx, run, "store_skip", err)
		return
	}
	s.removeCard(run)
	s.recordRun(ctx, run, runSkipped)
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

// remember keeps the delivered report for the owner's next conversation turn, so a follow-up
// question about it has something to refer to.
func (s *Service) remember(run Run) {
	report := truncateRunes(strings.TrimSpace(run.Reply), maximumNoticeRunes)
	notice := fmt.Sprintf("Your scheduled job %q just sent the user this report:\n%s", run.JobName, report)
	if run.JobType == TypeReminder {
		notice = fmt.Sprintf("The user's reminder (job id %s) was just delivered to them:\n%s", run.JobID, report)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notices := append(s.notices[run.PrincipalID], notice)
	if len(notices) > 2 {
		notices = notices[len(notices)-2:]
	}
	s.notices[run.PrincipalID] = notices
}

// PendingNotice returns and forgets the reports delivered since the principal's last turn.
func (s *Service) PendingNotice(principalID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	notices := s.notices[principalID]
	delete(s.notices, principalID)
	return strings.Join(notices, "\n\n")
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

func (s *Service) Delete(ctx context.Context, principalID, jobID string) (bool, error) {
	deleted, err := s.repository.DeleteScheduledJob(ctx, principalID, jobID)
	if err == nil && deleted {
		s.logger.InfoContext(ctx, "schedule.job.deleted", "principal_id", principalID, "job_id", jobID)
	}
	return deleted, err
}

// RunNow queues one run of the principal's job outside its schedule, leaving the schedule alone.
func (s *Service) RunNow(ctx context.Context, principalID, jobID string) (bool, error) {
	job, found, err := s.repository.LoadScheduledJob(ctx, principalID, jobID)
	if err != nil || !found {
		return false, err
	}
	id, err := newRunID()
	if err != nil {
		return false, err
	}
	if err := s.repository.QueueScheduledRun(ctx, Run{
		ID: id, JobID: job.ID, PrincipalID: principalID, ScheduledFor: s.now(), Manual: true, Status: StatusQueued,
	}); err != nil {
		return false, err
	}
	s.Wake()
	return true, nil
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

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}
