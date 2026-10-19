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
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/turn"
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
	CreateForumTopic(context.Context, int64, string) (int64, error)
	DeleteForumTopic(context.Context, int64, int64) error
	ForwardMessages(context.Context, int64, []int64) error
	SendInTab(context.Context, int64, string) error
	EditForumTopic(context.Context, int64, int64, string, string) error
	GetForumTopicIconStickers(context.Context) ([]TopicIcon, error)
	SendPlainInTab(context.Context, int64, string) (int64, error)
	PinChatMessage(context.Context, int64, int64) error
}

type ConversationController interface {
	Enqueue(context.Context, conversation.EnqueueInput) error
	Cancel(context.Context, conversation.Lane) (bool, error)
	// CancelAll stops the turns of every tab of the principal.
	CancelAll(context.Context, string) (bool, error)
	// StopDraft cancels the lane's running turn when draftID is the one it shows and reports whether
	// it did.
	StopDraft(ctx context.Context, lane conversation.Lane, draftID int64) (bool, error)
	NewSession(context.Context, conversation.Lane) (bool, error)
	// CloseLane ends the conversation of a tab that was closed or is gone.
	CloseLane(ctx context.Context, lane conversation.Lane, reason string) (bool, error)
	Sessions(context.Context, conversation.Lane) ([]conversation.SessionSummary, error)
	Resume(context.Context, conversation.Lane, string) (conversation.ResumeResult, error)
	// SessionMessages returns the Telegram messages of one of the principal's conversations, oldest
	// first.
	SessionMessages(ctx context.Context, principalID, sessionID string) ([]int64, error)
	// React records a reaction to one of the principal's replies and starts a reconsider turn for a
	// first 👎.
	React(context.Context, conversation.ReactionInput) (conversation.ReactionResult, error)
}

// TabNamer chooses a tab's name and one of icons from a conversation's first exchange, on the
// principal's own subscription.
type TabNamer interface {
	NameTab(ctx context.Context, principalID, message, reply string, icons []string) (turn.TabName, error)
}

// AttachmentStore reports what the principal's conversation may still hold; nil refuses photos
// and files instead of downloading them.
type AttachmentStore interface {
	Room(context.Context, string) (attachment.Room, error)
}

// ConnectionController is implemented by the model connection service, for the model settings
// section.
type ConnectionController interface {
	Connect(context.Context, string, int64) (connection.Ceremony, error)
	Status(context.Context, string) (connection.Status, error)
	Disconnect(context.Context, string) (bool, error)
}

// SwitchController is one integration's per-principal switch, implemented by integration.Toggle,
// for its on/off page in /settings.
type SwitchController interface {
	Enabled(context.Context, string) (bool, error)
	SetEnabled(context.Context, string, bool) error
}

// NotionController is implemented by the Notion connector, for the Notion settings section.
type NotionController interface {
	Status(context.Context, string) (notion.Status, error)
	Connect(context.Context, string, string) (notion.Status, error)
	Disconnect(context.Context, string) (bool, error)
	SetWrites(context.Context, string, bool) (notion.Status, error)
	BeginIntake(string)
	ConsumeIntake(string) bool
	ChatConnectEnabled() bool
}

// NASController is implemented by the NAS connector, for the NAS settings section.
type NASController interface {
	Status(context.Context, string) (nas.Status, error)
	SetWrites(context.Context, string, bool) (nas.Status, error)
}

// ApprovalController resolves Confirm and Reject presses on proposal messages; nil ignores them.
type ApprovalController interface {
	// Press resolves a Confirm or Reject press; a press on another principal's proposal is not owned.
	Press(ctx context.Context, principalID, action, approvalID string) (approval.Pressed, error)
	// Preview starts previews of a bundle's items, posted under its proposal message; the trigger
	// names the press so a retried update starts nothing new.
	Preview(ctx context.Context, principalID, approvalID, trigger string, chatID, messageID int64) (approval.PreviewPressed, error)
	// Undo reverses a direct action; a press on another principal's action is not owned.
	Undo(ctx context.Context, principalID, recordID string) (approval.Undid, error)
	// DeferContinuation keeps a continuation until the tab's in-flight turn is delivered.
	DeferContinuation(principalID string, threadID int64, continuation func(context.Context))
}

type identityResolver interface {
	Observe(context.Context, identity.TelegramUser) error
	Authorize(context.Context, int64) (identity.Authorization, error)
}

