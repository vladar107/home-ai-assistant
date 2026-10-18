package conversation

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
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const (
	// MaximumMessageRunes is the longest message a turn accepts.
	MaximumMessageRunes = 4096
	maximumReplyRunes   = 4000
	deliveryRetryDelay  = 5 * time.Second
	toolHintBudget      = 800 * time.Millisecond

	// CardRetention is how long a staged card may wait for its reply before a sweep removes it.
	CardRetention = 24 * time.Hour
)

const (
	BusyReply         = "I am still working on your previous message. You can wait for it or use /cancel."
	InterruptedReply  = "The previous response was interrupted by a restart, so I did not retry it. Please send it again if you still need it."
	TimeoutReply      = "That response took too long. Please try again with a smaller request."
	UnavailableReply  = "I could not complete that response. /status and /privacy are still available."
	QuotaReply        = "Your model subscription has reached its usage limit. Please try again later."
	ReconnectReply    = "Your model subscription is not connected or has expired. Reconnect it and try again."
	NotConnectedReply = "Your model subscription is not connected. Use /settings to connect it."
)

// NothingToAdd is the reply a continuation turn gives when the request is finished, or a turn whose
// reaction is the whole answer; it is never sent.
const NothingToAdd = "[nothing-to-add]"

// ContinueMessage starts the turn that follows a confirmed proposal, so a request with several
// dependent steps moves on without the user asking again. The outcome arrives as the turn's notice.
const ContinueMessage = "[system] The user pressed Confirm and the outcome is above. Continue the user's earlier request: " +
	"if a step remains, do it, which may mean proposing the next change, and answer any question from the user's " +
	"earlier messages that is still unanswered, using what you know now. Only when every step is done, every " +
	"question is answered, and the outcome already says everything, reply with exactly " + NothingToAdd + " and nothing else."

type Service struct {
	config       Config
	repository   Repository
	runtime      agentruntime.AgentRuntime
	deliverer    Deliverer
	toolProvider ToolProvider
	gate         ConnectionGate
	logger       *slog.Logger
	telemetry    Telemetry
	tabReplies   TabReplies
	// startedAt is truncated like the stored created_at, so a request queued in the same
	// millisecond as the start does not read as recovered.
	startedAt time.Time

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	workers  map[Lane]chan struct{}
	inflight map[Lane]context.CancelFunc
	// turns holds one slot per lane, so a lane's chat turns take turns with each other and the main
	// lane's also with scheduled runs. It once guarded a whole principal against a credential-refresh
	// race that Pi 1.0.0 removed by locking auth.json, which is why tabs and background tasks run
	// beside each other.
	turns map[Lane]chan struct{}
	// drafts holds the draft of each lane's running chat turn, so a stop press counts only while
	// that turn is still running.
	drafts map[Lane]*runningDraft
	wg     sync.WaitGroup
	// captures holds what a finished chat turn measured until its reply is delivered; a restart
	// in between loses it, and that reply is then counted as recovered.
	captures map[string]replyCapture
}

type runningDraft struct {
	id      int64
	cancel  context.CancelFunc
	stopped bool
}

type replyCapture struct {
	lane                Lane
	queue, setup, model time.Duration
	safetyChecks        string
	// plainReply is the model's successful chat reply without memory notes, or "" for any other.
	plainReply string
	// message is what the user wrote for a turn with a plain reply, which the store no longer
	// holds once the reply is saved, or "" when the message was the bot's own.
	message string
}

