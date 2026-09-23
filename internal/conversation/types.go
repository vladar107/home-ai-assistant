package conversation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
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
	StartConversationTurn(context.Context) (context.Context, func(string, string))
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

type Config struct {
	TurnTimeout time.Duration
	FollowUp    FollowUp
	Notice      Notice
	Attachments Attachments
}
