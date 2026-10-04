package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	DefaultTimeout          = 60 * time.Minute
	DefaultGrace            = 3 * time.Minute
	DefaultToolBudget       = 100
	DefaultModelBudget      = 150
	DefaultProgressInterval = time.Minute

	// maximumOpen is one running task and three queued behind it.
	maximumOpen = 4
	// maximumAttempts is how often a task's run may start; a task interrupted that often fails.
	maximumAttempts   = 2
	maximumTitleRunes = 80
	maximumBriefRunes = 4000
	maximumListed     = 10
	maximumNoticeRune = 600
	maximumNoticeLine = 5

	listWindow    = 7 * 24 * time.Hour
	noticeWindow  = 24 * time.Hour
	retention     = 30 * 24 * time.Hour
	retryDelay    = 5 * time.Second
	noticeTimeout = 2 * time.Second
	saveTimeout   = 5 * time.Second

	// webSearchTool is the runtime's own search, which the broker's budget does not count.
	webSearchTool = "web_search"

	InterruptedTwiceText = "This background task was interrupted by a restart twice, so I stopped it. Ask again if you still need it."
	LimitText            = "This background task reached its limit and could not write a report in time."

	resumeMessage = "[system] You were interrupted; continue the task and report when it is complete."
)

var (
	errCancelled   = errors.New("background task cancelled")
	errModelBudget = errors.New("background task reached its model call budget")
)

// Runner is the conversation service as a background task sees it: one run of the task's own
// session beside the chat.
type Runner interface {
	RunTask(ctx context.Context, run conversation.TaskRun) (conversation.TaskReply, error)
}

type Config struct {
	// Timeout caps a task's wall time from its first start; restarts do not extend it.
	Timeout time.Duration
	// Grace is how long a task that reached a limit has to write what it has.
	Grace time.Duration
	// ToolBudget caps a task's broker tool calls and ModelBudget its model calls.
	ToolBudget  int
	ModelBudget int
	// ProgressInterval is how often the status message is updated while a task runs.
	ProgressInterval time.Duration
	// Cards and Photos send the card a task staged with its report; nil leaves reports text only.
	Cards  conversation.Cards
	Photos conversation.PhotoDeliverer
}

type Service struct {
	config     Config
	repository Repository
	chats      Chats
	messenger  Messenger
	deliverer  conversation.Deliverer
	logger     *slog.Logger
	runner     Runner
	telemetry  Telemetry
	proposer   Proposer
	exposure   ExposureChecker

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	workers map[string]chan struct{}
	running map[string]context.CancelCauseFunc
	wg      sync.WaitGroup
}

func NewService(cfg Config, repository Repository, chats Chats, messenger Messenger, deliverer conversation.Deliverer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("task repository is required")
	}
	if chats == nil {
		return nil, fmt.Errorf("task chats are required")
	}
	if messenger == nil {
		return nil, fmt.Errorf("task messenger is required")
	}
	if deliverer == nil {
		return nil, fmt.Errorf("task deliverer is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("task logger is required")
	}
	if (cfg.Cards == nil) != (cfg.Photos == nil) {
		return nil, fmt.Errorf("task cards and photos must be configured together")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.ToolBudget <= 0 {
		cfg.ToolBudget = DefaultToolBudget
	}
	if cfg.ModelBudget <= 0 {
		cfg.ModelBudget = DefaultModelBudget
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = DefaultProgressInterval
	}
	return &Service{
		config:     cfg,
		repository: repository,
		chats:      chats,
		messenger:  messenger,
		deliverer:  deliverer,
		logger:     logger,
		telemetry:  noTelemetry{},
		workers:    make(map[string]chan struct{}),
		running:    make(map[string]context.CancelCauseFunc),
	}, nil
}

// SetRunner sets what runs a task's model turns, at wiring time before Start; the conversation
// service is built after this one because its turns read this one's notices.
func (s *Service) SetRunner(runner Runner) {
	s.runner = runner
}

// SetProposer sets who records task proposals, at wiring time; without it a task started after
// outside content was read is refused.
func (s *Service) SetProposer(proposer Proposer) {
	s.proposer = proposer
}

// SetExposure sets the check that makes task_start wait for confirmation once the conversation
// has read outside content, at wiring time; without it tasks always start at once.
func (s *Service) SetExposure(exposure ExposureChecker) {
	s.exposure = exposure
}

// SetTelemetry sets where task runs are measured, at wiring time before Start.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	if telemetry != nil {
		s.telemetry = telemetry
	}
}

