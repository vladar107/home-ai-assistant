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

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	maximumMessageRunes = 4096
	maximumReplyRunes   = 4000
	deliveryRetryDelay  = 5 * time.Second
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
	wg       sync.WaitGroup
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
	if !utf8.ValidString(input.Message) || strings.TrimSpace(input.Message) == "" || utf8.RuneCountInString(input.Message) > maximumMessageRunes {
		return fmt.Errorf("conversation message must be valid non-empty UTF-8 within 4096 characters")
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

	result, err := s.repository.EnqueueConversation(ctx, input, Session{
		ID:               sessionID,
		PrincipalID:      input.PrincipalID,
		RuntimeSessionID: runtimeSessionID,
	}, requestID)
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
	reset, err := s.repository.ResetConversation(ctx, principalID)
	if err != nil {
		return false, err
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
	turnCtx, cancel := context.WithTimeout(ctx, s.config.TurnTimeout)
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
	if runErr == nil && connected {
		_ = s.deliverer.SendTyping(turnCtx, request.ChatID)
		tools, runErr = s.toolProvider.ToolsFor(turnCtx, request.PrincipalID)
		if runErr != nil {
			errorKind = "tool_provider"
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
			Message:     request.Message,
			Tools:       tools,
		}, discardSink{})
		runErr = err
		switch {
		case errors.Is(turnCtx.Err(), context.DeadlineExceeded):
			errorKind = "timeout"
			reply = TimeoutReply
		case errors.Is(turnCtx.Err(), context.Canceled):
			endTurn("cancelled", "cancelled")
			return
		case runErr != nil || strings.TrimSpace(result.Response) == "":
			errorKind = "runtime"
			reply = UnavailableReply
			var providerErr *agentruntime.ProviderError
			if errors.As(runErr, &providerErr) {
				switch providerErr.Category {
				case agentruntime.FailureQuota:
					reportedKind, reply = string(agentruntime.FailureQuota), QuotaReply
				case agentruntime.FailureAuth:
					reportedKind, reply = string(agentruntime.FailureAuth), ReconnectReply
				}
			}
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
	for _, chunk := range splitReply(request.ResponseText) {
		if err := s.deliverer.SendReply(deliveryCtx, request.ChatID, replyTo, chunk); err != nil {
			if isPermanentDeliveryError(err) {
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
	if s.config.FollowUp != nil {
		if err := s.config.FollowUp.AfterDelivery(deliveryCtx, request.PrincipalID, request.ChatID); err != nil && ctx.Err() == nil {
			s.logWorkerError(ctx, request.PrincipalID, "follow_up")
		}
	}
	return nil
}

func isPermanentDeliveryError(err error) bool {
	var permanent interface {
		Permanent() bool
	}
	return errors.As(err, &permanent) && permanent.Permanent()
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

func splitReply(reply string) []string {
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
