package telegram

import (
	"cmp"
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

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/connectors/notion"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
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
	SendReply(context.Context, int64, int64, string) error
	GetFile(context.Context, string) (FileReference, error)
	DownloadFile(context.Context, string, int64) ([]byte, error)
	SendKeyboard(context.Context, int64, int64, string, InlineKeyboard) error
	EditKeyboard(context.Context, int64, int64, string, InlineKeyboard) error
	DeleteMessage(context.Context, int64, int64) error
	AnswerCallback(context.Context, string, string) error
	SetMyCommands(context.Context, CommandScope, []BotCommand) error
}

type ConversationController interface {
	Enqueue(context.Context, conversation.EnqueueInput) error
	Cancel(context.Context, string) (bool, error)
	NewSession(context.Context, string) (bool, error)
}

// AttachmentStore reports what the principal's conversation may still hold; nil refuses photos
// and files instead of downloading them.
type AttachmentStore interface {
	Room(context.Context, string) (attachment.Room, error)
}

// ConnectionController is implemented by the model connection service; nil disables the model settings section.
type ConnectionController interface {
	Connect(context.Context, string, int64) (connection.Ceremony, error)
	Status(context.Context, string) (connection.Status, error)
	Disconnect(context.Context, string) (bool, error)
}

// NotionController is implemented by the Notion connector; nil disables the Notion settings section.
type NotionController interface {
	Status(context.Context, string) (notion.Status, error)
	Connect(context.Context, string, string) (notion.Status, error)
	Disconnect(context.Context, string) (bool, error)
	SetWrites(context.Context, string, bool) (notion.Status, error)
	BeginIntake(string)
	ConsumeIntake(string) bool
	ChatConnectEnabled() bool
}

// NASController is implemented by the NAS connector; nil disables the NAS settings section.
type NASController interface {
	Status(context.Context, string) (nas.Status, error)
	SetWrites(context.Context, string, bool) (nas.Status, error)
}

// ApprovalController resolves Confirm and Reject presses on proposal messages; nil ignores them.
type ApprovalController interface {
	// Press returns the text the proposal message should show, or false when the press is not
	// the pressing principal's own proposal.
	Press(ctx context.Context, principalID, action, approvalID string) (string, bool, error)
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
	client        botAPI
	identities    identityResolver
	cursors       cursorStore
	conversation  ConversationController
	attachments   AttachmentStore
	connections   ConnectionController
	notion        NotionController
	nas           NASController
	google        GoogleController
	web           WebController
	typeSafe      TypeSafeController
	ns            NSController
	homeAssistant HomeAssistantController
	bus           BusController
	approvals     ApprovalController
	schedules     ScheduleController
	locations     LocationController
	sections      []settingsSection
	interceptors  []messageInterceptor
	logger        *slog.Logger
	telemetry     Telemetry

	mu     sync.Mutex
	cancel context.CancelFunc
	done   <-chan struct{}
	ready  atomic.Bool
}

func NewService(
	client botAPI,
	identities identityResolver,
	cursors cursorStore,
	conversation ConversationController,
	attachments AttachmentStore,
	connections ConnectionController,
	notion NotionController,
	nasFiles NASController,
	googleAccess GoogleController,
	webAccess WebController,
	nsTrains NSController,
	home HomeAssistantController,
	buses BusController,
	approvals ApprovalController,
	schedules ScheduleController,
	logger *slog.Logger,
	telemetry Telemetry,
) (*Service, error) {
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

	service := &Service{
		client:        client,
		identities:    identities,
		cursors:       cursors,
		conversation:  conversation,
		attachments:   attachments,
		connections:   connections,
		notion:        notion,
		nas:           nasFiles,
		google:        googleAccess,
		web:           webAccess,
		ns:            nsTrains,
		homeAssistant: home,
		bus:           buses,
		approvals:     approvals,
		schedules:     schedules,
		logger:        logger,
		telemetry:     telemetryOrNoop(telemetry),
	}
	if connections != nil {
		service.sections = append(service.sections, modelSection{
			connections: connections, conversation: conversation, logger: logger, telemetry: service.telemetry,
		})
	}
	if notion != nil {
		section := notionSection{notion: notion, logger: logger, telemetry: service.telemetry}
		service.sections = append(service.sections, section)
		service.interceptors = append(service.interceptors, section)
	}
	if nasFiles != nil {
		service.sections = append(service.sections, nasSection{nas: nasFiles, logger: logger, telemetry: service.telemetry})
	}
	if googleAccess != nil {
		section := googleSection{google: googleAccess, logger: logger, telemetry: service.telemetry}
		service.sections = append(service.sections, section)
		service.interceptors = append(service.interceptors, section)
	}
	if webAccess != nil {
		service.sections = append(service.sections, webSection{web: webAccess, logger: logger, telemetry: service.telemetry})
	}
	if nsTrains != nil {
		service.sections = append(service.sections, nsSection{ns: nsTrains, logger: logger, telemetry: service.telemetry})
	}
	if home != nil {
		service.sections = append(service.sections, homeAssistantSection{homeAssistant: home, logger: logger, telemetry: service.telemetry})
	}
	if buses != nil {
		service.sections = append(service.sections, busSection{bus: buses, logger: logger, telemetry: service.telemetry})
	}
	return service, nil
}