// Start fails what was interrupted too often, then resumes every principal's open work: reports
// still to deliver, interrupted runs, and queued tasks.
func (s *Service) Start(ctx context.Context) error {
	if s.runner == nil {
		return fmt.Errorf("task runner is required")
	}
	s.mu.Lock()
	if s.ctx != nil {
		s.mu.Unlock()
		return fmt.Errorf("task service is already running")
	}
	s.mu.Unlock()

	now := time.Now()
	if err := s.repository.PruneTasks(ctx, now.Add(-retention)); err != nil {
		s.logger.WarnContext(ctx, "task.prune.failed", "error", err)
	}
	principals, err := s.repository.RecoverTasks(ctx, maximumAttempts, InterruptedTwiceText, now)
	if err != nil {
		return fmt.Errorf("recover background tasks: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.ctx, s.cancel = runCtx, cancel
	s.mu.Unlock()
	for _, principalID := range principals {
		s.notify(principalID)
	}
	return nil
}

// Shutdown stops every run. A running task stays running in the store and resumes on the next
// start.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.mu.Lock()
		s.ctx = nil
		s.workers = make(map[string]chan struct{})
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown task service: %w", ctx.Err())
	}
}

// Create queues a task for the principal, started from the given conversation request, and posts
// its status message as a reply to that request's message.
func (s *Service) Create(ctx context.Context, principalID, requestID, title, brief string) (Task, error) {
	title, brief, err := validateBrief(title, brief)
	if err != nil {
		return Task{}, err
	}
	chatID, messageID, found, err := s.repository.TaskOrigin(ctx, principalID, requestID)
	if err != nil {
		return Task{}, err
	}
	if !found {
		return Task{}, inputError("The message that asked for the task is unknown.")
	}
	id, err := newTaskID()
	if err != nil {
		return Task{}, err
	}
	sessionID, err := newSessionID()
	if err != nil {
		return Task{}, err
	}
	task := Task{
		ID: id, PrincipalID: principalID, ChatID: chatID, RequestMessageID: messageID,
		Title: title, Brief: brief, SessionID: sessionID, Status: StatusQueued, CreatedAt: time.Now(),
	}
	ahead, err := s.repository.CreateTask(ctx, task, maximumOpen)
	if err != nil {
		return Task{}, err
	}
	s.logger.InfoContext(ctx, "task.created", "principal_id", principalID, "task_id", id, "queued_behind", ahead)
	text := StatusText(task, task.CreatedAt)
	if ahead == 0 {
		text = "Starting: " + title
	}
	if statusID, err := s.messenger.SendStatus(ctx, chatID, messageID, text, id); err != nil {
		s.logger.WarnContext(ctx, "task.status.failed", "principal_id", principalID, "task_id", id, "stage", "send", "error", err)
	} else if err := s.repository.SetTaskStatusMessage(ctx, id, statusID); err != nil {
		s.logger.WarnContext(ctx, "task.status.failed", "principal_id", principalID, "task_id", id, "stage", "store", "error", err)
	} else {
		task.StatusMessageID = statusID
	}
	s.notify(principalID)
	return task, nil
}

// validateBrief trims and checks a task's title and brief.
func validateBrief(title, brief string) (string, string, error) {
	title, brief = strings.TrimSpace(title), strings.TrimSpace(brief)
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > maximumTitleRunes || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", "", inputError(fmt.Sprintf("The title must be a single line of at most %d characters.", maximumTitleRunes))
	}
	if brief == "" || !utf8.ValidString(brief) || utf8.RuneCountInString(brief) > maximumBriefRunes {
		return "", "", inputError(fmt.Sprintf("The brief must be between 1 and %d characters.", maximumBriefRunes))
	}
	return title, brief, nil
}

