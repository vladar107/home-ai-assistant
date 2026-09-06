package connection

import (
	"context"
	"time"
)

const (
	ConnectedReply           = "Your model subscription is connected."
	ConnectDeniedReply       = "The connection was denied. Use /settings to try again."
	ConnectExpiredReply      = "The connection code expired. Use /settings to get a new one."
	ConnectFailedReply       = "The connection could not be completed. Use /settings to try again."
	AccountAlreadyBoundReply = "That ChatGPT account is already connected to another user of this bot."
)

// Ceremony describes a started device-code login the user must complete elsewhere.
type Ceremony struct {
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
}

// Status is the non-secret view of a principal's model connection.
type Status struct {
	Connected bool
	Provider  string
	AccountID string
	ExpiresAt time.Time
}

// Connection is the durable, non-secret record of a principal's bound model account.
type Connection struct {
	PrincipalID string
	Provider    string
	AccountID   string
	ConnectedAt time.Time
}

// Repository persists connection metadata; credentials never pass through it.
type Repository interface {
	// BindModelConnection records the account for the principal, replacing that principal's
	// previous record, and reports false when the account is already bound to another principal.
	BindModelConnection(ctx context.Context, principalID, provider, accountID string) (bool, error)
	LoadModelConnection(ctx context.Context, principalID string) (Connection, bool, error)
	DeleteModelConnection(ctx context.Context, principalID string) (bool, error)
}

// Deliverer sends the asynchronous outcome of a ceremony to the user's chat.
type Deliverer interface {
	SendMessage(context.Context, int64, string) error
}
