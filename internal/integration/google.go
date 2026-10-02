package integration

import (
	"context"
	"time"
)

// GoogleAccount is the non-secret record of one Google account a principal connected: which
// products its grant covers and which the principal has switched on.
type GoogleAccount struct {
	PrincipalID     string
	AccountID       string
	Address         string
	MailGranted     bool
	CalendarGranted bool
	MailEnabled     bool
	CalendarEnabled bool
	ConnectedAt     time.Time
}

type GoogleAccounts interface {
	// SaveGoogleAccount records the account for its principal, replacing an earlier record of
	// the same account for that principal.
	SaveGoogleAccount(context.Context, GoogleAccount) error
	ListGoogleAccounts(ctx context.Context, principalID string) ([]GoogleAccount, error)
	DeleteGoogleAccount(ctx context.Context, principalID, accountID string) (bool, error)
	// GoogleAccountOwner returns the principal the account is connected for, since one Google
	// account may belong to one principal only.
	GoogleAccountOwner(ctx context.Context, accountID string) (string, bool, error)
}
