package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	initialRetryBackoff  = time.Second
	maximumRetryBackoff  = 30 * time.Second
	maximumUsernameRunes = 64
	maximumDisplayRunes  = 200
)

type botAPI interface {
	GetMe(context.Context) (Bot, error)
	GetUpdates(context.Context, int64) ([]Update, error)
	SendMessage(context.Context, int64, string) error
}

type identityResolver interface {
	Observe(context.Context, identity.TelegramUser) error
	Authorize(context.Context, int64) (identity.Authorization, error)
}

type cursorStore interface {
	LoadTelegramCursor(context.Context, int64) (sqlite.TelegramCursor, bool, error)
	AdvanceTelegramCursor(context.Context, int64, int64) error
}

type Service struct {
	client     botAPI
	identities identityResolver
	cursors    cursorStore
	logger     *slog.Logger
	telemetry  Telemetry

	mu     sync.Mutex
	cancel context.CancelFunc
	done   <-chan struct{}
	ready  atomic.Bool
}

func NewService(client botAPI, identities identityResolver, cursors cursorStore, logger *slog.Logger, telemetry Telemetry) (*Service, error) {
	if client == nil {
		return nil, fmt.Errorf("Telegram client is required")
	}
	if identities == nil {
		return nil, fmt.Errorf("Telegram identity service is required")
	}
	if cursors == nil {
		return nil, fmt.Errorf("Telegram cursor store is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("Telegram logger is required")
	}

	return &Service{
		client:     client,
		identities: identities,
		cursors:    cursors,
		logger:     logger,
		telemetry:  telemetryOrNoop(telemetry),
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done != nil {
		return fmt.Errorf("Telegram service is already running")
	}

	bot, err := s.client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("validate Telegram bot credentials: %w", err)
	}
	if bot.ID <= 0 || bot.Username == "" || bot.Username != strings.TrimSpace(bot.Username) {
		return fmt.Errorf("Telegram getMe returned an invalid bot identity")
	}

	loadCursorCtx, endLoadCursor := s.telemetry.StartStore(ctx, "telegram_cursor_load")
	cursor, found, err := s.cursors.LoadTelegramCursor(loadCursorCtx, bot.ID)
	endLoadCursor(err)
	if err != nil {
		return fmt.Errorf("load Telegram polling cursor: %w", err)
	}
	lastCommittedUpdateID := int64(-1)
	if found {
		if cursor.LastCommittedUpdateID == math.MaxInt64 {
			return fmt.Errorf("Telegram polling cursor cannot advance")
		}
		lastCommittedUpdateID = cursor.LastCommittedUpdateID
	}

	pollerCtx := oteltrace.ContextWithSpanContext(ctx, oteltrace.SpanContext{})
	runCtx, cancel := context.WithCancel(pollerCtx)
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.ready.Store(true)

	go s.run(runCtx, done, bot, lastCommittedUpdateID)
	return nil
}

func (s *Service) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.ready.Load() {
		return fmt.Errorf("Telegram poller is not ready")
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.ready.Store(false)

	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	if done == nil {
		return nil
	}
	cancel()

	select {
	case <-done:
		s.mu.Lock()
		if s.done == done {
			s.cancel = nil
			s.done = nil
		}
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown Telegram poller: %w", ctx.Err())
	}
}

func (s *Service) run(ctx context.Context, done chan<- struct{}, bot Bot, lastCommittedUpdateID int64) {
	defer close(done)
	defer s.ready.Store(false)

	s.logger.Info("telegram.poller.started")
	defer s.logger.Info("telegram.poller.stopped")

	offset := lastCommittedUpdateID + 1
	backoff := initialRetryBackoff
	for {
		updates, err := s.client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.retry(ctx, "get_updates", err, backoff)
			if !waitForRetry(ctx, backoff) {
				return
			}
			backoff = nextRetryBackoff(backoff)
			continue
		}

		nextOffset, failureReason, failureErr := s.processBatch(ctx, bot, updates, lastCommittedUpdateID)
		if nextOffset > offset {
			offset = nextOffset
			lastCommittedUpdateID = nextOffset - 1
		}
		if failureReason != "" {
			if ctx.Err() != nil {
				return
			}
			s.retry(ctx, failureReason, failureErr, backoff)
			if !waitForRetry(ctx, backoff) {
				return
			}
			backoff = nextRetryBackoff(backoff)
			continue
		}

		backoff = initialRetryBackoff
		s.ready.Store(true)
	}
}