func NewService(
	config Config,
	repository Repository,
	runtime agentruntime.AgentRuntime,
	deliverer Deliverer,
	toolProvider ToolProvider,
	gate ConnectionGate,
	logger *slog.Logger,
	telemetry Telemetry,
) (*Service, error) {
	if config.TurnTimeout <= 0 {
		return nil, fmt.Errorf("conversation turn timeout must be positive")
	}
	if repository == nil {
		return nil, fmt.Errorf("conversation repository is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("conversation runtime is required")
	}
	if deliverer == nil {
		return nil, fmt.Errorf("conversation deliverer is required")
	}
	if toolProvider == nil {
		return nil, fmt.Errorf("conversation tool provider is required")
	}
	if gate == nil {
		return nil, fmt.Errorf("conversation connection gate is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("conversation logger is required")
	}
	if telemetry == nil {
		return nil, fmt.Errorf("conversation telemetry is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.TypingInterval <= 0 {
		config.TypingInterval = defaultTypingInterval
	}
	if config.DraftDelay <= 0 {
		config.DraftDelay = defaultDraftDelay
	}
	if config.DraftInterval <= 0 {
		config.DraftInterval = defaultDraftInterval
	}
	if (config.Cards == nil) != (config.Photos == nil) {
		return nil, fmt.Errorf("conversation cards and photos must be configured together")
	}

	return &Service{
		config:       config,
		repository:   repository,
		runtime:      runtime,
		deliverer:    deliverer,
		toolProvider: toolProvider,
		gate:         gate,
		logger:       logger,
		telemetry:    telemetry,
		startedAt:    time.Now().Truncate(time.Millisecond),
		captures:     make(map[string]replyCapture),
		workers:      make(map[Lane]chan struct{}),
		inflight:     make(map[Lane]context.CancelFunc),
		turns:        make(map[Lane]chan struct{}),
		drafts:       make(map[Lane]*runningDraft),
	}, nil
}

// SetTabReplies hands each plain reply delivered in a tab to tabs; call it before Start.
func (s *Service) SetTabReplies(tabs TabReplies) {
	s.tabReplies = tabs
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.ctx != nil {
		s.mu.Unlock()
		return fmt.Errorf("conversation service is already running")
	}
	// Workers stop at Shutdown, not at the start context's cancellation, so a turn running at a
	// signal gets the same shutdown order and budget as scheduled runs and tasks.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.ctx = runCtx
	s.cancel = cancel
	s.mu.Unlock()

	if s.config.Attachments != nil {
		if err := s.config.Attachments.Purge(ctx); err != nil {
			cancel()
			s.mu.Lock()
			s.ctx = nil
			s.cancel = nil
			s.mu.Unlock()
			return fmt.Errorf("purge conversation attachments: %w", err)
		}
	}
	if s.config.Cards != nil {
		if err := s.config.Cards.Sweep(time.Now().Add(-CardRetention)); err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "conversation.cards.sweep_failed", "error", err)
		}
	}

	lanes, err := s.repository.RecoverConversations(ctx)
	if err != nil {
		cancel()
		s.mu.Lock()
		s.ctx = nil
		s.cancel = nil
		s.mu.Unlock()
		return fmt.Errorf("recover conversation work: %w", err)
	}
	for _, lane := range lanes {
		s.notify(lane)
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	if cancel == nil {
		s.mu.Unlock()
		return nil
	}
	s.cancel = nil
	s.mu.Unlock()
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
		s.workers = make(map[Lane]chan struct{})
		s.inflight = make(map[Lane]context.CancelFunc)
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown conversation service: %w", ctx.Err())
	}
}

func (s *Service) Enqueue(ctx context.Context, input EnqueueInput) error {
	if input.BotUserID <= 0 || input.TelegramUpdateID < 0 || input.TelegramMessageID <= 0 || input.PrincipalID == "" || input.ChatID <= 0 {
		return fmt.Errorf("%w: identity is invalid", ErrInvalidRequest)
	}
	if !utf8.ValidString(input.Message) || utf8.RuneCountInString(input.Message) > MaximumMessageRunes {
		return fmt.Errorf("%w: message must be valid UTF-8 within %d characters", ErrInvalidRequest, MaximumMessageRunes)
	}
	// A photo or a file carries the request on its own, so only a message with neither is empty.
	if strings.TrimSpace(input.Message) == "" && len(input.Attachments) == 0 {
		return fmt.Errorf("%w: it must carry a message or an attachment", ErrInvalidRequest)
	}
	if len(input.Attachments) > 0 && s.config.Attachments == nil {
		return fmt.Errorf("%w: attachments are not supported", ErrInvalidRequest)
	}
	if !s.running() {
		return fmt.Errorf("conversation service is not running")
	}

	sessionID, err := newID()
	if err != nil {
		return err
	}
	runtimeSessionID, err := newID()
	if err != nil {
		return err
	}
	requestID, err := newID()
	if err != nil {
		return err
	}

	var staged []attachment.Record
	if len(input.Attachments) > 0 {
		staged, err = s.config.Attachments.Stage(input.PrincipalID, requestID, input.Attachments)
		if err != nil {
			return fmt.Errorf("store conversation attachments: %w", err)
		}
	}

	result, err := s.repository.EnqueueConversation(ctx, input, Session{
		ID:               sessionID,
		PrincipalID:      input.PrincipalID,
		RuntimeSessionID: runtimeSessionID,
	}, requestID, staged)
	// Only a created request owns the staged bytes; a busy or redelivered update leaves them behind.
	if !result.Created && len(staged) > 0 {
		s.config.Attachments.Discard(staged)
	}
	if err != nil {
		return fmt.Errorf("persist conversation request: %w", err)
	}
	if result.Busy {
		return ErrBusy
	}
	if result.Elsewhere {
		return &OpenElsewhereError{ThreadID: result.ElsewhereThread, TabName: result.ElsewhereTab}
	}
	if result.Created {
		s.leaveSession(ctx, input.PrincipalID, result.SwitchedFrom)
	}
	if result.Created && !result.Request.ResumedFrom.IsZero() {
		s.telemetry.RecordConversationResume(ctx, input.PrincipalID, "reply")
	}
	if result.Request.State == RequestStateQueued || result.Request.State == RequestStateReady {
		s.notify(result.Request.Lane())
	}
	return nil
}

func (s *Service) Cancel(ctx context.Context, lane Lane) (bool, error) {
	cancelled, err := s.repository.CancelPendingConversation(ctx, lane)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	cancel := s.inflight[lane]
	s.dropCaptures(lane)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return cancelled, nil
}

// CancelAll stops the pending and running turns of every lane of the principal, as when their model
// subscription is disconnected.
func (s *Service) CancelAll(ctx context.Context, principalID string) (bool, error) {
	cancelled, err := s.repository.CancelPrincipalConversations(ctx, principalID)
	if err != nil {
		return false, err
	}
	var cancels []context.CancelFunc
	s.mu.Lock()
	for lane, cancel := range s.inflight {
		if lane.PrincipalID == principalID {
			cancels = append(cancels, cancel)
			s.dropCaptures(lane)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return cancelled, nil
}

// StopDraft cancels the lane's running turn when draftID is the draft that turn shows. Like
// /cancel it cancels the request in the store before the context, so a failed store call leaves the
// turn running and a retried press still finds it. A turn that finds itself stopped when it
// unregisters saves nothing; one that saved first finds its request no longer running.
func (s *Service) StopDraft(ctx context.Context, lane Lane, draftID int64) (bool, error) {
	if !s.showsDraft(lane, draftID) {
		return false, nil
	}
	if _, err := s.repository.CancelPendingConversation(ctx, lane); err != nil {
		return false, err
	}
	s.mu.Lock()
	if draft := s.drafts[lane]; draft != nil && draft.id == draftID {
		draft.stopped = true
		draft.cancel()
	}
	s.dropCaptures(lane)
	s.mu.Unlock()
	return true, nil
}

func (s *Service) showsDraft(lane Lane, draftID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft := s.drafts[lane]
	return draft != nil && draft.id == draftID
}

// endDraft unregisters the lane's running turn from StopDraft and reports whether a stop press
// already cancelled it.
func (s *Service) endDraft(lane Lane) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft := s.drafts[lane]
	delete(s.drafts, lane)
	return draft != nil && draft.stopped
}

// dropCaptures forgets the measurements of the lane's cancelled requests, which never reach
// delivery. The caller holds s.mu.
func (s *Service) dropCaptures(lane Lane) {
	for id, capture := range s.captures {
		if capture.lane == lane {
			delete(s.captures, id)
		}
	}
}

// othersBusy reports whether another lane of the principal has a turn running or delivering. The
// caller holds s.mu.
func (s *Service) othersBusy(lane Lane) bool {
	for other := range s.inflight {
		if other.PrincipalID == lane.PrincipalID && other != lane {
			return true
		}
	}
	return false
}

func (s *Service) NewSession(ctx context.Context, lane Lane) (bool, error) {
	sessionID, reset, err := s.repository.ResetConversation(ctx, lane)
	if err != nil {
		return false, err
	}
	if reset && s.config.Attachments != nil {
		if err := s.config.Attachments.CloseSession(ctx, lane.PrincipalID, sessionID); err != nil {
			return false, err
		}
	}
	s.mu.Lock()
	cancel := s.inflight[lane]
	s.dropCaptures(lane)
	// Staged cards are kept per principal, so they are cleared only while no other tab's turn may
	// still send one. A cancelled turn removes its own card, and the sweep removes any left over.
	discard := s.config.Cards != nil && !s.othersBusy(lane)
	s.mu.Unlock()
	if discard {
		s.config.Cards.Discard(lane.PrincipalID)
	}
	if cancel != nil {
		cancel()
	}
	return reset, nil
}

// CloseLane ends the conversation of a tab that was closed or is gone, keeping it in the list to
// resume. A tab the user closed has its turn cancelled first, so no reply of it reaches the main
// view. A gone tab's turn, if one is pending, is left to finish: its replies already fall back to
// the main view, so cancelling it would only lose them.
func (s *Service) CloseLane(ctx context.Context, lane Lane, reason string) (bool, error) {
	if reason == CloseTabClosed {
		if _, err := s.Cancel(ctx, lane); err != nil {
			return false, err
		}
	}
	sessionID, closed, err := s.repository.CloseLane(ctx, lane, reason)
	if err != nil {
		return false, err
	}
	if closed && reason == CloseTabClosed {
		s.leaveSession(ctx, lane.PrincipalID, sessionID)
	}
	return closed, nil
}

// Sessions lists the principal's recent conversations, newest first, as the lane sees them: only
// the lane's own active conversation is current, and one another tab holds is marked. Earlier
// conversations of which Pi recorded nothing the user wrote are left out.
func (s *Service) Sessions(ctx context.Context, lane Lane) ([]SessionSummary, error) {
	principalID := lane.PrincipalID
	records, err := s.repository.ConversationSessions(ctx, principalID, sessionCandidates)
	if err != nil {
		return nil, err
	}
	summaries := make([]SessionSummary, 0, min(len(records), ListedSessions))
	for _, record := range records {
		if len(summaries) == ListedSessions {
			break
		}
		summary, err := s.summarize(ctx, principalID, record)
		if err != nil {
			return nil, err
		}
		if summary.Active && record.ThreadID != lane.ThreadID {
			summary.Active, summary.Elsewhere = false, true
		}
		if summary.Title == "" && !summary.Active && s.config.History != nil {
			continue
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// Resume makes one of the principal's earlier conversations the lane's active one, so the lane's
// next turn continues its Pi session. A pending turn in the lane blocks it, like a new message, and
// a conversation another tab holds stays there.
func (s *Service) Resume(ctx context.Context, lane Lane, sessionID string) (ResumeResult, error) {
	principalID := lane.PrincipalID
	switched, err := s.repository.SwitchConversation(ctx, lane, sessionID)
	if err != nil {
		return ResumeResult{}, err
	}
	if switched.Busy || !switched.Found || switched.Elsewhere {
		return ResumeResult{Found: switched.Found, Busy: switched.Busy, Elsewhere: switched.Elsewhere}, nil
	}
	if switched.Switched {
		s.leaveSession(ctx, principalID, switched.Previous)
		s.telemetry.RecordConversationResume(ctx, principalID, "command")
	}
	summary, err := s.summarize(ctx, principalID, switched.Session)
	if err != nil {
		return ResumeResult{}, err
	}
	return ResumeResult{Found: true, Current: !switched.Switched, Session: summary}, nil
}

// SessionMessages returns the Telegram messages of one of the principal's conversations, oldest
// first: what the user sent and the replies they got.
func (s *Service) SessionMessages(ctx context.Context, principalID, sessionID string) ([]int64, error) {
	return s.repository.SessionMessageIDs(ctx, principalID, sessionID)
}

// leaveSession removes the files a conversation held once another one replaced it, as /new does.
// The switch is already committed, and the start-up purge removes the files of every closed
// session, so a failure here is only logged.
func (s *Service) leaveSession(ctx context.Context, principalID, sessionID string) {
	if sessionID == "" || s.config.Attachments == nil {
		return
	}
	if err := s.config.Attachments.CloseSession(ctx, principalID, sessionID); err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "conversation.attachments.close_failed", "principal_id", principalID, "error", err)
	}
}

func (s *Service) summarize(ctx context.Context, principalID string, record SessionRecord) (SessionSummary, error) {
	summary := SessionSummary{ID: record.ID, Active: record.Active, LastActive: s.local(record.LastActive), ThreadID: record.ThreadID, TabName: record.TabName}
	if s.config.History == nil {
		return summary, nil
	}
	title, err := s.config.History.Title(ctx, principalID, record.RuntimeSessionID)
	if err != nil {
		return SessionSummary{}, err
	}
	summary.Title = title
	return summary, nil
}

func (s *Service) local(at time.Time) time.Time {
	if s.config.Location == nil {
		return at
	}
	return at.In(s.config.Location)
}

func (s *Service) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx != nil && s.cancel != nil
}

func (s *Service) notify(lane Lane) {
	s.mu.Lock()
	if s.ctx == nil || s.cancel == nil {
		s.mu.Unlock()
		return
	}
	wake, exists := s.workers[lane]
	if !exists {
		wake = make(chan struct{}, 1)
		s.workers[lane] = wake
		s.wg.Add(1)
		go s.runWorker(s.ctx, lane, wake)
	}
	s.mu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
	}
}

// runWorker runs one lane's turns in order. Everything it sends goes to the lane's tab.
func (s *Service) runWorker(ctx context.Context, lane Lane, wake <-chan struct{}) {
	defer s.wg.Done()
	ctx = thread.With(ctx, lane.ThreadID)
	principalID := lane.PrincipalID
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}

		for ctx.Err() == nil {
			request, found, err := s.repository.NextConversationRequest(ctx, lane)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.logWorkerError(ctx, principalID, "load_request")
				if !wait(ctx, deliveryRetryDelay) {
					return
				}
				continue
			}
			if !found {
				break
			}
			if request.State == RequestStateReady {
				if err := s.deliver(ctx, request); err != nil {
					if ctx.Err() != nil {
						return
					}
					s.logWorkerError(ctx, principalID, "deliver_response")
					if !wait(ctx, deliveryRetryDelay) {
						return
					}
					continue
				}
				continue
			}
			s.runTurn(ctx, request)
		}
	}
}