// Cancel ends one of the principal's open tasks at once, without a report.
func (s *Service) Cancel(ctx context.Context, principalID, taskID string) (Task, bool, error) {
	task, cancelled, err := s.repository.CancelTask(ctx, principalID, taskID, time.Now())
	if err != nil || !cancelled {
		return task, false, err
	}
	s.mu.Lock()
	stop := s.running[taskID]
	s.mu.Unlock()
	if stop != nil {
		stop(errCancelled)
	}
	s.removeCard(task)
	s.editStatus(ctx, task)
	s.logger.InfoContext(ctx, "task.cancelled", "principal_id", principalID, "task_id", taskID)
	return task, true, nil
}

// Tasks lists the principal's open tasks and those finished in the last week, newest last.
func (s *Service) Tasks(ctx context.Context, principalID string) ([]Task, error) {
	tasks, err := s.repository.ListTasks(ctx, principalID, time.Now().Add(-listWindow))
	if err != nil {
		return nil, err
	}
	if len(tasks) > maximumListed {
		tasks = tasks[len(tasks)-maximumListed:]
	}
	return tasks, nil
}

// Task returns one of the principal's own tasks.
func (s *Service) Task(ctx context.Context, principalID, taskID string) (Task, bool, error) {
	return s.repository.LoadTask(ctx, principalID, taskID)
}

// PendingNotice tells the principal's next chat turn which task reports reached them since the
// last turn, so "what did you find?" has something to refer to.
func (s *Service) PendingNotice(principalID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
	defer cancel()
	tasks, err := s.repository.TakeTaskNotices(ctx, principalID, time.Now().Add(-noticeWindow))
	if err != nil {
		s.logger.WarnContext(ctx, "task.notice.failed", "principal_id", principalID, "error", err)
		return ""
	}
	notices := make([]string, 0, len(tasks))
	for _, task := range tasks {
		outcome := "finished"
		switch {
		case task.Status == StatusFailed:
			outcome = "failed"
		case task.Partial:
			outcome = "stopped at its limit with a partial report"
		}
		notices = append(notices, fmt.Sprintf(
			"Background task %q (id %s) %s, and its report was sent to the user's chat. It begins:\n%s\ntask_result with this id returns the full report.",
			task.Title, task.ID, outcome, firstLines(task.Result),
		))
	}
	return strings.Join(notices, "\n\n")
}

// Now is the clock task lists are rendered against.
func (s *Service) Now() time.Time { return time.Now() }

func (s *Service) notify(principalID string) {
	s.mu.Lock()
	if s.ctx == nil || s.cancel == nil {
		s.mu.Unlock()
		return
	}
	wake, exists := s.workers[principalID]
	if !exists {
		wake = make(chan struct{}, 1)
		s.workers[principalID] = wake
		s.wg.Add(1)
		go s.runWorker(s.ctx, principalID, wake)
	}
	s.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// runWorker does one principal's task work in order, one task at a time.
func (s *Service) runWorker(ctx context.Context, principalID string, wake <-chan struct{}) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		for ctx.Err() == nil {
			task, found, err := s.repository.NextTask(ctx, principalID)
			if err != nil {
				if !s.retry(ctx, principalID, "", "load_task", err) {
					return
				}
				continue
			}
			if !found {
				break
			}
			if task.Open() {
				err = s.execute(ctx, task)
			} else {
				err = s.deliver(ctx, task)
			}
			if err != nil && !s.retry(ctx, principalID, task.ID, "work", err) {
				return
			}
		}
	}
}

