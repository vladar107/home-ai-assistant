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
)

var ErrBusy = errors.New("a conversation turn is already pending")

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
}

type EnqueueResult struct {
	Request Request
	Created bool
	Busy    bool
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
}

type Deliverer interface {
	// SendReply quotes the request message with the given ID; a zero ID sends without a quote.
	SendReply(context.Context, int64, int64, string) error
	SendTyping(context.Context, int64) error
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

// MessageRecorder remembers the raw text of each user message a chat turn runs, for guards that
// judge an action against what the user asked.
type MessageRecorder interface {
	RecordMessage(principalID, text string)
}

// ToolHinter names the offered tool a message most likely needs, or "" when none clearly fits.
type ToolHinter interface {
	Hint(ctx context.Context, principalID, message string, tools []agentruntime.ToolDefinition) (string, error)
}

// Notices joins several sources of notices into one, in order.
type Notices []Notice

func (n Notices) PendingNotice(principalID string) string {
	var pending []string
	for _, notice := range n {
		if text := notice.PendingNotice(principalID); text != "" {
			pending = append(pending, text)
		}
	}
	return strings.Join(pending, "\n\n")
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

type Config struct {
	TurnTimeout time.Duration
	FollowUp    FollowUp
	Notice      Notice
	Recent      MessageRecorder
	Attachments Attachments
	WebSearch   WebSearch
	ToolHint    ToolHinter
	// SafetyChecks labels the turn's metrics; nil labels every turn "unavailable".
	SafetyChecks SafetyChecks
	// Cards and Photos attach a staged card to a successful reply; nil leaves replies text only.
	Cards  Cards
	Photos PhotoDeliverer
	// Location is the household's time zone; each chat turn then tells the model the current
	// local time. Nil leaves the time out.
	Location *time.Location
	// Now is the clock that time is read from; nil means time.Now.
	Now func() time.Time
}