func (s *Service) runTurn(ctx context.Context, request Request) {
	release, acquired := s.awaitTurn(ctx, request)
	if !acquired {
		return
	}
	defer release()

	run := tools.NewRun(request.Message)
	// A continuation's or a reconsider's Telegram message is one of the bot's own, so nothing may act
	// on it as the user's.
	if !ownMessage(request.Message) {
		run.MarkUserMessage()
	}
	if strings.HasPrefix(request.Message, ReconsiderMessage) {
		s.markReconsider(ctx, run, request)
	}
	turnCtx, cancel := context.WithTimeout(tools.WithRun(tools.WithTurn(ctx, outbox.ChatTurn(request.ID)), run), s.config.TurnTimeout)
	lane := request.Lane()
	s.mu.Lock()
	s.inflight[lane] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.inflight, lane)
		s.mu.Unlock()
	}()

	claimed, err := s.repository.ClaimConversationRequest(ctx, request.ID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logWorkerError(ctx, request.PrincipalID, "claim_request")
		_ = wait(ctx, deliveryRetryDelay)
		return
	}
	if !claimed {
		return
	}
	claimedAt := time.Now()

	safetyChecks := s.safetyChecksLabel(turnCtx, request.PrincipalID)
	turnCtx, endTurn := s.telemetry.StartConversationTurn(turnCtx, request.PrincipalID, safetyChecks)
	var modelTime time.Duration
	var runStarted time.Time

	errorKind := ""
	// The stored kind stays within the schema's allowed set; telemetry and logs carry the finer category.
	reportedKind := ""
	reply, plainReply := "", ""
	connected, runErr := s.gate.Ready(turnCtx, request.PrincipalID)
	var tools agentruntime.ToolSet
	var webSearch bool
	var memory string
	var attachments attachment.Turn
	var hint toolHint
	var activity *turnActivity
	defer func() {
		activity.stop()
		s.endDraft(lane)
	}()
	if runErr == nil && connected {
		draft := draftID(request.ID)
		s.mu.Lock()
		s.drafts[lane] = &runningDraft{id: draft, cancel: cancel}
		s.mu.Unlock()
		activity = s.startActivity(turnCtx, request.PrincipalID, request.ChatID, draft)
		tools, runErr = s.toolProvider.ToolsFor(turnCtx, request.PrincipalID)
		var hinted <-chan toolHint
		if runErr == nil {
			hinted = s.startToolHint(turnCtx, request, tools)
			webSearch, runErr = s.webSearchEnabled(turnCtx, request.PrincipalID)
		}
		if runErr == nil {
			memory, runErr = s.memoryText(turnCtx, request.PrincipalID)
		}
		if runErr != nil {
			errorKind = ErrorKindToolProvider
		}
		if runErr == nil && s.config.Attachments != nil {
			attachments, runErr = s.config.Attachments.Turn(turnCtx, request.PrincipalID, request.ID)
			if runErr != nil {
				errorKind, reportedKind = ErrorKindRuntime, ErrorKindAttachments
			} else if attachments.Summary != "" {
				s.markAttachmentsExternal(turnCtx, request, run)
			}
		}
		if hinted != nil && runErr == nil {
			hint = <-hinted
		}
	} else if runErr == nil {
		errorKind, reportedKind = ErrorKindRuntime, ErrorKindNotConnected
		reply = NotConnectedReply
	} else {
		errorKind = ErrorKindRuntime
	}
	if errorKind != "" {
		if reply == "" {
			reply = UnavailableReply
		}
	} else {
		// A continuation's or a reconsider's message is ours, so the user's own earlier request stays
		// the one actions are judged against.
		if s.config.Recent != nil && !ownMessage(request.Message) {
			s.config.Recent.RecordMessage(request.PrincipalID, request.ThreadID, request.Message)
		}
		notice := s.takeNotice(turnCtx, request)
		sink := s.newCountingSink(request.PrincipalID, outbox.ChatTurn(request.ID), run)
		sink.activity, sink.activities = activity, tools.Activities
		runStarted = time.Now()
		result, err := s.runtime.Run(turnCtx, agentruntime.RunRequest{
			PrincipalID: request.PrincipalID,
			SessionID:   request.RuntimeSessionID,
			Message:     s.turnMessage(request, notice.text, attachments.Summary, hint.name, s.placeLines(turnCtx, request.PrincipalID)),
			Images:      attachments.Images,
			Tools:       tools,
			Memory:      memory,
			Jobs:        s.jobsText(turnCtx, request.PrincipalID),
			WebSearch:   webSearch,
			SearchCheck: s.searchCheck(request.PrincipalID, webSearch, true),
		}, sink)
		modelTime = time.Since(runStarted)
		runErr = err
		activity.stop()
		notes := s.memoryNotes(request.PrincipalID, outbox.ChatTurn(request.ID))
		if err == nil && !errors.Is(turnCtx.Err(), context.Canceled) {
			s.recordTurnMetrics(turnCtx, request.PrincipalID, safetyChecks, sink, result.Usage, !tools.Empty() || webSearch, hint)
		}
		switch {
		case errors.Is(turnCtx.Err(), context.DeadlineExceeded) || errors.Is(runErr, agentruntime.ErrStalled):
			errorKind = ErrorKindTimeout
			reply = TimeoutReply
		case errors.Is(turnCtx.Err(), context.Canceled):
			s.removeCard(request)
			endTurn("cancelled", "cancelled")
			return
		case runErr != nil || strings.TrimSpace(result.Response) == "":
			errorKind = ErrorKindRuntime
			reply, reportedKind = failureReply(runErr)
		default:
			reply = result.Response
			if s.replyAsReaction(turnCtx, request.PrincipalID, reply) {
				reply = NothingToAdd
			}
			if !strings.Contains(reply, NothingToAdd) {
				plainReply = reply
			}
		}
		reply = withMemoryNotes(reply, notes)
	}
	if reportedKind == "" {
		reportedKind = errorKind
	}
	activity.stop()
	if s.endDraft(lane) {
		s.removeCard(request)
		endTurn("cancelled", "cancelled")
		return
	}

	outcome := "success"
	if errorKind != "" {
		outcome = "error"
	}
	var saved bool
	for {
		saved, err = s.repository.SaveConversationResponse(ctx, request.ID, reply, errorKind)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			endTurn("cancelled", "shutdown")
			return
		}
		s.logWorkerError(ctx, request.PrincipalID, "store_response")
		if !wait(ctx, deliveryRetryDelay) {
			endTurn("cancelled", "shutdown")
			return
		}
	}
	if !saved {
		s.removeCard(request)
		endTurn("cancelled", "cancelled")
		return
	}
	if runStarted.IsZero() {
		runStarted = time.Now()
	}
	message := ""
	if plainReply != "" && !ownMessage(request.Message) {
		message = request.Message
	}
	s.mu.Lock()
	s.captures[request.ID] = replyCapture{
		lane:         lane,
		queue:        max(claimedAt.Sub(request.CreatedAt), 0),
		setup:        runStarted.Sub(claimedAt),
		model:        modelTime,
		safetyChecks: safetyChecks,
		plainReply:   plainReply,
		message:      message,
	}
	s.mu.Unlock()
	endTurn(outcome, reportedKind)
	s.telemetry.ContextLogger(ctx, s.logger).Info(
		"conversation.turn.completed",
		"principal_id", request.PrincipalID,
		"session_id", request.SessionID,
		"outcome", outcome,
		"error_kind", reportedKind,
	)
}

