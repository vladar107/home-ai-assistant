package conversation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

var ErrBusy = errors.New("a conversation turn is already pending")

// Error kinds a failed turn reports to telemetry. Startup creates each kind's series at zero from
// TurnErrorKinds, so a new kind belongs there too.
const (
	ErrorKindTimeout       = "timeout"
	ErrorKindRuntime       = "runtime"
	ErrorKindToolProvider  = "tool_provider"
	ErrorKindAttachments   = "attachments"
	ErrorKindNotConnected  = "not_connected"
	ErrorKindProviderQuota = string(agentruntime.FailureQuota)
	ErrorKindProviderAuth  = string(agentruntime.FailureAuth)
)

func TurnErrorKinds() []string {
	return []string{
		ErrorKindTimeout,
		ErrorKindRuntime,
		ErrorKindToolProvider,
		ErrorKindAttachments,
		ErrorKindNotConnected,
		ErrorKindProviderQuota,
		ErrorKindProviderAuth,
	}
}

const (
	RequestStateQueued = "queued"
	RequestStateReady  = "ready"
)

type Session struct {
	ID               string
	PrincipalID      string
	RuntimeSessionID string
}

type EnqueueInput struct {
	BotUserID         int64
	TelegramUpdateID  int64
	TelegramMessageID int64
	PrincipalID       string
	ChatID            int64
	Message           string
	Attachments       []attachment.Incoming
	// ReplyToMessageID is the message the user replied to; a reply to an answer from a closed
	// conversation returns to that conversation. Zero when the message is not a reply.
	ReplyToMessageID int64
}

type Request struct {
	ID                string
	PrincipalID       string
	ChatID            int64
	TelegramMessageID int64
	SessionID         string
	RuntimeSessionID  string
	Message           string
	State             string
	ResponseText      string
	ErrorKind         string
	CreatedAt         time.Time
	// ResumedFrom is when the conversation this request returned to was last active; zero when the
	// request did not switch conversations.
	ResumedFrom time.Time
}

type EnqueueResult struct {
	Request Request
	Created bool
	Busy    bool
	// SwitchedFrom is the session the request closed by returning to an earlier one, if any.
	SwitchedFrom string
}

// ListedSessions is how many conversations /sessions shows. More are read, because earlier
// conversations without anything the user wrote are left out.
const (
	ListedSessions    = 10
	sessionCandidates = 30
)

// SessionSummary is one conversation as the user sees it in the list; LastActive is local time.
type SessionSummary struct {
	ID         string
	Title      string
	Active     bool
	LastActive time.Time
}

type ResumeResult struct {
	Found bool
	Busy  bool
	// Current is true when the chosen conversation already was the active one.
	Current bool
	// Session is the conversation now active, as it was last active before the switch.
	Session SessionSummary
}

// ResumedLine opens the first answer after a reply returned to an earlier conversation.
func ResumedLine(lastActive time.Time) string {
	return "Back in the conversation from " + lastActive.Format("2 January 2006") + "."
}

// SessionRecord is one of a principal's conversations as the store knows it.
type SessionRecord struct {
	ID               string
	RuntimeSessionID string
	Active           bool
	LastActive       time.Time
	// Private is true once the conversation has read Notion page content or NAS file contents.
	Private bool
}

type SwitchResult struct {
	Found bool
	Busy  bool
	// Switched is false when the target already was the active session.
	Switched bool
	// Session is the conversation now active, with its last activity from before the switch.
	Session SessionRecord
	// Previous is the session the switch closed, or "" when none was active or the target already was.
	Previous string
}

type Repository interface {
	// EnqueueConversation records the request and its attachments in one transaction, filling in
	// each attachment's session from the session the request joined.
	EnqueueConversation(context.Context, EnqueueInput, Session, string, []attachment.Record) (EnqueueResult, error)
	RecoverConversations(context.Context) ([]string, error)
	NextConversationRequest(context.Context, string) (Request, bool, error)
	ClaimConversationRequest(context.Context, string) (bool, error)
	SaveConversationResponse(context.Context, string, string, string) (bool, error)
	ClaimConversationDelivery(context.Context, string) (bool, error)
	ReleaseConversationDelivery(context.Context, string) error
	CancelConversationDelivery(context.Context, string) error
	MarkConversationDelivered(context.Context, string) error
	CancelPendingConversation(context.Context, string) (bool, error)
	// ResetConversation closes the principal's active session and returns its ID.
	ResetConversation(context.Context, string) (string, bool, error)
	// ConversationSessions lists the principal's conversations, most recently active first.
	ConversationSessions(ctx context.Context, principalID string, limit int) ([]SessionRecord, error)
	// SwitchConversation makes the principal's own session the active one unless a turn is pending,
	// closing the previously active session as switched.
	SwitchConversation(ctx context.Context, principalID, sessionID string) (SwitchResult, error)
	// RecordConversationReply remembers which session a delivered Telegram message belongs to.
	RecordConversationReply(ctx context.Context, principalID, sessionID string, messageID int64) error
}