type cursorStore interface {
	// LoadTelegramCursor returns the last update the bot committed, and whether it committed one.
	LoadTelegramCursor(context.Context, int64) (int64, bool, error)
	AdvanceTelegramCursor(context.Context, int64, int64) error
}

type Service struct {
	client       botAPI
	identities   identityResolver
	cursors      cursorStore
	conversation ConversationController
	attachments  AttachmentStore
	approvals    ApprovalController
	schedules    ScheduleController
	locations    LocationController
	memory       MemoryController
	backlog      BacklogController
	tasks        TaskController
	watches      WatchController
	trips        TripController
	routes       RouteController
	transcriber  Transcriber
	model        *ModelCommand
	tabs         *Tabs
	sections     []Section
	interceptors []messageInterceptor
	logger       *slog.Logger
	telemetry    Telemetry

	// freshTabs holds the tabs the user just opened, with when, until their first message.
	freshTabs sync.Map

	mu     sync.Mutex
	cancel context.CancelFunc
	done   <-chan struct{}
	ready  atomic.Bool
}

// Dependencies is everything the Telegram service uses. Client, Identities, Cursors, and Logger are
// required; a nil Telemetry records nothing, and any other nil field leaves its feature out.
type Dependencies struct {
	Client     botAPI
	Identities identityResolver
	Cursors    cursorStore
	Logger     *slog.Logger
	Telemetry  Telemetry

	// Tabs holds the tab registry, the feed and trip tabs, and tab naming; without it tabs are not
	// recorded or named and every refused tab is taken for gone.
	Tabs         *Tabs
	Conversation ConversationController
	Attachments  AttachmentStore
	Approvals    ApprovalController
	Schedules    ScheduleController
	Locations    LocationController
	Memory       MemoryController
	Backlog      BacklogController
	Tasks        TaskController
	Watches      WatchController
	Trips        TripController
	Routes       RouteController
	Transcriber  Transcriber
	// Model enables /model; it also needs Conversation.
	Model *ModelCommand

	// Sections are the /settings pages, in any order: settingsGroups places the pages, sectionOrder
	// their app_help topics, and privacyOrder their /privacy lines.
	Sections []Section
}

func NewService(deps Dependencies) (*Service, error) {
	if deps.Client == nil {
		return nil, fmt.Errorf("Telegram client is required")
	}
	if deps.Identities == nil {
		return nil, fmt.Errorf("Telegram identity service is required")
	}
	if deps.Cursors == nil {
		return nil, fmt.Errorf("Telegram cursor store is required")
	}
	if deps.Logger == nil {
		return nil, fmt.Errorf("Telegram logger is required")
	}
	if err := validateSections(deps.Sections); err != nil {
		return nil, err
	}

	service := &Service{
		client:       deps.Client,
		identities:   deps.Identities,
		cursors:      deps.Cursors,
		conversation: deps.Conversation,
		attachments:  deps.Attachments,
		approvals:    deps.Approvals,
		schedules:    deps.Schedules,
		locations:    deps.Locations,
		memory:       deps.Memory,
		backlog:      deps.Backlog,
		tasks:        deps.Tasks,
		watches:      deps.Watches,
		trips:        deps.Trips,
		routes:       deps.Routes,
		transcriber:  deps.Transcriber,
		model:        deps.Model,
		tabs:         cmp.Or(deps.Tabs, NewTabs(deps.Client, nil, nil, nil, deps.Logger, deps.Telemetry)),
		sections:     inSectionOrder(deps.Sections),
		logger:       deps.Logger,
		telemetry:    telemetryOrNoop(deps.Telemetry),
	}
	for _, section := range service.sections {
		if interceptor, ok := section.(messageInterceptor); ok {
			service.interceptors = append(service.interceptors, interceptor)
		}
	}
	return service, nil
}

func inSectionOrder(sections []Section) []Section {
	ordered := slices.Clone(sections)
	slices.SortStableFunc(ordered, func(left, right Section) int {
		return slices.Index(sectionOrder, left.prefix()) - slices.Index(sectionOrder, right.prefix())
	})
	return ordered
}