// webSearchEnabled reads the principal's switch at turn start; an error fails the turn instead of
// guessing either way.
func (s *Service) webSearchEnabled(ctx context.Context, principalID string) (bool, error) {
	if s.config.WebSearch == nil {
		return false, nil
	}
	return s.config.WebSearch.WebSearchEnabled(ctx, principalID)
}

// searchCheck binds the search check to the principal for one run, or returns nil when the run
// has no web search. Chat turns can ask the user to confirm a search; scheduled runs and tasks
// cannot.
func (s *Service) searchCheck(principalID string, webSearch, interactive bool) agentruntime.SearchCheck {
	if !webSearch || s.config.SearchChecks == nil {
		return nil
	}
	return func(ctx context.Context, query string) agentruntime.SearchDecision {
		return s.config.SearchChecks.Check(ctx, principalID, query, interactive)
	}
}

// memoryText reads the principal's memory at turn start; an error fails the turn instead of
// running it without what the user asked it to remember.
func (s *Service) memoryText(ctx context.Context, principalID string) (string, error) {
	if s.config.Memory == nil {
		return "", nil
	}
	return s.config.Memory.Render(ctx, principalID)
}

func (s *Service) jobsText(ctx context.Context, principalID string) string {
	if s.config.Jobs == nil {
		return ""
	}
	return s.config.Jobs.PromptSection(ctx, principalID)
}