// SetTypeSafe adds the safety checks section to /settings and its line to /privacy; call it before
// Start. A nil controller leaves both out.
func (s *Service) SetTypeSafe(typeSafe TypeSafeController) {
	if typeSafe == nil {
		return
	}
	s.typeSafe = typeSafe
	s.sections = append(s.sections, typeSafeSection{typeSafe: typeSafe})
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

	commands := registeredCommands(s.conversation != nil, len(s.sections) > 0, s.schedules != nil)
	// Private chats resolve the all-private-chats scope before the default one, so a stale list there would hide the default registration.
	for _, scope := range []CommandScope{"", CommandScopeAllPrivateChats} {
		if err := s.client.SetMyCommands(ctx, scope, commands); err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.commands.register_failed", "scope", cmp.Or(string(scope), "default"), "error", err)
		}
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
		result, failureReason, failureErr := s.processUpdate(updateCtx, bot, update)
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
		case "telegram.command.undeliverable", "telegram.callback.undeliverable":
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

func (s *Service) processUpdate(ctx context.Context, bot Bot, update Update) (processingResult, string, error) {
	if update.CallbackQuery != nil {
		return s.processCallback(ctx, update.CallbackQuery)
	}
	if update.EditedMessage != nil {
		return s.processEdit(ctx, update.EditedMessage)
	}
	if update.Message == nil {
		return ignoredResult("non_message"), "", nil
	}

	message := update.Message
	if reason := validatePrivateMessage(message.From, message.Chat, message.MessageID); reason != "" {
		return ignoredResult(reason), "", nil
	}
	authorization, reason, failure, err := s.resolvePrincipal(ctx, message.From)
	if err != nil {
		return processingResult{}, failure, err
	}
	if reason != "" {
		return ignoredResult(reason), "", nil
	}

	command, isCommand := ParseCommand(message.Text, bot.Username)
	if isCommand {
		reply, keyboard, err := s.handleCommand(ctx, authorization.Principal.ID, command)
		if err != nil {
			return processingResult{}, "handle_command", err
		}
		return s.sendCommandReply(ctx, message, authorization.Principal.ID, command, reply, keyboard)
	}
	if strings.HasPrefix(strings.TrimSpace(message.Text), "/") {
		return ignoredResult("command_for_other_bot"), "", nil
	}

	if message.Location != nil || message.Venue != nil {
		return s.processLocation(ctx, message, authorization.Principal.ID)
	}

	carried, unsupported := classifyMedia(message)
	text := cmp.Or(message.Text, message.Caption)
	if s.conversation == nil || (unsupported == "" && len(carried) == 0 && strings.TrimSpace(text) == "") {
		return processingResult{
			event:       "telegram.update.ignored",
			reason:      "unhandled_text",
			principalID: authorization.Principal.ID,
		}, "", nil
	}
	if unsupported != "" {
		return s.sendMediaReply(ctx, message, authorization.Principal.ID, unsupported, unsupportedMediaReply(unsupported))
	}

	var attachments []attachment.Incoming
	if len(carried) > 0 {
		files, refusal, err := s.fetchMedia(ctx, authorization.Principal.ID, carried)
		if err != nil {
			return processingResult{}, "fetch_media", err
		}
		if refusal != "" {
			return s.sendMediaReply(ctx, message, authorization.Principal.ID, "refused", refusal)
		}
		attachments = files
	} else {
		for _, interceptor := range s.interceptors {
			result, err := interceptor.intercept(ctx, authorization.Principal.ID, text)
			if err != nil {
				return processingResult{}, "intercept_message", err
			}
			if result.handled {
				return s.finishInterception(ctx, message, authorization.Principal.ID, result)
			}
		}
	}

	err = s.conversation.Enqueue(ctx, conversation.EnqueueInput{
		BotUserID:         bot.ID,
		TelegramUpdateID:  update.UpdateID,
		TelegramMessageID: message.MessageID,
		PrincipalID:       authorization.Principal.ID,
		ChatID:            message.Chat.ID,
		Message:           text,
		Attachments:       attachments,
	})
	if errors.Is(err, conversation.ErrBusy) {
		return s.sendCommandReply(ctx, message, authorization.Principal.ID, "busy", conversation.BusyReply, nil)
	}
	if err != nil {
		return processingResult{}, "enqueue_conversation", err
	}
	return processingResult{
		event:       "telegram.conversation.queued",
		principalID: authorization.Principal.ID,
	}, "", nil
}

// sendMediaReply answers a message carrying something that cannot become a turn, quoting it so
// the user sees which message was refused.
func (s *Service) sendMediaReply(ctx context.Context, message *Message, principalID, reason, reply string) (processingResult, string, error) {
	result := processingResult{event: "telegram.media.refused", reason: reason, principalID: principalID}
	if err := s.client.SendReply(ctx, message.Chat.ID, message.MessageID, reply); err != nil {
		if isPermanentAPIError(err) {
			result.event, result.err = "telegram.command.undeliverable", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// finishInterception removes the consumed message when the interceptor asked for it and sends the
// reply without a quote. A failed deletion is reported to the user rather than retried, because the
// message has already been consumed.
func (s *Service) finishInterception(ctx context.Context, message *Message, principalID string, result interception) (processingResult, string, error) {
	reply := result.reply
	if result.remove {
		if err := s.client.DeleteMessage(ctx, message.Chat.ID, message.MessageID); err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.message.delete_failed", "principal_id", principalID, "reason", result.reason, "error", err)
			reply += " " + DeleteFailedLine
		}
	}
	outcome := processingResult{event: "telegram.message.intercepted", reason: result.reason, principalID: principalID}
	if err := s.client.SendReply(ctx, message.Chat.ID, 0, reply); err != nil {
		if isPermanentAPIError(err) {
			outcome.event, outcome.err = "telegram.command.undeliverable", err
			return outcome, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return outcome, "", nil
}

// validatePrivateMessage returns the ignore reason for input that is not a well-formed message
// from a user in their own private chat, or an empty string.
func validatePrivateMessage(from *User, chat Chat, messageID int64) string {
	if !validSender(from) {
		return "malformed_sender"
	}
	if chat.ID == 0 || chat.Type == "" {
		return "malformed_chat"
	}
	if chat.Type != ChatTypePrivate {
		return "non_private_chat"
	}
	if chat.ID <= 0 || chat.ID != from.ID {
		return "malformed_chat"
	}
	if messageID <= 0 {
		return "malformed_message"
	}
	displayName := strings.TrimSpace(strings.Join([]string{from.FirstName, from.LastName}, " "))
	if !validObservedNames(from.Username, displayName) {
		return "malformed_sender"
	}
	return ""
}

// resolvePrincipal authorizes the sender and records the observed identity. It returns an ignore
// reason for an unauthorized sender, or a failure reason with the error for a store problem.
func (s *Service) resolvePrincipal(ctx context.Context, from *User) (identity.Authorization, string, string, error) {
	authorizeCtx, endAuthorize := s.telemetry.StartStore(ctx, "identity_authorize")
	authorization, err := s.identities.Authorize(authorizeCtx, from.ID)
	endAuthorize(err)
	if err != nil {
		return identity.Authorization{}, "", "authorize_identity", err
	}
	if !authorization.Authorized() {
		return identity.Authorization{}, string(authorization.Status), "", nil
	}

	observeCtx, endObserve := s.telemetry.StartStore(ctx, "identity_observe")
	err = s.identities.Observe(observeCtx, identity.TelegramUser{
		ID:          from.ID,
		Username:    from.Username,
		DisplayName: strings.TrimSpace(strings.Join([]string{from.FirstName, from.LastName}, " ")),
	})
	endObserve(err)
	if err != nil {
		return identity.Authorization{}, "", "observe_identity", err
	}
	return authorization, "", "", nil
}

// processCallback handles a button press on a settings message: the press is answered first, the
// section acts, and the message is refreshed from current state so stale buttons stay harmless.
func (s *Service) processCallback(ctx context.Context, callback *CallbackQuery) (processingResult, string, error) {
	if callback.ID == "" || callback.Message == nil {
		return ignoredResult("malformed_callback"), "", nil
	}
	message := callback.Message
	if reason := validatePrivateMessage(callback.From, message.Chat, message.MessageID); reason != "" {
		return ignoredResult(reason), "", nil
	}
	authorization, reason, failure, err := s.resolvePrincipal(ctx, callback.From)
	if err != nil {
		return processingResult{}, failure, err
	}
	if reason != "" {
		return ignoredResult(reason), "", nil
	}
	principalID := authorization.Principal.ID
	chatID := message.Chat.ID

	if err := s.client.AnswerCallback(ctx, callback.ID, ""); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, "answer_callback", err
	}

	if callback.Data == labelCallbackData {
		return processingResult{event: "telegram.callback.handled", principalID: principalID, command: "label"}, "", nil
	}
	if action, approvalID, isApproval := approval.ParseCallback(callback.Data); isApproval {
		return s.processApprovalPress(ctx, callback, principalID, action, approvalID)
	}
	if action, jobID, page, isJob := parseJobCallback(callback.Data); isJob && s.schedules != nil {
		return s.processJobPress(ctx, callback, principalID, action, jobID, page)
	}
	if snooze, jobID, isSnooze := parseReminderCallback(callback.Data); isSnooze && s.schedules != nil {
		return s.processSnoozePress(ctx, callback, principalID, snooze, jobID)
	}

	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "settings"}
	page, outcome := settingsRootPage, settingsOutcome{}
	if target, isNav := settingsNavFor(callback.Data); isNav {
		page, result.command = target, "settings.nav"
	} else if section, action, known := settingsSectionFor(s.sections, callback.Data); known {
		page = settingsPageOf(s.sections, section.prefix())
		outcome, err = section.act(ctx, principalID, chatID, action)
		switch {
		case errors.Is(err, errUnknownSettingsAction):
			result.reason = "unknown_action"
		case err != nil:
			return processingResult{}, "settings_action", err
		default:
			result.command = section.prefix() + "." + action
		}
	} else {
		result.reason = "unknown_action"
	}

	text, keyboard, err := renderSettings(ctx, principalID, s.sections, page, outcome.note)
	if err != nil {
		return processingResult{}, "render_settings", err
	}
	err = s.client.EditKeyboard(ctx, chatID, message.MessageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		// Telegram no longer allows editing an old message; a fresh settings message replaces it.
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err == nil && outcome.message != "" {
		err = s.client.SendReply(ctx, chatID, 0, outcome.message)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// processApprovalPress resolves a Confirm or Reject press and replaces the proposal message with
// its outcome; a press on another principal's proposal or without an approval service is ignored.
func (s *Service) processApprovalPress(ctx context.Context, callback *CallbackQuery, principalID, action, approvalID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "approval." + action}
	if s.approvals == nil {
		result.event, result.reason = "telegram.update.ignored", "unknown_action"
		return result, "", nil
	}
	text, owned, err := s.approvals.Press(ctx, principalID, action, approvalID)
	if err != nil {
		return processingResult{}, "approval_press", err
	}
	if !owned {
		result.event, result.reason = "telegram.update.ignored", "foreign_approval"
		return result, "", nil
	}
	err = s.client.EditKeyboard(ctx, callback.Message.Chat.ID, callback.Message.MessageID, text, nil)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendReply(ctx, callback.Message.Chat.ID, 0, text)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

// processJobPress acts on one of the principal's jobs and refreshes the /jobs message from current
// state, so a button on an outdated list cannot act on anything but the principal's own job.
func (s *Service) processJobPress(ctx context.Context, callback *CallbackQuery, principalID, action, jobID string, page int) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "jobs." + action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	followUp, err := actOnJob(ctx, principalID, action, jobID, s.schedules)
	if errors.Is(err, errUnknownJobAction) {
		result.command, result.reason, err = "jobs", "unknown_action", nil
	}
	if err != nil {
		return processingResult{}, "jobs_action", err
	}

	text, keyboard, err := renderJobs(ctx, principalID, s.schedules, page)
	if err != nil {
		return processingResult{}, "render_jobs", err
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err == nil && followUp != "" {
		err = s.client.SendReply(ctx, chatID, 0, followUp)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}

func (s *Service) handleCommand(ctx context.Context, principalID string, command string) (string, InlineKeyboard, error) {
	switch command {
	case "status":
		if s.conversation != nil {
			return ConversationStatusReply, nil, nil
		}
		return StatusReply, nil, nil
	case "privacy":
		if s.conversation == nil {
			return PrivacyReply, nil, nil
		}
		reply := ConversationPrivacyReply
		if s.attachments != nil {
			reply += " " + AttachmentPrivacyLine
		}
		if s.nas != nil {
			reply += " " + SentFilePrivacyLine
		}
		if s.notion != nil {
			reply += " " + NotionPrivacyLine
		}
		if s.google != nil {
			reply += " " + GooglePrivacyLine
		}
		if s.schedules != nil {
			reply += " " + SchedulePrivacyLine
		}
		if s.web != nil {
			status, err := s.web.Status(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if status.Enabled {
				reply += " " + WebPrivacyLine
			}
		}
		if s.ns != nil {
			status, err := s.ns.Status(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if status.Enabled {
				reply += " " + NSPrivacyLine
			}
		}
		if s.homeAssistant != nil {
			status, err := s.homeAssistant.Status(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if status.Enabled {
				reply += " " + HomeAssistantPrivacyLine
			}
		}
		if s.bus != nil {
			status, err := s.bus.Status(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if status.Enabled {
				reply += " " + BusPrivacyLine + " " + BusPlanningPrivacyLine
			}
		}
		if s.locations != nil {
			reply += " " + LocationPrivacyLine
		}
		if s.typeSafe != nil {
			enabled, err := s.typeSafe.Status(ctx, principalID)
			if err != nil {
				return "", nil, err
			}
			if enabled {
				reply += " " + TypeSafePrivacyLine
			}
		}
		return reply, nil, nil
	case "cancel":
		if s.conversation == nil {
			return NothingToCancelReply, nil, nil
		}
		cancelled, err := s.conversation.Cancel(ctx, principalID)
		if err != nil {
			return "", nil, err
		}
		if cancelled {
			return CancelledReply, nil, nil
		}
		return NothingToCancelReply, nil, nil
	case "new":
		if s.conversation == nil {
			return NoSessionReply, nil, nil
		}
		reset, err := s.conversation.NewSession(ctx, principalID)
		if err != nil {
			return "", nil, err
		}
		if reset {
			return NewSessionReply, nil, nil
		}
		return NoSessionReply, nil, nil
	case "jobs":
		if s.schedules == nil {
			return HelpReply, nil, nil
		}
		return renderJobs(ctx, principalID, s.schedules, 1)
	case "settings":
		if len(s.sections) == 0 {
			return HelpReply, nil, nil
		}
		text, keyboard, err := renderSettings(ctx, principalID, s.sections, settingsRootPage, "")
		if err != nil {
			return "", nil, err
		}
		return text, keyboard, nil
	default:
		return HelpReply, nil, nil
	}
}

func (s *Service) sendCommandReply(
	ctx context.Context,
	message *Message,
	principalID string,
	command string,
	reply string,
	keyboard InlineKeyboard,
) (processingResult, string, error) {
	var err error
	if len(keyboard) > 0 {
		err = s.client.SendKeyboard(ctx, message.Chat.ID, message.MessageID, reply, keyboard)
	} else {
		err = s.client.SendReply(ctx, message.Chat.ID, message.MessageID, reply)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			return processingResult{
				event:       "telegram.command.undeliverable",
				reason:      "send_rejected",
				principalID: principalID,
				command:     command,
				err:         err,
			}, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return processingResult{
		event:       "telegram.command.handled",
		principalID: principalID,
		command:     command,
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
