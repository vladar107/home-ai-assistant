package app

import (
	"net/http"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/health"
)

// httpHandler builds the server's routes; without a backlog service only health is served.
func httpHandler(checker health.ReadinessChecker, telemetry health.Telemetry, backlogService *backlog.Service) http.Handler {
	mux := http.NewServeMux()
	healthHandler := health.Handler(checker, telemetry)
	mux.Handle("/healthz", healthHandler)
	mux.Handle("/readyz", healthHandler)
	if backlogService != nil {
		backlog.Register(mux, backlogService, telemetry)
	}
	return mux
}