func (s *Service) memoryNotes(principalID, turn string) []string {
	if s.config.Memory == nil {
		return nil
	}
	return s.config.Memory.TakeNotes(principalID, turn)
}

// withMemoryNotes ends the reply with a line for each memory write the turn made, so the user
// does not depend on the model mentioning them.
func withMemoryNotes(reply string, notes []string) string {
	if len(notes) == 0 {
		return reply
	}
	return strings.TrimRight(reply, "\n") + "\n\n" + strings.Join(notes, "\n")
}

// toolHint is what a turn's tool hint came to. answered is false when no hint was asked or none
// came back in time, so follow-through counts only hints that had their say.
type toolHint struct {
	name     string
	answered bool
}

// startToolHint asks for the tool the message most likely needs while the rest of the turn is
// set up. The answer is advisory: any failure or a late answer yields no answer, and a name the
// turn does not offer yields an answered hint without a name. A nil result means no hint was
// started; a continuation's or a reconsider's message is ours, so it starts none.
func (s *Service) startToolHint(ctx context.Context, request Request, tools agentruntime.ToolSet) <-chan toolHint {
	if s.config.ToolHint == nil || tools.Empty() || strings.TrimSpace(request.Message) == "" || ownMessage(request.Message) {
		return nil
	}
	// Read before this turn's message is recorded, so earlier holds only what came before it.
	var earlier []turnstate.Line
	if s.config.Recent != nil {
		earlier = s.config.Recent.RecentExchange(request.PrincipalID, request.ThreadID)
	}
	result := make(chan toolHint, 1)
	go func() {
		hintCtx, cancel := context.WithTimeout(ctx, toolHintBudget)
		defer cancel()
		name, answered, err := s.config.ToolHint.Hint(hintCtx, request.PrincipalID, request.Message, earlier, tools.Definitions)
		if err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).DebugContext(ctx, "conversation.tool_hint.failed")
			result <- toolHint{}
			return
		}
		for _, definition := range tools.Definitions {
			if definition.Name == name && name != "" {
				result <- toolHint{name: name, answered: true}
				return
			}
		}
		// A name the turn does not offer is no answer, not a hint that found nothing.
		result <- toolHint{answered: answered && name == ""}
	}()
	return result
}