// validateSections refuses a page /settings could not place or reach, and a /privacy line without
// a place, so a new section cannot silently go missing.
func validateSections(sections []Section) error {
	seen := make(map[string]bool, len(sections))
	for _, section := range sections {
		if section == nil {
			return fmt.Errorf("Telegram settings section is nil")
		}
		prefix := section.prefix()
		if seen[prefix] {
			return fmt.Errorf("Telegram settings section %q is duplicated", prefix)
		}
		seen[prefix] = true
		if !slices.ContainsFunc(settingsGroups, func(group settingsGroup) bool { return slices.Contains(group.members, prefix) }) {
			return fmt.Errorf("Telegram settings section %q is in no settings group", prefix)
		}
		if !slices.Contains(sectionOrder, prefix) {
			return fmt.Errorf("Telegram settings section %q has no place in the section order", prefix)
		}
		if _, private := section.(privacySection); private && !slices.Contains(privacyOrder, prefix) {
			return fmt.Errorf("Telegram settings section %q has privacy lines without a place in /privacy", prefix)
		}
	}
	return nil
}

func (s *Service) botCommands() []BotCommand {
	conversationEnabled := s.conversation != nil
	modelEnabled := conversationEnabled && s.model != nil
	return menuCommands([]menuCommand{
		{BotCommand{Command: "backlog", Description: "Show and export your ideas for developing me"}, s.backlog != nil},
		{BotCommand{Command: "cancel", Description: "Stop the current response"}, conversationEnabled},
		{BotCommand{Command: "close", Description: "Close this tab"}, conversationEnabled && s.tabs.enabled()},
		{BotCommand{Command: "jobs", Description: "Show and manage your reminders and scheduled jobs"}, s.schedules != nil},
		{BotCommand{Command: "memory", Description: "Show and delete what I remember about you"}, s.memory != nil},
		{BotCommand{Command: "model", Description: ModelCommandDescription}, modelEnabled},
		{BotCommand{Command: "new", Description: "Start a fresh conversation"}, conversationEnabled},
		{BotCommand{Command: "sessions", Description: "Return to a recent conversation"}, conversationEnabled},
		{BotCommand{Command: "settings", Description: "Show and change your settings"}, len(s.sections) > 0},
		{BotCommand{Command: "usage", Description: "Show your ChatGPT subscription usage"}, modelEnabled && s.model.Usage != nil},
		{BotCommand{Command: "tasks", Description: "Show and cancel your background tasks"}, s.tasks != nil},
	})
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
	s.tabs.setTopics(bot.HasTopicsEnabled)
	if bot.HasTopicsEnabled {
		s.tabs.refreshTopicIcons(ctx)
	}

	commands := s.botCommands()
	// Private chats resolve the all-private-chats scope before the default one, so a stale list there would hide the default registration.
	for _, scope := range []CommandScope{"", CommandScopeAllPrivateChats} {
		if err := s.client.SetMyCommands(ctx, scope, commands); err != nil {
			s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.commands.register_failed", "scope", cmp.Or(string(scope), "default"), "error", err)
		}
	}

	loadCursorCtx, endLoadCursor := s.telemetry.StartStore(ctx, "telegram_cursor_load")
	committed, found, err := s.cursors.LoadTelegramCursor(loadCursorCtx, bot.ID)
	endLoadCursor(err)
	if err != nil {
		return fmt.Errorf("load Telegram polling cursor: %w", err)
	}
	lastCommittedUpdateID := int64(-1)
	if found {
		if committed == math.MaxInt64 {
			return fmt.Errorf("Telegram polling cursor cannot advance")
		}
		lastCommittedUpdateID = committed
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

// processUpdate handles one update in the tab it came from, so every reply lands beside it. With
// tabs off every update is in the main view.
func (s *Service) processUpdate(ctx context.Context, bot Bot, update Update) (processingResult, string, error) {
	if bot.HasTopicsEnabled {
		ctx = thread.With(ctx, updateThread(update))
	}
	if update.CallbackQuery != nil {
		return s.processCallback(ctx, bot, update.UpdateID, update.CallbackQuery)
	}
	if update.StoppedMessageGeneration != nil {
		return s.processDraftStop(ctx, update.StoppedMessageGeneration)
	}
	if update.MessageReaction != nil {
		return s.processReaction(ctx, bot, update.UpdateID, update.MessageReaction)
	}
	if update.EditedMessage != nil {
		return s.processEdit(ctx, update.EditedMessage)
	}
	if update.Message == nil {
		return ignoredResult("non_message"), "", nil
	}

	message := update.Message
	// A topic service message may come from the bot itself, so it is read before the sender check.
	if message.ForumTopicCreated != nil || message.ForumTopicEdited != nil {
		return s.processTopicService(ctx, bot, message)
	}
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
	s.tabs.noteTab(ctx, bot, authorization.Principal.ID, message)

	command, isCommand := ParseCommand(message.Text, bot.Username)
	if threadID := thread.From(ctx); isCommand && s.openedForCommand(message.Chat.ID, threadID) {
		return s.processMainViewCommand(ctx, bot, message, authorization.Principal.ID, command, threadID)
	}
	s.freshTabs.Delete(chatTab{message.Chat.ID, thread.From(ctx)})
	if isCommand {
		return s.dispatchCommand(ctx, bot, message, authorization.Principal.ID, command)
	}
	if strings.HasPrefix(strings.TrimSpace(message.Text), "/") {
		return ignoredResult("command_for_other_bot"), "", nil
	}

	if message.Location != nil || message.Venue != nil {
		return s.processLocation(ctx, message, authorization.Principal.ID)
	}

	text := cmp.Or(message.Text, message.Caption)
	transcribed := false
	if item, found := voiceRecording(message); found && s.transcriber != nil && s.conversation != nil {
		transcript, reply, reason, err := s.transcribe(ctx, authorization.Principal.ID, item)
		if err != nil {
			return processingResult{}, "transcribe_voice", err
		}
		if reply == "" {
			if result := s.guardSecrets(transcript, message.Caption); result.handled {
				return s.finishInterception(ctx, message, authorization.Principal.ID, result)
			}
			text, reply, reason = voiceTurn(transcript, message.Caption)
		}
		if reply != "" {
			return s.sendMediaReply(ctx, message, authorization.Principal.ID, reason, reply)
		}
		transcribed = true
	}
	var carried []media
	var unsupported string
	if !transcribed {
		carried, unsupported = classifyMedia(message)
	}
	if s.conversation == nil || (unsupported == "" && len(carried) == 0 && strings.TrimSpace(text) == "") {
		return processingResult{
			event:       "telegram.update.ignored",
			reason:      "unhandled_text",
			principalID: authorization.Principal.ID,
		}, "", nil
	}
	if unsupported != "" || len(carried) > 0 {
		if result := s.guardSecrets(message.Caption); result.handled {
			return s.finishInterception(ctx, message, authorization.Principal.ID, result)
		}
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
	} else if !transcribed {
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
		ThreadID:          thread.From(ctx),
		ChatID:            message.Chat.ID,
		Message:           text,
		Attachments:       attachments,
		ReplyToMessageID:  repliedTo(message),
		Forwarded:         message.Forwarded(),
	})
	if errors.Is(err, conversation.ErrBusy) {
		return s.sendCommandReply(ctx, message, authorization.Principal.ID, "busy", conversation.BusyReply, nil)
	}
	if elsewhere := (*conversation.OpenElsewhereError)(nil); errors.As(err, &elsewhere) {
		return s.sendCommandReply(ctx, message, authorization.Principal.ID, "elsewhere", SessionOpenIn(elsewhere.ThreadID, elsewhere.TabName), nil)
	}
	// A retry would be refused the same way and hold every later update behind this one.
	if errors.Is(err, conversation.ErrInvalidRequest) {
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.conversation.refused", "principal_id", authorization.Principal.ID, "error", err)
		return s.sendCommandReply(ctx, message, authorization.Principal.ID, "invalid", InvalidMessageReply, nil)
	}
	if err != nil {
		return processingResult{}, "enqueue_conversation", err
	}
	return processingResult{
		event:       "telegram.conversation.queued",
		principalID: authorization.Principal.ID,
	}, "", nil
}

// guardSecrets runs only the secret guards, for a caption or transcript that cannot be an intake
// reply, so it never consumes a pending intake.
func (s *Service) guardSecrets(texts ...string) interception {
	for _, text := range texts {
		for _, interceptor := range s.interceptors {
			if result := interceptor.guard(text); result.handled {
				return result
			}
		}
	}
	return interception{}
}

// processDraftStop cancels the turn whose draft the user stopped. The update names no user, and a
// private chat's ID is its user's ID, so the chat authorizes like a sender would.
func (s *Service) processDraftStop(ctx context.Context, stopped *MessageGenerationStopped) (processingResult, string, error) {
	if stopped.Chat.Type != ChatTypePrivate || stopped.Chat.ID <= 0 || stopped.DraftID == 0 {
		return ignoredResult("non_private_chat"), "", nil
	}
	if s.conversation == nil {
		return ignoredResult("unhandled_stop"), "", nil
	}
	authorizeCtx, endAuthorize := s.telemetry.StartStore(ctx, "identity_authorize")
	authorization, err := s.identities.Authorize(authorizeCtx, stopped.Chat.ID)
	endAuthorize(err)
	if err != nil {
		return processingResult{}, "authorize_identity", err
	}
	if !authorization.Authorized() {
		return ignoredResult(string(authorization.Status)), "", nil
	}
	principalID := authorization.Principal.ID
	cancelled, err := s.conversation.StopDraft(ctx, laneOf(ctx, principalID), stopped.DraftID)
	if err != nil {
		return processingResult{}, "stop_draft", err
	}
	if !cancelled {
		return processingResult{event: "telegram.draft.stale", principalID: principalID}, "", nil
	}
	result := processingResult{event: "telegram.draft.stopped", principalID: principalID}
	// The turn is already cancelled and a retried update would find its draft gone, so a failed
	// confirmation is only reported.
	if err := s.client.SendReply(ctx, stopped.Chat.ID, 0, CancelledReply); err != nil {
		result.event, result.err = "telegram.command.undeliverable", err
	}
	return result, "", nil
}

// processReaction passes a user's reaction in their own private chat on to the conversation, which
// keeps it only for one of the principal's replies. A reaction from a chat identity names no user and
// is ignored. Only the class of the reaction is reported, never what it reacted to.
func (s *Service) processReaction(ctx context.Context, bot Bot, updateID int64, reaction *MessageReactionUpdated) (processingResult, string, error) {
	if reaction.User == nil {
		return ignoredResult("reaction_without_user"), "", nil
	}
	if reason := validatePrivateMessage(reaction.User, reaction.Chat, reaction.MessageID); reason != "" {
		return ignoredResult(reason), "", nil
	}
	emojis, valid := reactionEmojis(reaction.NewReaction)
	if !valid {
		return ignoredResult("malformed_reaction"), "", nil
	}
	authorization, reason, failure, err := s.resolvePrincipal(ctx, reaction.User)
	if err != nil {
		return processingResult{}, failure, err
	}
	if reason != "" {
		return ignoredResult(reason), "", nil
	}
	principalID := authorization.Principal.ID
	if s.conversation == nil {
		return processingResult{event: "telegram.update.ignored", reason: "unhandled_reaction", principalID: principalID}, "", nil
	}
	reacted, err := s.conversation.React(ctx, conversation.ReactionInput{
		BotUserID:        bot.ID,
		TelegramUpdateID: updateID,
		PrincipalID:      principalID,
		ChatID:           reaction.Chat.ID,
		MessageID:        reaction.MessageID,
		Emojis:           emojis,
	})
	if err != nil {
		return processingResult{}, "record_reaction", err
	}
	result := processingResult{event: "telegram.reaction.recorded", reason: reacted.Class, principalID: principalID, command: "reaction"}
	switch {
	case !reacted.Recorded:
		result.event, result.reason = "telegram.update.ignored", "unknown_message"
	case reacted.Class == "":
		result.event, result.reason = "telegram.reaction.removed", ""
	case reacted.Reconsider == conversation.ReconsiderStarted:
		result.event = "telegram.reaction.reconsider_queued"
	case reacted.Reconsider == conversation.ReconsiderBusy:
		result.event = "telegram.reaction.reconsider_skipped"
	}
	return result, "", nil
}

// reactionEmojis lists a reaction's emoji. A custom or paid reaction has none, so it stands as its
// type, which still counts as a reaction.
func reactionEmojis(reactions []ReactionType) ([]string, bool) {
	emojis := make([]string, 0, len(reactions))
	for _, reaction := range reactions {
		emoji := reaction.Emoji
		if reaction.Type != "emoji" {
			emoji = reaction.Type
		}
		if emoji == "" || !utf8.ValidString(emoji) || utf8.RuneCountInString(emoji) > 32 {
			return nil, false
		}
		emojis = append(emojis, emoji)
	}
	return emojis, true
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

const undoCallbackPrefix = "undo:"

// processCallback handles a button press on a settings message: the press is answered first, the
// section acts, and the message is refreshed from current state so stale buttons stay harmless.
func (s *Service) processCallback(ctx context.Context, bot Bot, updateID int64, callback *CallbackQuery) (processingResult, string, error) {
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

	if action, approvalID, isApproval := approval.ParseCallback(callback.Data); isApproval && action == approval.ActionPreview {
		return s.processPreviewPress(ctx, pressTrigger(bot, updateID), callback, principalID, approvalID)
	}
	if recordID, isUndo := approval.ParseUndoCallback(callback.Data); isUndo {
		return s.processUndoPress(ctx, callback, principalID, recordID)
	}
	if strings.HasPrefix(callback.Data, undoCallbackPrefix) {
		return s.ignoreUnknownPress(ctx, callback, principalID)
	}
	if err := s.client.AnswerCallback(ctx, callback.ID, ""); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, "answer_callback", err
	}

	if callback.Data == labelCallbackData {
		return processingResult{event: "telegram.callback.handled", principalID: principalID, command: "label"}, "", nil
	}
	if action, approvalID, isApproval := approval.ParseCallback(callback.Data); isApproval {
		return s.processApprovalPress(ctx, bot, updateID, callback, principalID, action, approvalID)
	}
	if action, jobID, page, isJob := parseJobCallback(callback.Data); isJob && s.schedules != nil {
		return s.processJobPress(ctx, pressTrigger(bot, updateID), callback, principalID, action, jobID, page)
	}
	if press, isMemory := parseMemoryCallback(callback.Data); isMemory && s.memory != nil {
		return s.processMemoryPress(ctx, callback, principalID, press)
	}
	if press, isBacklog := parseBacklogCallback(callback.Data); isBacklog && s.backlog != nil {
		return s.processBacklogPress(ctx, callback, principalID, press)
	}
	if snooze, jobID, isSnooze := parseReminderCallback(callback.Data); isSnooze && s.schedules != nil {
		return s.processSnoozePress(ctx, callback, principalID, snooze, jobID)
	}
	if action, watchID, isWatch := parseWatchCallback(callback.Data); isWatch && s.watches != nil {
		return s.processWatchPress(ctx, callback, principalID, action, watchID)
	}
	if action, tripID, isTrip := parseTripCallback(callback.Data); isTrip && s.trips != nil {
		return s.processTripPress(ctx, callback, principalID, action, tripID)
	}
	if action, sessionID, isSession := parseSessionCallback(callback.Data); isSession && s.conversation != nil {
		return s.processSessionPress(ctx, bot, callback, principalID, action, sessionID)
	}
	if action, threadID, isTab := parseTabCallback(callback.Data); isTab && s.conversation != nil && bot.HasTopicsEnabled {
		return s.processTabPress(ctx, callback, principalID, action, threadID)
	}
	if press, isModel := parseModelCallback(callback.Data); isModel && s.conversation != nil && s.model != nil {
		return s.processModelPress(ctx, callback, principalID, press)
	}
	if taskID, isTask := parseTaskCallback(callback.Data); isTask && s.tasks != nil {
		return s.processTaskPress(ctx, callback, principalID, taskID)
	}

	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "settings"}
	sections := sectionsFor(s.sections, principalID)
	page, outcome := settingsRootPage, settingsOutcome{}
	if target, isNav := settingsNavFor(callback.Data); isNav {
		page, result.command = target, "settings.nav"
	} else if section, action, known := settingsSectionFor(sections, callback.Data); known {
		page = settingsPageOf(sections, section.prefix())
		outcome, err = section.act(withPressTrigger(ctx, pressTrigger(bot, updateID)), principalID, chatID, action)
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

	text, keyboard, err := renderSettings(ctx, principalID, sections, page, outcome.note)
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
// A press that ran the actions queues a turn that continues the request from their outcome.
func (s *Service) processApprovalPress(ctx context.Context, bot Bot, updateID int64, callback *CallbackQuery, principalID, action, approvalID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "approval." + action}
	if s.approvals == nil {
		result.event, result.reason = "telegram.update.ignored", "unknown_action"
		return result, "", nil
	}
	pressed, err := s.approvals.Press(ctx, principalID, action, approvalID)
	if err != nil {
		return processingResult{}, "approval_press", err
	}
	if !pressed.Owned {
		result.event, result.reason = "telegram.update.ignored", "foreign_approval"
		return result, "", nil
	}
	if pressed.Ran {
		s.continueConversation(ctx, bot, updateID, callback, principalID)
	}
	err = s.client.EditKeyboard(ctx, callback.Message.Chat.ID, callback.Message.MessageID, pressed.Text, nil)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendReply(ctx, callback.Message.Chat.ID, 0, pressed.Text)
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

// processUndoPress reverses a direct action and answers the press with the outcome; the approval
// service updates the "Done" message itself.
func (s *Service) processUndoPress(ctx context.Context, callback *CallbackQuery, principalID, recordID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "approval.undo"}
	var undid approval.Undid
	if s.approvals != nil {
		var err error
		undid, err = s.approvals.Undo(ctx, principalID, recordID)
		if err != nil {
			return processingResult{}, "approval_undo", err
		}
	}
	if err := s.client.AnswerCallback(ctx, callback.ID, undid.Answer); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, "answer_callback", err
	}
	switch {
	case s.approvals == nil:
		result.event, result.reason = "telegram.update.ignored", "unknown_action"
	case !undid.Owned:
		result.event, result.reason = "telegram.update.ignored", "foreign_approval"
	}
	return result, "", nil
}

