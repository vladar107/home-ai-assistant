package conversation

import (
	"context"
	"errors"
	"log/slog"
	"time"

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
	BotUserID        int64
	TelegramUpdateID int64
	PrincipalID      string
	ChatID           int64
	Message          string
}

type Request struct {
	ID               string
	PrincipalID      string
	ChatID           int64
	SessionID        string
	RuntimeSessionID string
	Message          string
	State            string
	ResponseText     string
	ErrorKind        string
}

type EnqueueResult struct {
	Request Request
	Created bool
	Busy    bool
}

type Repository interface {
	EnqueueConversation(context.Context, EnqueueInput, Session, string) (EnqueueResult, error)
	RecoverConversations(context.Context) ([]string, error)
	NextConversationRequest(context.Context, string) (Request, bool, error)
	ClaimConversationRequest(context.Context, string) (bool, error)
	SaveConversationResponse(context.Context, string, string, string) (bool, error)
	ClaimConversationDelivery(context.Context, string) (bool, error)
	ReleaseConversationDelivery(context.Context, string) error
	CancelConversationDelivery(context.Context, string) error
	MarkConversationDelivered(context.Context, string) error
	CancelPendingConversation(context.Context, string) (bool, error)
	ResetConversation(context.Context, string) (bool, error)
}

type Deliverer interface {
	SendMessage(context.Context, int64, string) error
	SendTyping(context.Context, int64) error
}

type ToolProvider interface {
	ToolsFor(context.Context, string) ([]agentruntime.ToolDefinition, error)
}

type Telemetry interface {
	ContextLogger(context.Context, *slog.Logger) *slog.Logger
	StartConversationTurn(context.Context) (context.Context, func(string, string))
}

type Config struct {
	TurnTimeout time.Duration
}