func (s *Service) processBatch(ctx context.Context, bot Bot, updates []Update, lastCommittedUpdateID int64) (int64, string, error) {
	slices.SortStableFunc(updates, func(left, right Update) int {
		switch {
		case left.UpdateID < right.UpdateID:
			return -1
		case left.UpdateID > right.UpdateID:
			return 1
		default:
			return 0
		}
	})

	nextOffset := lastCommittedUpdateID + 1
	for _, update := range updates {
		if update.UpdateID < 0 || update.UpdateID == math.MaxInt64 {
			_, endUpdate := s.telemetry.StartTelegramUpdate(ctx)
			err := errors.New("Telegram update has an invalid update ID")
			endUpdate("failed", "invalid_update", "", err)
			return nextOffset, "invalid_update", err
		}
		if update.UpdateID <= lastCommittedUpdateID {
			continue
		}

		updateCtx, endUpdate := s.telemetry.StartTelegramUpdate(ctx)
		result, failureReason, failureErr := s.processUpdate(updateCtx, bot.Username, update)
		if failureReason != "" {
			endUpdate("failed", failureReason, "", failureErr)
			return nextOffset, failureReason, failureErr
		}

		// A successful send is completed before its cursor commit so an ambiguous
		// delivery is retried instead of silently losing the reply.
		advanceCtx, endAdvance := s.telemetry.StartStore(updateCtx, "telegram_cursor_advance")
		err := s.cursors.AdvanceTelegramCursor(advanceCtx, bot.ID, update.UpdateID)
		endAdvance(err)
		if err != nil {
			endUpdate("failed", "advance_cursor", result.command, err)
			return nextOffset, "advance_cursor", err
		}

		lastCommittedUpdateID = update.UpdateID
		nextOffset = update.UpdateID + 1
		outcome := "handled"
		switch result.event {
		case "telegram.update.ignored":
			outcome = "ignored"
		case "telegram.command.undeliverable":
			outcome = "undeliverable"
		}
		endUpdate(outcome, result.reason, result.command, result.err)
		s.logResult(updateCtx, result)
	}

	return nextOffset, "", nil
}

type processingResult struct {
	event       string
	reason      string
	principalID string
	command     string
	err         error
}

func (s *Service) processUpdate(ctx context.Context, botUsername string, update Update) (processingResult, string, error) {
	if update.Message == nil {
		return ignoredResult("non_message"), "", nil
	}

	message := update.Message
	if !validSender(message.From) {
		return ignoredResult("malformed_sender"), "", nil
	}
	if message.Chat.ID == 0 || message.Chat.Type == "" {
		return ignoredResult("malformed_chat"), "", nil
	}
	if message.Chat.Type != ChatTypePrivate {
		return ignoredResult("non_private_chat"), "", nil
	}
	if message.Chat.ID <= 0 || message.Chat.ID != message.From.ID {
		return ignoredResult("malformed_chat"), "", nil
	}

	displayName := strings.TrimSpace(strings.Join([]string{message.From.FirstName, message.From.LastName}, " "))
	if !validObservedNames(message.From.Username, displayName) {
		return ignoredResult("malformed_sender"), "", nil
	}

	authorizeCtx, endAuthorize := s.telemetry.StartStore(ctx, "identity_authorize")
	authorization, err := s.identities.Authorize(authorizeCtx, message.From.ID)
	endAuthorize(err)
	if err != nil {
		return processingResult{}, "authorize_identity", err
	}
	if !authorization.Authorized() {
		return ignoredResult(string(authorization.Status)), "", nil
	}

	observeCtx, endObserve := s.telemetry.StartStore(ctx, "identity_observe")
	err = s.identities.Observe(observeCtx, identity.TelegramUser{
		ID:          message.From.ID,
		Username:    message.From.Username,
		DisplayName: displayName,
	})
	endObserve(err)
	if err != nil {
		return processingResult{}, "observe_identity", err
	}

	reply, handled := CommandReply(message.Text, botUsername)
	if !handled {
		return processingResult{
			event:       "telegram.update.ignored",
			reason:      "unhandled_text",
			principalID: authorization.Principal.ID,
		}, "", nil
	}

	if err := s.client.SendMessage(ctx, message.Chat.ID, reply); err != nil {
		if isPermanentAPIError(err) {
			return processingResult{
				event:       "telegram.command.undeliverable",
				reason:      "send_rejected",
				principalID: authorization.Principal.ID,
				command:     commandName(reply),
				err:         err,
			}, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return processingResult{
		event:       "telegram.command.handled",
		principalID: authorization.Principal.ID,
		command:     commandName(reply),
	}, "", nil
}

func validSender(user *User) bool {
	return user != nil && user.ID > 0
}

func validObservedNames(username, displayName string) bool {
	if username != strings.TrimSpace(username) || utf8.RuneCountInString(username) > maximumUsernameRunes {
		return false
	}
	return utf8.RuneCountInString(displayName) <= maximumDisplayRunes
}

func ignoredResult(reason string) processingResult {
	return processingResult{event: "telegram.update.ignored", reason: reason}
}

func commandName(reply string) string {
	switch reply {
	case StatusReply:
		return "status"
	case PrivacyReply:
		return "privacy"
	default:
		return "unknown"
	}
}

func (s *Service) logResult(ctx context.Context, result processingResult) {
	attributes := make([]any, 0, 6)
	if result.reason != "" {
		attributes = append(attributes, "reason", result.reason)
	}
	if result.principalID != "" {
		attributes = append(attributes, "principal_id", result.principalID)
	}
	if result.command != "" {
		attributes = append(attributes, "command", result.command)
	}
	if result.err != nil {
		attributes = append(attributes, "error", result.err)
	}
	s.telemetry.ContextLogger(ctx, s.logger).Info(result.event, attributes...)
}

func (s *Service) retry(ctx context.Context, reason string, err error, backoff time.Duration) {
	s.ready.Store(false)
	s.telemetry.RecordTelegramRetry(ctx, reason)
	s.telemetry.ContextLogger(ctx, s.logger).WarnContext(
		ctx,
		"telegram.poll.retry",
		"reason", reason,
		"error", err,
		"retry_after", backoff,
	)
}

func waitForRetry(ctx context.Context, backoff time.Duration) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func nextRetryBackoff(current time.Duration) time.Duration {
	if current >= maximumRetryBackoff/2 {
		return maximumRetryBackoff
	}
	return current * 2
}