// turnMessage prefixes what became of the principal's last confirmation request, which was
// resolved between turns and which the model would otherwise have to guess at, and appends what
// the message carried so the model can see its files without reading them.
// turnNotice is what became of the principal's earlier requests, and whether it carries text from
// outside the household.
type turnNotice struct {
	text    string
	outside bool
}

// markAttachmentsExternal treats a turn that carries files as having read outside content: a
// forwarded file or photo can carry instructions, and it stays readable in the conversation after
// its turn.
func (s *Service) markAttachmentsExternal(ctx context.Context, request Request, run *tools.Run) {
	run.MarkExternal("")
	if s.config.Exposure != nil {
		s.config.Exposure.MarkTurnExposed(ctx, request.PrincipalID, outbox.ChatTurn(request.ID), true)
	}
}

// takeNotice collects the pending notices for a turn that is about to run. A notice from outside the
// household marks the turn as having read it, before the turn decides whether it may search, and a
// notice that may retell private content makes the conversation private before the turn replies.
func (s *Service) takeNotice(ctx context.Context, request Request) turnNotice {
	var text, reports string
	var outside, private, reportsOutside, reportsPrivate bool
	// Reports are read first, because quoting the message a reply answers drops that message's
	// notice, so the turn is told of it once.
	if s.config.Reports != nil {
		reports, reportsOutside, reportsPrivate = s.config.Reports.TurnReports(ctx, request.PrincipalID, ReportTurn{OpensSession: request.OpensSession, ReplyToMessageID: request.ReplyToMessageID})
	}
	if s.config.Notice != nil {
		text, outside, private = pendingNotice(s.config.Notice, request.Lane())
	}
	if reports != "" {
		text = strings.TrimSpace(text + "\n\n" + reports)
		outside = outside || reportsOutside
		private = private || reportsPrivate
	}
	if private && s.config.Exposure != nil {
		s.config.Exposure.MarkTurnPrivate(ctx, request.PrincipalID, outbox.ChatTurn(request.ID))
	}
	if outside {
		if run := tools.RunFrom(ctx); run != nil {
			run.MarkExternal(text)
		}
		if s.config.Exposure != nil {
			s.config.Exposure.MarkTurnExposed(ctx, request.PrincipalID, outbox.ChatTurn(request.ID), true)
		}
	}
	return turnNotice{text: text, outside: outside}
}

func (s *Service) turnMessage(request Request, notice, attachments, hint, place string) string {
	message := request.Message
	if hint != "" {
		message = "[system] Likely relevant tool: " + hint + ". Ignore this if it does not fit the request.\n\n" + message
	}
	if notice != "" {
		message = "[system] " + notice + "\n\n" + message
	}
	message = s.withUser(request.PrincipalID, withPlace(place, message))
	if line := subjectContext(request.Subject, request.SubjectName); line != "" {
		message = line + "\n\n" + message
	}
	if s.config.Location != nil {
		// The time goes in the message rather than the system prompt, so the prompt stays
		// cacheable and the session history keeps when each message was sent.
		now := s.config.Now().In(s.config.Location).Format("Monday 2 January 2006, 15:04 MST")
		message = "[system] Current local time: " + now + " (" + s.config.Location.String() + ").\n\n" + message
	}
	if attachments == "" {
		return message
	}
	if strings.TrimSpace(message) == "" {
		return attachments
	}
	return strings.TrimRight(message, "\n") + "\n\n" + attachments
}

func (s *Service) deliver(ctx context.Context, request Request) error {
	deliveryCtx, cancel := context.WithCancel(ctx)
	lane := request.Lane()
	s.mu.Lock()
	s.inflight[lane] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.inflight, lane)
		s.mu.Unlock()
	}()

	claimed, err := s.repository.ClaimConversationDelivery(deliveryCtx, request.ID)
	if err != nil {
		return err
	}
	if !claimed {
		s.mu.Lock()
		delete(s.captures, request.ID)
		s.mu.Unlock()
		return nil
	}
	deliveryStarted := time.Now()
	replyTo := request.TelegramMessageID
	response := request.ResponseText
	silent := false
	if strings.Contains(response, NothingToAdd) {
		response = strings.TrimSpace(strings.ReplaceAll(response, NothingToAdd, ""))
		silent = response == ""
	}
	// A turn with nothing left to say sends nothing, not even the line marking a resumed conversation.
	var chunks []string
	if !silent {
		switch {
		case request.SubjectLabel != "":
			var resumedFrom time.Time
			if !request.ResumedFrom.IsZero() {
				resumedFrom = s.local(request.ResumedFrom)
			}
			response = SubjectLine(request.SubjectLabel, resumedFrom) + "\n\n" + response
		case !request.ResumedFrom.IsZero():
			response = ResumedLine(s.local(request.ResumedFrom)) + "\n\n" + response
		}
		chunks = SplitReply(response)
	}
	// A failed turn's text explains the failure, so a card it staged would illustrate nothing.
	if s.config.Cards != nil && request.ErrorKind == "" && len(chunks) > 0 {
		chunks, replyTo, err = SendCard(deliveryCtx, s.telemetry.ContextLogger(deliveryCtx, s.logger), s.config.Cards, s.config.Photos, request.PrincipalID, outbox.ChatTurn(request.ID), request.ChatID, replyTo, chunks)
		if err != nil {
			_ = s.repository.ReleaseConversationDelivery(ctx, request.ID)
			return err
		}
	}
	for _, chunk := range chunks {
		messageID, _, err := s.deliverer.SendTrackedReply(deliveryCtx, request.ChatID, replyTo, chunk)
		if err != nil {
			if isPermanentDeliveryError(err) {
				s.removeCard(request)
				s.mu.Lock()
				capture, captured := s.captures[request.ID]
				delete(s.captures, request.ID)
				s.mu.Unlock()
				if !captured {
					capture.safetyChecks = "unavailable"
				}
				s.telemetry.RecordReply(ctx, request.PrincipalID, "chat", "undeliverable", capture.safetyChecks, max(time.Since(request.CreatedAt), 0))
				return s.repository.CancelConversationDelivery(ctx, request.ID)
			}
			_ = s.repository.ReleaseConversationDelivery(ctx, request.ID)
			return err
		}
		replyTo = 0
		// The chunk is already in the chat, so a failure to remember it only costs the user the
		// ability to return to this conversation by replying to it.
		if messageID > 0 {
			if err := s.repository.RecordConversationReply(deliveryCtx, request.PrincipalID, request.SessionID, request.ID, messageID); err != nil && ctx.Err() == nil {
				s.logWorkerError(ctx, request.PrincipalID, "record_reply")
			}
		}
	}
	if err := s.repository.MarkConversationDelivered(deliveryCtx, request.ID); err != nil {
		_ = s.repository.ReleaseConversationDelivery(ctx, request.ID)
		return err
	}
	plainReply, message := s.recordReply(ctx, request, deliveryStarted)
	s.removeCard(request)
	if s.config.FollowUp != nil {
		if err := s.config.FollowUp.AfterDelivery(deliveryCtx, request.PrincipalID, request.ChatID); err != nil && ctx.Err() == nil {
			s.logWorkerError(ctx, request.PrincipalID, "follow_up")
		}
	}
	s.rememberReply(ctx, request, plainReply)
	s.tabReplied(ctx, request, message, plainReply)
	return nil
}