// ignoreUnknownPress answers a press whose data no handler accepts without touching the message.
func (s *Service) ignoreUnknownPress(ctx context.Context, callback *CallbackQuery, principalID string) (processingResult, string, error) {
	if err := s.client.AnswerCallback(ctx, callback.ID, ""); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, "answer_callback", err
	}
	return processingResult{event: "telegram.update.ignored", principalID: principalID, reason: "unknown_action"}, "", nil
}

// processPreviewPress starts a preview of the pressed bundle and answers the press with what it
// did, leaving the proposal message and its buttons as they are. The press is answered after the
// preview starts, so the answer can say whether one was already running; a retried update gets the
// same answer and starts nothing.
func (s *Service) processPreviewPress(ctx context.Context, trigger string, callback *CallbackQuery, principalID, approvalID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "approval." + approval.ActionPreview}
	var pressed approval.PreviewPressed
	if s.approvals != nil {
		var err error
		pressed, err = s.approvals.Preview(ctx, principalID, approvalID, trigger, callback.Message.Chat.ID, callback.Message.MessageID)
		if err != nil {
			return processingResult{}, "approval_preview", err
		}
	}
	if err := s.client.AnswerCallback(ctx, callback.ID, pressed.Text); err != nil && !isPermanentAPIError(err) {
		return processingResult{}, "answer_callback", err
	}
	switch {
	case s.approvals == nil:
		result.event, result.reason = "telegram.update.ignored", "unknown_action"
	case !pressed.Owned:
		result.event, result.reason = "telegram.update.ignored", "foreign_approval"
	}
	return result, "", nil
}