type Deliverer interface {
	// SendReply quotes the request message with the given ID; a zero ID sends without a quote.
	SendReply(context.Context, int64, int64, string) error
	// SendTrackedReply is SendReply returning the sent message's ID.
	SendTrackedReply(context.Context, int64, int64, string) (int64, error)
	SendTyping(context.Context, int64) error
	// SendMessageDraft shows an ephemeral draft; an empty text shows Telegram's thinking placeholder.
	SendMessageDraft(ctx context.Context, chatID, draftID int64, text string, canStop bool) error
}

// History reads what Pi recorded of a principal's conversations.
type History interface {
	// Title is the first thing the user wrote in the session, shortened, or "" when there is none.
	Title(ctx context.Context, principalID, runtimeSessionID string) (string, error)
}

// ConnectionGate reports whether a principal's own model connection is usable before a turn starts.
type ConnectionGate interface {
	Ready(context.Context, string) (bool, error)
}

type AlwaysConnected struct{}

func (AlwaysConnected) Ready(context.Context, string) (bool, error) { return true, nil }

// ToolProvider returns the tools a principal may use in one turn, with the executor bound to that
// principal; an empty set runs the turn without tools.
type ToolProvider interface {
	ToolsFor(context.Context, string) (agentruntime.ToolSet, error)
	// ReadOnlyToolsFor is the same set limited to read tools, for a turn nobody is watching.
	ReadOnlyToolsFor(context.Context, string) (agentruntime.ToolSet, error)
}

type Telemetry interface {
	ContextLogger(context.Context, *slog.Logger) *slog.Logger
	StartConversationTurn(ctx context.Context, principal, safetyChecks string) (context.Context, func(string, string))
	RecordReply(ctx context.Context, principal, kind, outcome, safetyChecks string, duration time.Duration)
	RecordReplyStage(ctx context.Context, principal, stage string, duration time.Duration)
	RecordModelTurn(ctx context.Context, principal, safetyChecks string, calls int, promptTokens, outputTokens int64)
	RecordToolCallsPerTurn(ctx context.Context, principal, safetyChecks string, calls int)
	RecordHintFollowThrough(ctx context.Context, principal, result string)
	// RecordConversationResume counts a return to an earlier conversation by what triggered it.
	RecordConversationResume(ctx context.Context, principal, trigger string)
	// RecordTurnIndicatorFailure counts a failed Telegram typing or draft call; method is "typing" or "draft".
	RecordTurnIndicatorFailure(ctx context.Context, principal, method string)
}

// SafetyChecks reports whether a principal has opted in to the safety checks, for labelling the
// turn's metrics; nil or an error labels the turn "unavailable".
type SafetyChecks interface {
	JudgmentsEnabled(ctx context.Context, principalID string) (bool, error)
}

// FollowUp runs after a turn's reply has been delivered, for work that must follow the reply
// such as a pending confirmation request; failures are logged, never retried with the reply.
type FollowUp interface {
	AfterDelivery(ctx context.Context, principalID string, chatID int64) error
}

// Notice reports what happened to the principal outside a turn, such as the resolution of a
// confirmation the model asked for, so the next turn starts from the outcome instead of a guess.
type Notice interface {
	PendingNotice(principalID string) string
}

// Exchange remembers the raw text of each user message a chat turn runs and of the plain replies
// delivered to them, for guards that judge an action against what the user asked or agreed to.
type Exchange interface {
	RecordMessage(principalID, text string)
	RecordReply(principalID, text string)
	RecentExchange(principalID string) []turnstate.Line
}

// ToolHinter names the offered tool a message most likely needs, or "" when none clearly fits.
// earlier is the conversation before message; answered reports whether a hint was asked and
// answered, whatever it named.
type ToolHinter interface {
	Hint(ctx context.Context, principalID, message string, earlier []turnstate.Line, tools []agentruntime.ToolDefinition) (name string, answered bool, err error)
}

// PrivateNotice is a notice source that knows whether what it returns may retell Notion page
// content or NAS file contents; the turn that receives such a notice makes its conversation
// private.
type PrivateNotice interface {
	Notice
	// TakeNotice is PendingNotice that also reports whether the text may retell private content.
	TakeNotice(principalID string) (text string, private bool)
}

// OutsideNotice is a source whose notices retell reports that may carry third-party content, such
// as a scheduled run's or a background task's; the turn that receives one counts as having read
// outside content.
type OutsideNotice struct{ Notice }

// Notices joins several sources of notices into one, in order.
type Notices []Notice

func (n Notices) PendingNotice(principalID string) string {
	text, _, _ := pendingNotice(n, principalID)
	return text
}

// pendingNotice returns the notice text, whether any part of it came from an OutsideNotice, and
// whether any part of it may retell private content.
func pendingNotice(source Notice, principalID string) (text string, outside, private bool) {
	switch source := source.(type) {
	case Notices:
		var pending []string
		for _, notice := range source {
			if text, fromOutside, fromPrivate := pendingNotice(notice, principalID); text != "" {
				pending = append(pending, text)
				outside = outside || fromOutside
				private = private || fromPrivate
			}
		}
		return strings.Join(pending, "\n\n"), outside, private
	case OutsideNotice:
		text, private := noticeFrom(source.Notice, principalID)
		return text, text != "", private
	default:
		text, private := noticeFrom(source, principalID)
		return text, false, private
	}
}

