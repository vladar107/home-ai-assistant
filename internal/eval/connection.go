package eval

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
)

// Endpoints overrides the provider endpoints; the zero value is production's.
type Endpoints struct {
	AuthBaseURL    string
	ChatGPTBaseURL string
	HTTPClient     *http.Client
	// PollInterval is the shortest wait between device token polls; zero means the provider's.
	PollInterval time.Duration
}

// newService builds the connection service over the eval profiles root, the way the application
// builds it over its own.
func newService(profiles Profiles, repository connection.Repository, deliverer connection.Deliverer, endpoints Endpoints) (*connection.Service, error) {
	return connection.NewService(connection.Config{
		ProfilesDir:         profiles.Root,
		Principals:          []string{PrincipalID},
		AuthBaseURL:         endpoints.AuthBaseURL,
		HTTPClient:          endpoints.HTTPClient,
		MinimumPollInterval: endpoints.PollInterval,
	}, repository, deliverer, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

type discardDeliverer struct{}

func (discardDeliverer) SendMessage(context.Context, int64, string) error { return nil }

func newChatGPT(endpoints Endpoints, service *connection.Service) (*chatgpt.Client, error) {
	return chatgpt.New(chatgpt.Config{BaseURL: endpoints.ChatGPTBaseURL, HTTPClient: endpoints.HTTPClient}, service)
}