func (s *Service) retry(ctx context.Context, principalID, taskID, stage string, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	s.logger.WarnContext(ctx, "task.worker.retry", "principal_id", principalID, "task_id", taskID, "stage", stage, "error", err)
	timer := time.NewTimer(retryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// execute runs one open task until it finishes, is cancelled, or the service stops. An error means
// nothing changed and the work should be tried again later.
func (s *Service) execute(ctx context.Context, task Task) error {
	if _, found, err := s.chats.ResolveActiveChat(ctx, task.PrincipalID); err != nil {
		return err
	} else if !found {
		_, _, err := s.repository.CancelTask(ctx, task.PrincipalID, task.ID, time.Now())
		s.logger.InfoContext(ctx, "task.skipped", "principal_id", task.PrincipalID, "task_id", task.ID, "reason", "no_chat")
		return err
	}

	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	s.mu.Lock()
	s.running[task.ID] = stop
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, task.ID)
		s.mu.Unlock()
	}()

	now := time.Now()
	started, ok, err := s.repository.StartTask(ctx, task.ID, now.Add(s.config.Timeout), now)
	if err != nil || !ok {
		return err
	}
	spanCtx, end := s.telemetry.StartTask(runCtx, started.PrincipalID)
	s.logger.InfoContext(spanCtx, "task.started", "principal_id", started.PrincipalID, "task_id", started.ID, "attempt", started.Attempts)
	sink := &progressSink{progress: started.Progress, modelBudget: s.config.ModelBudget}
	s.editStatus(spanCtx, started)
	stopWatching := s.watch(spanCtx, started, sink)
	outcome, finished := s.run(spanCtx, started, sink)
	stopWatching()
	progress := sink.snapshot()
	if !finished {
		label := "interrupted"
		if errors.Is(context.Cause(runCtx), errCancelled) {
			label = "cancelled"
		} else if err := s.repository.SaveTaskProgress(context.WithoutCancel(ctx), started.ID, progress, time.Now()); err != nil {
			s.logger.WarnContext(spanCtx, "task.progress.failed", "principal_id", started.PrincipalID, "task_id", started.ID, "error", err)
		}
		end(label, progress.ModelCalls, progress.ToolCalls, progress.PromptTokens, progress.OutputTokens)
		return nil
	}
	outcome.Progress = progress
	// The work is done, so storing its result outlives a shutdown that has begun and is retried here
	// rather than by running the task again.
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	saved, err := s.repository.FinishTask(saveCtx, started.ID, outcome, time.Now())
	cancelSave()
	for err != nil {
		if !s.retry(ctx, started.PrincipalID, started.ID, "store_result", err) {
			end("interrupted", progress.ModelCalls, progress.ToolCalls, progress.PromptTokens, progress.OutputTokens)
			return nil
		}
		saved, err = s.repository.FinishTask(ctx, started.ID, outcome, time.Now())
	}
	label := outcome.Status
	switch {
	case !saved:
		label = "cancelled"
	case outcome.Partial:
		label = "partial"
	}
	end(label, progress.ModelCalls, progress.ToolCalls, progress.PromptTokens, progress.OutputTokens)
	s.logger.InfoContext(spanCtx, "task.finished",
		"principal_id", started.PrincipalID, "task_id", started.ID, "outcome", label, "error_kind", outcome.ErrorKind,
		"tool_calls", progress.ToolCalls, "model_calls", progress.ModelCalls,
	)
	return nil
}

