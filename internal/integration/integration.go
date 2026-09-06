// Package integration holds the core-owned, non-secret record of a principal's connection to an
// external service and the credential file conventions every connector shares. Tokens never pass
// through the repository; they live only in the credential files.
package integration

import (
	"context"
	"time"
)

const (
	TokenKindInternal = "internal"
	TokenKindPersonal = "personal"
)

// Connection is the durable, non-secret metadata of one principal's integration.
type Connection struct {
	PrincipalID   string
	Integration   string
	TokenKind     string
	WorkspaceID   string
	WorkspaceName string
	BotName       string
	WritesEnabled bool
	ConnectedAt   time.Time
}

type Repository interface {
	// SaveIntegrationConnection records a fresh connection, replacing the principal's previous
	// record for the integration and switching writes off again.
	SaveIntegrationConnection(context.Context, Connection) error
	LoadIntegrationConnection(ctx context.Context, principalID, integration string) (Connection, bool, error)
	DeleteIntegrationConnection(ctx context.Context, principalID, integration string) (bool, error)
}
