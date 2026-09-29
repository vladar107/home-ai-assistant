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
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	maximumMessageRunes = 4096
	maximumReplyRunes   = 4000
	deliveryRetryDelay  = 5 * time.Second

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

type Service struct {
	config       Config
	repository   Repository
	runtime      agentruntime.AgentRuntime
	deliverer    Deliverer
	toolProvider ToolProvider
	gate         ConnectionGate
	logger       *slog.Logger
	telemetry    Telemetry

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	workers  map[string]chan struct{}
	inflight map[string]context.CancelFunc
	// turns holds one slot per principal: two model processes on one principal's profile would
	// race on refreshing its credentials, so chat turns and scheduled runs take turns.
	turns map[string]chan struct{}
	wg    sync.WaitGroup
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
		workers:      make(map[string]chan struct{}),
		inflight:     make(map[string]context.CancelFunc),
		turns:        make(map[string]chan struct{}),
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.ctx != nil {
		s.mu.Unlock()
		return fmt.Errorf("conversation service is already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
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

	principals, err := s.repository.RecoverConversations(ctx)
	if err != nil {
		cancel()
		s.mu.Lock()
		s.ctx = nil
		s.cancel = nil
		s.mu.Unlock()
		return fmt.Errorf("recover conversation work: %w", err)
	}
	for _, principalID := range principals {
		s.notify(principalID)
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
		s.workers = make(map[string]chan struct{})
		s.inflight = make(map[string]context.CancelFunc)
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown conversation service: %w", ctx.Err())
	}
}

func (s *Service) Enqueue(ctx context.Context, input EnqueueInput) error {
	if input.BotUserID <= 0 || input.TelegramUpdateID < 0 || input.TelegramMessageID <= 0 || input.PrincipalID == "" || input.ChatID <= 0 {
		return fmt.Errorf("conversation request identity is invalid")
	}
	if !utf8.ValidString(input.Message) || utf8.RuneCountInString(input.Message) > maximumMessageRunes {
		return fmt.Errorf("conversation message must be valid UTF-8 within 4096 characters")
	}
	// A photo or a file carries the request on its own, so only a message with neither is empty.
	if strings.TrimSpace(input.Message) == "" && len(input.Attachments) == 0 {
		return fmt.Errorf("conversation request must carry a message or an attachment")
	}
	if len(input.Attachments) > 0 && s.config.Attachments == nil {
		return fmt.Errorf("conversation attachments are not supported")
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
	if !result.Created {
		s.config.Attachments.Discard(staged)
	}
	if err != nil {
		return fmt.Errorf("persist conversation request: %w", err)
	}
	if result.Busy {
		return ErrBusy
	}
	if result.Request.State == RequestStateQueued || result.Request.State == RequestStateReady {
		s.notify(input.PrincipalID)
	}
	return nil
}

func (s *Service) Cancel(ctx context.Context, principalID string) (bool, error) {
	cancelled, err := s.repository.CancelPendingConversation(ctx, principalID)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	cancel := s.inflight[principalID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return cancelled, nil
}

func (s *Service) NewSession(ctx context.Context, principalID string) (bool, error) {
	sessionID, reset, err := s.repository.ResetConversation(ctx, principalID)
	if err != nil {
		return false, err
	}
	if reset && s.config.Attachments != nil {
		if err := s.config.Attachments.CloseSession(ctx, principalID, sessionID); err != nil {
			return false, err
		}
	}
	if s.config.Cards != nil {
		s.config.Cards.Discard(principalID)
	}
	s.mu.Lock()
	cancel := s.inflight[principalID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return reset, nil
}

func (s *Service) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx != nil && s.cancel != nil
}

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

func (s *Service) runWorker(ctx context.Context, principalID string, wake <-chan struct{}) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}

		for ctx.Err() == nil {
			request, found, err := s.repository.NextConversationRequest(ctx, principalID)
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

	turnCtx, cancel := context.WithTimeout(tools.WithTurn(ctx, outbox.ChatTurn(request.ID)), s.config.TurnTimeout)
	s.mu.Lock()
	s.inflight[request.PrincipalID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.inflight, request.PrincipalID)
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

	turnCtx, endTurn := s.telemetry.StartConversationTurn(turnCtx)

	errorKind := ""
	// The stored kind stays within the schema's allowed set; telemetry and logs carry the finer category.
	reportedKind := ""
	reply := ""
	connected, runErr := s.gate.Ready(turnCtx, request.PrincipalID)
	var tools agentruntime.ToolSet
	var webSearch bool
	var attachments attachment.Turn
	if runErr == nil && connected {
		_ = s.deliverer.SendTyping(turnCtx, request.ChatID)
		tools, runErr = s.toolProvider.ToolsFor(turnCtx, request.PrincipalID)
		if runErr == nil {
			webSearch, runErr = s.webSearchEnabled(turnCtx, request.PrincipalID)
		}
		if runErr != nil {
			errorKind = "tool_provider"
		}
		if runErr == nil && s.config.Attachments != nil {
			attachments, runErr = s.config.Attachments.Turn(turnCtx, request.PrincipalID, request.ID)
			if runErr != nil {
				errorKind, reportedKind = "runtime", "attachments"
			}
		}
	} else if runErr == nil {
		errorKind, reportedKind = "runtime", "not_connected"
		reply = NotConnectedReply
	} else {
		errorKind = "runtime"
	}
	if errorKind != "" {
		if reply == "" {
			reply = UnavailableReply
		}
	} else {
		result, err := s.runtime.Run(turnCtx, agentruntime.RunRequest{
			PrincipalID: request.PrincipalID,
			SessionID:   request.RuntimeSessionID,
			Message:     s.turnMessage(request, attachments.Summary),
			Images:      attachments.Images,
			Tools:       tools,
			WebSearch:   webSearch,
		}, s.turnSink(request.PrincipalID, webSearch))
		runErr = err
		switch {
		case errors.Is(turnCtx.Err(), context.DeadlineExceeded):
			errorKind = "timeout"
			reply = TimeoutReply
		case errors.Is(turnCtx.Err(), context.Canceled):
			s.removeCard(request)
			endTurn("cancelled", "cancelled")
			return
		case runErr != nil || strings.TrimSpace(result.Response) == "":
			errorKind = "runtime"
			reply, reportedKind = failureReply(runErr)
		default:
			reply = result.Response
		}
	}
	if reportedKind == "" {
		reportedKind = errorKind
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

// turnMessage prefixes what became of the principal's last confirmation request, which was
// resolved between turns and which the model would otherwise have to guess at, and appends what
// the message carried so the model can see its files without reading them.
func (s *Service) turnMessage(request Request, attachments string) string {
	message := request.Message
	if s.config.Notice != nil {
		if notice := s.config.Notice.PendingNotice(request.PrincipalID); notice != "" {
			message = "[system] " + notice + "\n\n" + message
		}
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
	s.mu.Lock()
	s.inflight[request.PrincipalID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.inflight, request.PrincipalID)
		s.mu.Unlock()
	}()

	claimed, err := s.repository.ClaimConversationDelivery(deliveryCtx, request.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	replyTo := request.TelegramMessageID
	chunks := SplitReply(request.ResponseText)
	// A failed turn's text explains the failure, so a card it staged would illustrate nothing.
	if s.config.Cards != nil && request.ErrorKind == "" {
		chunks, replyTo, err = SendCard(deliveryCtx, s.telemetry.ContextLogger(deliveryCtx, s.logger), s.config.Cards, s.config.Photos, request.PrincipalID, outbox.ChatTurn(request.ID), request.ChatID, replyTo, chunks)
		if err != nil {
			_ = s.repository.ReleaseConversationDelivery(ctx, request.ID)
			return err
		}
	}
	for _, chunk := range chunks {
		if err := s.deliverer.SendReply(deliveryCtx, request.ChatID, replyTo, chunk); err != nil {
			if isPermanentDeliveryError(err) {
				s.removeCard(request)
				return s.repository.CancelConversationDelivery(ctx, request.ID)
			}
			_ = s.repository.ReleaseConversationDelivery(ctx, request.ID)
			return err
		}
		replyTo = 0
	}
	if err := s.repository.MarkConversationDelivered(deliveryCtx, request.ID); err != nil {
		_ = s.repository.ReleaseConversationDelivery(ctx, request.ID)
		return err
	}
	s.removeCard(request)
	if s.config.FollowUp != nil {
		if err := s.config.FollowUp.AfterDelivery(deliveryCtx, request.PrincipalID, request.ChatID); err != nil && ctx.Err() == nil {
			s.logWorkerError(ctx, request.PrincipalID, "follow_up")
		}
	}
	return nil
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
	var providerErr *agentruntime.ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.Category {
		case agentruntime.FailureQuota:
			return QuotaReply, string(agentruntime.FailureQuota)
		case agentruntime.FailureAuth:
			return ReconnectReply, string(agentruntime.FailureAuth)
		}
	}
	return UnavailableReply, "runtime"
}

// awaitTurn waits for the principal's turn slot. While another turn holds it the user sees the
// typing indicator, and /cancel or /new can still stop the wait.
func (s *Service) awaitTurn(ctx context.Context, request Request) (func(), bool) {
	slot := s.turnSlot(request.PrincipalID)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	default:
	}

	waitCtx, stopWaiting := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight[request.PrincipalID] = stopWaiting
	s.mu.Unlock()
	defer func() {
		stopWaiting()
		s.mu.Lock()
		delete(s.inflight, request.PrincipalID)
		s.mu.Unlock()
	}()
	_ = s.deliverer.SendTyping(waitCtx, request.ChatID)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-waitCtx.Done():
		return nil, false
	}
}

func (s *Service) acquireTurn(ctx context.Context, principalID string) (func(), error) {
	slot := s.turnSlot(principalID)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) turnSlot(principalID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, exists := s.turns[principalID]
	if !exists {
		slot = make(chan struct{}, 1)
		s.turns[principalID] = slot
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

type discardSink struct{}

func (discardSink) Emit(context.Context, agentruntime.Event) error {
	return nil
}

// webSearchTool runs inside the runtime rather than through the broker, so its calls get no
// broker audit line; the turn's sink records each by name instead.
const webSearchTool = "web_search"

// turnSink discards runtime events except that a turn offering web search logs each search call,
// by name only.
func (s *Service) turnSink(principalID string, webSearch bool) agentruntime.EventSink {
	if !webSearch {
		return discardSink{}
	}
	return webSearchSink{principalID: principalID, logger: s.logger, telemetry: s.telemetry}
}

type webSearchSink struct {
	principalID string
	logger      *slog.Logger
	telemetry   Telemetry
}

func (w webSearchSink) Emit(ctx context.Context, event agentruntime.Event) error {
	if event.Type == "tool_call" && event.Text == webSearchTool {
		w.telemetry.ContextLogger(ctx, w.logger).InfoContext(ctx, "conversation.web_search.called", "principal_id", w.principalID, "tool", webSearchTool)
	}
	return nil
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