// run does the task's model work: its first run or the resumption of an interrupted one, then, if
// a limit ended it, one short run that asks for what it has. It reports false when the task was
// cancelled or the service stopped, which leaves the task as it is.
func (s *Service) run(ctx context.Context, task Task, sink *progressSink) (Outcome, bool) {
	callBudget := max(s.config.ToolBudget-(task.Progress.ToolCalls-task.Progress.WebSearches), 0)
	limit := "time limit"
	if time.Now().Before(task.Deadline) && !sink.overBudget() {
		message := resumeMessage
		if task.Attempts <= 1 {
			message = s.firstMessage(task)
		}
		deadlineCtx, cancelDeadline := context.WithDeadline(ctx, task.Deadline)
		mainCtx, stopMain := context.WithCancelCause(deadlineCtx)
		sink.begin(func() { stopMain(errModelBudget) })
		reply, err := s.runner.RunTask(mainCtx, conversation.TaskRun{
			PrincipalID: task.PrincipalID, Turn: outbox.TaskTurn(task.ID), SessionID: task.SessionID,
			Message: message, Instruction: s.firstMessage(task), Continued: task.Attempts > 1, CallBudget: callBudget, Sink: sink,
		})
		cause := context.Cause(mainCtx)
		partial := sink.end(callBudget)
		stopMain(nil)
		cancelDeadline()
		switch {
		case err == nil && reply.ErrorKind != "":
			return Outcome{Status: StatusFailed, Result: reply.Text, ErrorKind: reply.ErrorKind}, true
		case err == nil:
			return Outcome{Status: StatusDone, Partial: partial, Result: reply.Text}, true
		case ctx.Err() != nil:
			return Outcome{}, false
		case errors.Is(cause, errModelBudget):
			limit = "limit on model calls"
		}
	} else if sink.overBudget() {
		limit = "limit on model calls"
	}

	reportCtx, cancelReport := context.WithTimeout(ctx, s.config.Grace)
	defer cancelReport()
	sink.begin(nil)
	reply, err := s.runner.RunTask(reportCtx, conversation.TaskRun{
		PrincipalID: task.PrincipalID, Turn: outbox.TaskTurn(task.ID), SessionID: task.SessionID,
		Message: limitMessage(limit), Instruction: s.firstMessage(task), Continued: true, Report: true, Sink: sink,
	})
	switch {
	case ctx.Err() != nil:
		return Outcome{}, false
	case err != nil:
		return Outcome{Status: StatusFailed, Result: LimitText, ErrorKind: "limit"}, true
	case reply.ErrorKind != "":
		return Outcome{Status: StatusFailed, Result: reply.Text, ErrorKind: reply.ErrorKind}, true
	default:
		return Outcome{Status: StatusDone, Partial: true, Result: reply.Text}, true
	}
}

// firstMessage tells the sub-agent what it is, so it works through the brief and reports instead
// of asking questions or offering changes it cannot make.
func (s *Service) firstMessage(task Task) string {
	return fmt.Sprintf(
		"[system] You are a background task the user started from chat, titled %q. The user is not watching and cannot answer questions, "+
			"and you cannot see the chat, so the brief below is all you have. Only read-only tools are available, so nothing can be created, "+
			"changed, or saved; if something should change, say so in the report. Work through the brief thoroughly: you may make up to %d "+
			"tool calls and have up to %s. Then reply with the report the user should receive, leading with what they asked for.\n\n%s",
		task.Title, s.config.ToolBudget, Duration(s.config.Timeout), task.Brief,
	)
}

func limitMessage(limit string) string {
	return "[system] This background task has reached its " + limit + ". Do not look anything else up: tool calls will be refused. " +
		"Write the report now from what you have found so far, and say plainly what you did not get to."
}

// watch updates the status message and the stored progress once per interval until stopped.
func (s *Service) watch(ctx context.Context, task Task, sink *progressSink) func() {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(s.config.ProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			task.Progress = sink.snapshot()
			if err := s.repository.SaveTaskProgress(ctx, task.ID, task.Progress, time.Now()); err != nil && ctx.Err() == nil {
				s.logger.WarnContext(ctx, "task.progress.failed", "principal_id", task.PrincipalID, "task_id", task.ID, "error", err)
			}
			s.editStatus(ctx, task)
		}
	}()
	return func() {
		close(stop)
		<-stopped
	}
}

func (s *Service) editStatus(ctx context.Context, task Task) {
	// A worker can start a task before task_start has stored the id of its status message.
	if task.StatusMessageID == 0 {
		if stored, found, err := s.repository.LoadTask(ctx, task.PrincipalID, task.ID); err == nil && found {
			task.StatusMessageID = stored.StatusMessageID
		}
	}
	if task.StatusMessageID == 0 {
		return
	}
	if err := s.messenger.EditStatus(ctx, task.ChatID, task.StatusMessageID, StatusText(task, time.Now()), task.ID, task.Open()); err != nil && ctx.Err() == nil {
		s.logger.WarnContext(ctx, "task.status.failed", "principal_id", task.PrincipalID, "task_id", task.ID, "stage", "edit", "error", err)
	}
}

