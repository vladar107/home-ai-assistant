package health

import (
	"context"
	"encoding/json"
	"net/http"
)

type ReadinessChecker interface {
	Ready(context.Context) error
}

type Telemetry interface {
	StartHTTP(context.Context, string) (context.Context, func(string, int))
}

func Handler(checker ReadinessChecker, telemetry Telemetry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, end := telemetry.StartHTTP(r.Context(), "healthz")
		writeJSON(w, http.StatusOK, "ok")
		end("ok", http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, end := telemetry.StartHTTP(r.Context(), "readyz")
		r = r.WithContext(ctx)
		if err := checker.Ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, "not_ready")
			end("not_ready", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, "ready")
		end("ready", http.StatusOK)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, statusCode int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
