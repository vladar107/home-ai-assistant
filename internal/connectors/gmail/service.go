// Package gmail is the Gmail connector: a principal's turns can search the mailboxes of every
// Google account they switched Mail on for and read a message's text. Nothing here can send,
// draft, label, or delete mail, because the read-only grant cannot.
package gmail

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
)

type Config struct {
	APIBaseURL string
	HTTPClient *http.Client
}

// Grants is the Google module: it holds the credentials and decides which accounts are usable.
type Grants interface {
	Accounts(ctx context.Context, principalID string, product google.Product) ([]google.Account, error)
	AccessToken(ctx context.Context, principalID, accountID string, product google.Product, forceRefresh bool) (string, error)
}

type Service struct {
	client *Client
	grants Grants
	logger *slog.Logger
}

func NewService(cfg Config, grants Grants, logger *slog.Logger) (*Service, error) {
	if grants == nil {
		return nil, fmt.Errorf("Gmail grants are required")
	}
	if logger == nil {
		return nil, fmt.Errorf("Gmail logger is required")
	}
	client, err := NewClient(ClientConfig{APIBaseURL: cfg.APIBaseURL, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, err
	}
	return &Service{client: client, grants: grants, logger: logger}, nil
}