func noticeFrom(source Notice, principalID string) (string, bool) {
	if source, ok := source.(PrivateNotice); ok {
		text, private := source.TakeNotice(principalID)
		return text, private && text != ""
	}
	return source.PendingNotice(principalID), false
}

// Attachments keeps the photos and files a principal sent, for as long as the session lives; nil
// leaves a deployment without attachment support.
type Attachments interface {
	// Stage writes the bytes of one request's files and returns the rows describing them.
	Stage(principalID, requestID string, files []attachment.Incoming) ([]attachment.Record, error)
	// Discard removes bytes that were staged for a request the store did not record.
	Discard(records []attachment.Record)
	// Turn returns the images one request carried and the text describing all of its files.
	Turn(ctx context.Context, principalID, requestID string) (attachment.Turn, error)
	CloseSession(ctx context.Context, principalID, sessionID string) error
	Purge(ctx context.Context) error
}

// WebSearch reports whether a principal has web access on, so the runtime offers its own web
// search in their turns; nil never offers it.
type WebSearch interface {
	WebSearchEnabled(ctx context.Context, principalID string) (bool, error)
}

// Cards holds the image a turn's tool staged to go out with its reply, keyed by the turn.
type Cards interface {
	Card(principalID, turn string) (outbox.Card, bool, error)
	MarkSent(principalID, turn string, captioned bool) error
	Remove(principalID, turn string)
	// Discard removes everything the principal has staged.
	Discard(principalID string)
	// Sweep removes whatever was staged before the given time.
	Sweep(before time.Time) error
}

// PhotoDeliverer sends a card as a photo, quoting like Deliverer.SendReply, with an optional
// caption; CaptionFits reports whether a reply chunk can be that caption.
type PhotoDeliverer interface {
	SendPhoto(ctx context.Context, chatID, replyTo int64, png []byte, caption string) error
	CaptionFits(caption string) bool
}

// Locations describes a principal's shared position for a turn, or returns "" when there is none.
type Locations interface {
	TurnLine(ctx context.Context, principalID string) (string, error)
}

// Memory is the principal's remembered instructions and facts, which every turn starts from.
type Memory interface {
	// Render is the memory as prompt text, or "" when it is off or empty.
	Render(ctx context.Context, principalID string) (string, error)
	// TakeNotes returns, and forgets, the lines the turn's memory writes owe its reply.
	TakeNotes(principalID, turn string) []string
}

type Config struct {
	TurnTimeout time.Duration
	FollowUp    FollowUp
	Notice      Notice
	Recent      Exchange
	Attachments Attachments
	WebSearch   WebSearch
	ToolHint    ToolHinter
	// SafetyChecks labels the turn's metrics; nil labels every turn "unavailable".
	SafetyChecks SafetyChecks
	// Cards and Photos attach a staged card to a successful reply; nil leaves replies text only.
	Cards  Cards
	Photos PhotoDeliverer
	// Locations tells each turn where its user is while they share a location; nil leaves it out.
	Locations Locations
	// Memory goes into every turn's system prompt and adds a line to a reply for each write the
	// turn made; nil leaves memory out.
	Memory Memory
	// Exposure records a turn that searched the web or carried an outside notice as having read
	// text from outside the household; nil records nothing.
	Exposure Exposure
	// SearchChecks decides each query of a turn's web search; nil refuses every search.
	SearchChecks SearchChecks
	// Location is the household's time zone; each chat turn then tells the model the current
	// local time. Nil leaves the time out.
	Location *time.Location
	// Now is the clock that time is read from, including a turn's elapsed time on its draft; nil
	// means time.Now.
	Now func() time.Time
	// TypingInterval is how often typing is resent while a chat turn waits or runs; zero means 4s.
	TypingInterval time.Duration
	// DraftDelay is how long a chat turn runs before it shows a draft; zero means 8s.
	DraftDelay time.Duration
	// DraftInterval is how often a shown draft is refreshed; zero means 20s.
	DraftInterval time.Duration
	// History titles the conversations /sessions lists; nil lists them without titles.
	History History
	// DisplayNames tells each turn which household member its user is; a principal without a
	// name goes unnamed.
	DisplayNames map[string]string
}

// SearchChecks decides one web search query of a run. A run with nobody to confirm a search, such
// as a scheduled job or a background task, is not interactive.
type SearchChecks interface {
	Check(ctx context.Context, principalID, query string, interactive bool) agentruntime.SearchDecision
}

// Exposure records which turns read third-party content or private content and answers whether a
// call's conversation or run has read text from outside the household, and whether a chat
// request's conversation has read private content.
type Exposure interface {
	tools.TurnExposureRecorder
	tools.TurnPrivacyRecorder
	External(ctx context.Context, principalID string) bool
	Private(ctx context.Context, principalID, requestID string) bool
}