// continueConversation queues the turn after a confirmed proposal. It is queued before the message
// edit, because a replayed press finds the bundle resolved and would not queue it again. While
// another turn is in flight it is deferred until that turn is delivered. Failing to queue costs
// only the automatic step: the outcome still reaches the user's next turn as a notice.
func (s *Service) continueConversation(ctx context.Context, bot Bot, updateID int64, callback *CallbackQuery, principalID string) {
	if s.conversation == nil {
		return
	}
	input := conversation.EnqueueInput{
		BotUserID:         bot.ID,
		TelegramUpdateID:  updateID,
		TelegramMessageID: callback.Message.MessageID,
		PrincipalID:       principalID,
		ThreadID:          thread.From(ctx),
		ChatID:            callback.Message.Chat.ID,
		Message:           prompt.Continue,
	}
	err := s.conversation.Enqueue(ctx, input)
	if errors.Is(err, conversation.ErrBusy) && s.approvals != nil {
		s.approvals.DeferContinuation(principalID, input.ThreadID, func(ctx context.Context) {
			s.continueConversation(ctx, bot, updateID, callback, principalID)
		})
		// The busy turn may have been delivered before the deferral was stored, so nothing would
		// run it; queueing again closes that gap, and the turn it queues drops the deferral.
		if err = s.conversation.Enqueue(ctx, input); errors.Is(err, conversation.ErrBusy) {
			s.telemetry.ContextLogger(ctx, s.logger).InfoContext(ctx, "telegram.continuation.deferred", "principal_id", principalID, "reason", "busy")
			return
		}
	}
	logger := s.telemetry.ContextLogger(ctx, s.logger)
	switch {
	case errors.Is(err, conversation.ErrBusy):
		logger.InfoContext(ctx, "telegram.continuation.skipped", "principal_id", principalID, "reason", "busy")
	case err != nil:
		logger.WarnContext(ctx, "telegram.continuation.failed", "principal_id", principalID, "error", err)
	default:
		logger.InfoContext(ctx, "telegram.continuation.queued", "principal_id", principalID)
	}
}