// tabReplied passes a plain reply in a tab on in the background, under the worker's context, so
// Shutdown cancels and waits for it. A reply to the bot's own message, such as a continuation's,
// tells nothing of the conversation's topic and is not passed on.
func (s *Service) tabReplied(ctx context.Context, request Request, message, plainReply string) {
	if s.tabReplies == nil || request.ThreadID <= 0 || message == "" || plainReply == "" {
		return
	}
	reply := TabReply{Lane: request.Lane(), ChatID: request.ChatID, SessionID: request.SessionID, Message: message, Reply: plainReply}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.tabReplies.TabReplied(ctx, reply)
	}()
}

// rememberReply keeps a plain reply for the guards of the principal's next turns. It runs after the
// follow-up that judges this turn's proposals, so a reply never vouches for its own proposal. A
// conversation that read private content, or whose privacy cannot be told, keeps its replies to
// itself.
func (s *Service) rememberReply(ctx context.Context, request Request, plainReply string) {
	if plainReply == "" || s.config.Recent == nil || s.config.Exposure == nil {
		return
	}
	if s.config.Exposure.Private(ctx, request.PrincipalID, request.ID) {
		return
	}
	s.config.Recent.RecordReply(request.PrincipalID, request.ThreadID, plainReply)
}

// recordReply counts a delivered reply and returns its plain text and the user's message, if the
// turn kept them. The stages are recorded only for a plain delivered reply, because dashboards
// divide their sums by the delivered chat reply count.
func (s *Service) recordReply(ctx context.Context, request Request, deliveryStarted time.Time) (string, string) {
	s.mu.Lock()
	capture, captured := s.captures[request.ID]
	delete(s.captures, request.ID)
	s.mu.Unlock()

	outcome := "delivered"
	switch {
	case !captured || request.CreatedAt.Before(s.startedAt):
		outcome = "recovered"
	case request.ErrorKind != "":
		outcome = "failed"
	}
	if !captured {
		capture.safetyChecks = "unavailable"
	}
	now := time.Now()
	s.telemetry.RecordReply(ctx, request.PrincipalID, "chat", outcome, capture.safetyChecks, max(now.Sub(request.CreatedAt), 0))
	if outcome != "delivered" {
		return "", ""
	}
	s.telemetry.RecordReplyStage(ctx, request.PrincipalID, "queue", capture.queue)
	s.telemetry.RecordReplyStage(ctx, request.PrincipalID, "setup", capture.setup)
	s.telemetry.RecordReplyStage(ctx, request.PrincipalID, "model", capture.model)
	s.telemetry.RecordReplyStage(ctx, request.PrincipalID, "delivery", now.Sub(deliveryStarted))
	return capture.plainReply, capture.message
}

// safetyChecksLabel reads the principal's opt-in once per turn; any failure to know is "unavailable".
func (s *Service) safetyChecksLabel(ctx context.Context, principalID string) string {
	if s.config.SafetyChecks == nil {
		return "unavailable"
	}
	enabled, err := s.config.SafetyChecks.JudgmentsEnabled(ctx, principalID)
	switch {
	case err != nil:
		return "unavailable"
	case enabled:
		return "on"
	default:
		return "off"
	}
}

func (s *Service) recordTurnMetrics(ctx context.Context, principalID, safetyChecks string, sink *countingSink, usage agentruntime.Usage, offeredTools bool, hint toolHint) {
	s.telemetry.RecordModelTurn(ctx, principalID, safetyChecks, usage.ModelCalls, usage.PromptTokens, usage.OutputTokens)
	calls, called := sink.snapshot()
	if offeredTools {
		s.telemetry.RecordToolCallsPerTurn(ctx, principalID, safetyChecks, calls)
	}
	if !hint.answered {
		return
	}
	result := "no_tool"
	switch {
	case hint.name == "":
		if calls == 0 {
			return
		}
		result = "missed"
	case called[hint.name]:
		result = "followed"
	case calls > 0:
		result = "other_tool"
	}
	s.telemetry.RecordHintFollowThrough(ctx, principalID, result)
}

func (s *Service) removeCard(request Request) {
	if s.config.Cards != nil {
		s.config.Cards.Remove(request.PrincipalID, outbox.ChatTurn(request.ID))
	}
}

func isPermanentDeliveryError(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
}

