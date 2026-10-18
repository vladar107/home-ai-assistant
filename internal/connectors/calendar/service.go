// Package calendar is the Google Calendar connector: a principal's turns can list the events in a
// time range across the calendars of every Google account with Calendar switched on, and read one
// event. It holds no credential; the Google module decides which accounts are usable and hands
// out their access tokens. Nothing here can create, change, or answer an event, because the
// credential cannot.
package calendar

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

// Grants is the Google module: it holds the credentials and decides which accounts are usable.
type Grants interface {
	Accounts(ctx context.Context, principalID string, product google.Product) ([]google.Account, error)
	AccessToken(ctx context.Context, principalID, accountID string, product google.Product, forceRefresh bool) (string, error)
}

type Config struct {
	// Location is where dates are read and times are shown, the configured agent time zone.
	Location   *time.Location
	APIBaseURL string
	HTTPClient *http.Client
}

type Service struct {
	location *time.Location
	client   *Client
	grants   Grants
	logger   *slog.Logger
}

func NewService(cfg Config, grants Grants, logger *slog.Logger) (*Service, error) {
	if cfg.Location == nil {
		return nil, fmt.Errorf("calendar time zone is required")
	}
	if grants == nil {
		return nil, fmt.Errorf("calendar grants are required")
	}
	if logger == nil {
		return nil, fmt.Errorf("calendar logger is required")
	}
	client, err := NewClient(cfg.APIBaseURL, cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &Service{location: cfg.Location, client: client, grants: grants, logger: logger}, nil
}

// withAccess runs one account's call with its access token. A token Google turns away is
// refreshed once and the call retried, which covers a token revoked or rotated mid-hour.
func (s *Service) withAccess(ctx context.Context, principalID string, account google.Account, run func(access string) error) error {
	access, err := s.grants.AccessToken(ctx, principalID, account.ID, google.ProductCalendar, false)
	if err != nil {
		return err
	}
	err = run(access)
	if errorClass(err) == google.ErrorUnauthorized {
		if access, err = s.grants.AccessToken(ctx, principalID, account.ID, google.ProductCalendar, true); err != nil {
			return err
		}
		err = run(access)
	}
	return err
}