// pressTrigger names a button press uniquely across retries of its update.
func pressTrigger(bot Bot, updateID int64) string {
	return fmt.Sprintf("telegram:%d:%d", bot.ID, updateID)
}

// processJobPress acts on one of the principal's jobs and refreshes the /jobs message from current
// state, so a button on an outdated list cannot act on anything but the principal's own job.
func (s *Service) processJobPress(ctx context.Context, trigger string, callback *CallbackQuery, principalID, action, jobID string, page int) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "jobs." + action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	followUp, err := actOnJob(ctx, principalID, action, jobID, trigger, s.schedules, s.watches, s.routes)
	if errors.Is(err, errUnknownJobAction) {
		result.command, result.reason, err = "jobs", "unknown_action", nil
	}
	if err != nil {
		return processingResult{}, "jobs_action", err
	}

	text, keyboard, err := renderJobs(ctx, principalID, s.schedules, s.watches, s.routes, page)
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
			reply, err := s.statusReply(ctx, principalID)
			return reply, nil, err
		}
		return StatusReply, nil, nil
	case "usage":
		if s.model == nil || s.model.Usage == nil {
			return s.helpReply(), nil, nil
		}
		return strings.Join(s.model.statusUsageLines(ctx, principalID), "\n"), nil, nil
	case "privacy":
		reply, err := s.privacyReply(ctx, principalID)
		return reply, nil, err
	case "cancel":
		if s.conversation == nil {
			return NothingToCancelReply, nil, nil
		}
		cancelled, err := s.conversation.Cancel(ctx, laneOf(ctx, principalID))
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
		reset, err := s.conversation.NewSession(ctx, laneOf(ctx, principalID))
		if err != nil {
			return "", nil, err
		}
		if reset {
			return NewSessionReply, nil, nil
		}
		return NoSessionReply, nil, nil
	case "sessions":
		if s.conversation == nil {
			return s.helpReply(), nil, nil
		}
		return renderSessions(ctx, laneOf(ctx, principalID), s.conversation, s.tabs.enabled())
	case "jobs":
		if s.schedules == nil {
			return s.helpReply(), nil, nil
		}
		return renderJobs(ctx, principalID, s.schedules, s.watches, s.routes, 1)
	case "memory":
		if s.memory == nil {
			return s.helpReply(), nil, nil
		}
		return renderMemory(ctx, principalID, s.memory, 1)
	case "backlog":
		if s.backlog == nil {
			return s.helpReply(), nil, nil
		}
		return renderBacklog(ctx, principalID, s.backlog, 1)
	case "tasks":
		if s.tasks == nil {
			return s.helpReply(), nil, nil
		}
		return renderTasks(ctx, principalID, s.tasks)
	case "settings":
		if len(s.sections) == 0 {
			return s.helpReply(), nil, nil
		}
		text, keyboard, err := renderSettings(ctx, principalID, sectionsFor(s.sections, principalID), settingsRootPage, "")
		if err != nil {
			return "", nil, err
		}
		return text, keyboard, nil
	default:
		return s.helpReply(), nil, nil
	}
}