// deliver sends a finished task's report as a reply to the message that asked for it. A transient
// failure returns an error and is retried; a permanent one, or an owner without a chat, ends it.
func (s *Service) deliver(ctx context.Context, task Task) error {
	if _, found, err := s.chats.ResolveActiveChat(ctx, task.PrincipalID); err != nil {
		return err
	} else if !found {
		s.logger.InfoContext(ctx, "task.undeliverable", "principal_id", task.PrincipalID, "task_id", task.ID, "reason", "no_chat")
		return s.finishDelivery(ctx, task)
	}
	replyTo := task.RequestMessageID
	chunks := conversation.SplitReply(ReportText(task))
	var err error
	// A failed task's text explains the failure, so a card it staged would illustrate nothing.
	if s.config.Cards != nil && task.ErrorKind == "" {
		chunks, replyTo, err = conversation.SendCard(ctx, s.logger, s.config.Cards, s.config.Photos, task.PrincipalID, outbox.TaskTurn(task.ID), task.ChatID, replyTo, chunks)
		if err != nil {
			return err
		}
	}
	for _, chunk := range chunks {
		if err := s.deliverer.SendReply(ctx, task.ChatID, replyTo, chunk); err != nil {
			if isPermanent(err) {
				s.logger.WarnContext(ctx, "task.undeliverable", "principal_id", task.PrincipalID, "task_id", task.ID, "reason", "send_rejected")
				return s.finishDelivery(ctx, task)
			}
			return err
		}
		replyTo = 0
	}
	s.logger.InfoContext(ctx, "task.delivered", "principal_id", task.PrincipalID, "task_id", task.ID)
	return s.finishDelivery(ctx, task)
}

func (s *Service) finishDelivery(ctx context.Context, task Task) error {
	if err := s.repository.MarkTaskDelivered(ctx, task.ID, time.Now()); err != nil {
		return err
	}
	s.removeCard(task)
	s.editStatus(ctx, task)
	return nil
}

func (s *Service) removeCard(task Task) {
	if s.config.Cards != nil {
		s.config.Cards.Remove(task.PrincipalID, outbox.TaskTurn(task.ID))
	}
}

// progressSink counts what a task's runs use and stops a run that exceeds the model call budget.
type progressSink struct {
	mu          sync.Mutex
	progress    Progress
	brokerCalls int
	modelBudget int
	stop        func()
}

func (p *progressSink) Emit(_ context.Context, event agentruntime.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch event.Type {
	case "tool_call":
		p.progress.ToolCalls++
		if event.Text == webSearchTool {
			p.progress.WebSearches++
		} else {
			p.brokerCalls++
		}
	case "model_call":
		p.progress.ModelCalls += event.Usage.ModelCalls
		p.progress.PromptTokens += event.Usage.PromptTokens
		p.progress.OutputTokens += event.Usage.OutputTokens
		if p.progress.ModelCalls > p.modelBudget && p.stop != nil {
			p.stop()
			p.stop = nil
		}
	}
	return nil
}

// begin starts counting one run, with what stops it once it exceeds the model call budget; nil
// lets a run go past it, as the report run must.
func (p *progressSink) begin(stop func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.brokerCalls = 0
	p.stop = stop
}

// end stops bounding the run and reports whether it asked for more broker calls than its budget,
// which means its report was written once further lookups were refused.
func (p *progressSink) end(budget int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop = nil
	return p.brokerCalls > budget
}

func (p *progressSink) overBudget() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.progress.ModelCalls > p.modelBudget
}

func (p *progressSink) snapshot() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.progress
}

type noTelemetry struct{}

func (noTelemetry) StartTask(ctx context.Context, _ string) (context.Context, func(string, int, int, int64, int64)) {
	return ctx, func(string, int, int, int64, int64) {}
}

// firstLines is the start of a report, enough for the chat to know what it said.
func firstLines(text string) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, line)
		if len(kept) == maximumNoticeLine {
			break
		}
	}
	joined := strings.Join(kept, "\n")
	if utf8.RuneCountInString(joined) > maximumNoticeRune {
		joined = string([]rune(joined)[:maximumNoticeRune]) + "…"
	}
	return joined
}

// inputError is a request the model can correct, returned to it as the tool's result.
type inputError string

func (e inputError) Error() string { return string(e) }

func isInput(err error) bool {
	var input inputError
	return errors.As(err, &input)
}

func isPermanent(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
}

func newTaskID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate task identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate task session identifier: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
