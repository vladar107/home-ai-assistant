package health

import (
	"context"
	"encoding/json"
	"net/http"
)

type ReadinessChecker interface {
	Ready(context.Context) error
}

func Handler(checker ReadinessChecker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := checker.Ready(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		writeJSON(w, http.StatusOK, "ready")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, statusCode int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