// failureReply maps a failed run to the reply the user sees and the category telemetry reports.
func failureReply(err error) (string, string) {
	if errors.Is(err, agentruntime.ErrStalled) {
		return TimeoutReply, ErrorKindTimeout
	}
	var providerErr *agentruntime.ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.Category {
		case agentruntime.FailureQuota:
			return QuotaReply, ErrorKindProviderQuota
		case agentruntime.FailureAuth:
			return ReconnectReply, ErrorKindProviderAuth
		}
	}
	return UnavailableReply, ErrorKindRuntime
}

// awaitTurn waits for the lane's turn slot. While another turn holds it the user sees the typing
// indicator, and /cancel or /new can still stop the wait.
func (s *Service) awaitTurn(ctx context.Context, request Request) (func(), bool) {
	lane := request.Lane()
	slot := s.turnSlot(lane)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	default:
	}

	waitCtx, stopWaiting := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight[lane] = stopWaiting
	s.mu.Unlock()
	defer func() {
		stopWaiting()
		s.mu.Lock()
		delete(s.inflight, lane)
		s.mu.Unlock()
	}()
	typing := s.startActivity(waitCtx, request.PrincipalID, request.ChatID, 0)
	defer typing.stop()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-waitCtx.Done():
		return nil, false
	}
}

// acquireTurn takes the main lane's slot, where scheduled runs take turns with chat turns.
func (s *Service) acquireTurn(ctx context.Context, principalID string) (func(), error) {
	slot := s.turnSlot(Lane{PrincipalID: principalID})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) turnSlot(lane Lane) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, exists := s.turns[lane]
	if !exists {
		slot = make(chan struct{}, 1)
		s.turns[lane] = slot
	}
	return slot
}

func (s *Service) logWorkerError(ctx context.Context, principalID, errorKind string) {
	s.telemetry.ContextLogger(ctx, s.logger).Warn(
		"conversation.worker.retry",
		"principal_id", principalID,
		"error_kind", errorKind,
	)
}

// webSearchTool runs inside the runtime rather than through the broker, so its calls get no
// broker audit line; the turn's sink records each by name instead.
const webSearchTool = "web_search"

// countingSink remembers which tools a turn called, by name only, and logs each web search call.
// A web search result also marks the turn as having read third-party content, which the broker
// cannot do for a tool that runs inside the runtime. The mark waits for the result, so the search
// check of a call still in flight never sees that call's own exposure.
type countingSink struct {
	principalID string
	turn        string
	logger      *slog.Logger
	telemetry   Telemetry
	exposure    tools.TurnExposureRecorder
	run         *tools.Run

	// activity and activities follow a chat turn's tool calls on its draft; both are nil for a turn
	// nobody is watching.
	activity   *turnActivity
	activities map[string]string

	mu     sync.Mutex
	calls  int
	called map[string]bool
}

func (s *Service) newCountingSink(principalID, turn string, run *tools.Run) *countingSink {
	return &countingSink{principalID: principalID, turn: turn, logger: s.logger, telemetry: s.telemetry, exposure: s.config.Exposure, run: run, called: make(map[string]bool)}
}

func (c *countingSink) Emit(ctx context.Context, event agentruntime.Event) error {
	switch event.Type {
	case "tool_call":
		c.activity.show(activityKey(event.Text, c.activities))
	case "tool_result":
		c.activity.show("")
	}
	if event.Type == "tool_result" && event.Tool == webSearchTool {
		if c.run != nil {
			c.run.MarkExternal(event.Text)
		}
		if c.exposure != nil {
			c.exposure.MarkTurnExposed(ctx, c.principalID, c.turn, true)
		}
		return nil
	}
	if event.Type != "tool_call" {
		return nil
	}
	c.mu.Lock()
	c.calls++
	c.called[event.Text] = true
	c.mu.Unlock()
	if event.Text == webSearchTool {
		c.telemetry.ContextLogger(ctx, c.logger).InfoContext(ctx, "conversation.web_search.called", "principal_id", c.principalID, "tool", webSearchTool)
	}
	return nil
}

func (c *countingSink) snapshot() (int, map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	called := make(map[string]bool, len(c.called))
	for name := range c.called {
		called[name] = true
	}
	return c.calls, called
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate conversation identifier: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// SplitReply cuts a reply into Telegram-sized chunks, preferring line breaks.
func SplitReply(reply string) []string {
	runes := []rune(strings.TrimSpace(reply))
	if len(runes) == 0 {
		return []string{UnavailableReply}
	}
	chunks := make([]string, 0, (len(runes)+maximumReplyRunes-1)/maximumReplyRunes)
	for len(runes) > maximumReplyRunes {
		cut := maximumReplyRunes
		for index := maximumReplyRunes - 1; index >= maximumReplyRunes/2; index-- {
			if runes[index] == '\n' {
				cut = index + 1
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
	}
	if remaining := strings.TrimSpace(string(runes)); remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// locationLine is where the principal is, for the turn's message. A failed lookup leaves the
// line out rather than failing the turn.
func (s *Service) locationLine(ctx context.Context, principalID string) string {
	if s.config.Locations == nil {
		return ""
	}
	line, err := s.config.Locations.TurnLine(ctx, principalID)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "conversation.location.failed", "principal_id", principalID, "error", err)
		return ""
	}
	return line
}

// tripLine is the principal's active or next trip, for the turn's message. A failed lookup
// leaves the line out rather than failing the turn.
func (s *Service) tripLine(ctx context.Context, principalID string) string {
	if s.config.Trips == nil {
		return ""
	}
	line, err := s.config.Trips.TurnLine(ctx, principalID)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "conversation.trip.failed", "principal_id", principalID, "error", err)
		return ""
	}
	return line
}

// placeLines is where the principal is and where they are going, for the turn's message.
func (s *Service) placeLines(ctx context.Context, principalID string) string {
	var lines []string
	for _, line := range []string{s.locationLine(ctx, principalID), s.tripLine(ctx, principalID)} {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}