// helpReply lists the commands registered with Telegram and those answered only when typed, so it
// cannot drift from either.
func (s *Service) helpReply() string {
	reply := HelpHeading + commandList(s.botCommands()) + commandList(typedCommands)
	if s.backlog != nil {
		reply += "\n" + BacklogHelpLine
	}
	if s.conversation != nil {
		reply += "\n\n" + HelpConversationLine
	}
	return reply
}

func commandList(commands []BotCommand) string {
	var builder strings.Builder
	for _, command := range commands {
		fmt.Fprintf(&builder, "\n/%s: %s", command.Command, command.Description)
	}
	return builder.String()
}

// dispatchCommand runs one command and answers it in the tab of ctx.
func (s *Service) dispatchCommand(ctx context.Context, bot Bot, message *Message, principalID, command string) (processingResult, string, error) {
	if command == "backlog" && s.backlog != nil && commandArgument(message.Text) == backlogExportArgument {
		return s.sendBacklogExport(ctx, message, principalID)
	}
	if command == "model" && s.conversation != nil && s.model != nil {
		return s.processModelCommand(ctx, message, principalID, command)
	}
	if command == "close" && bot.HasTopicsEnabled && s.conversation != nil {
		return s.processCloseCommand(ctx, message, principalID)
	}
	reply, keyboard, err := s.handleCommand(ctx, principalID, command)
	if err != nil {
		return processingResult{}, "handle_command", err
	}
	return s.sendCommandReply(ctx, message, principalID, command, reply, keyboard)
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
