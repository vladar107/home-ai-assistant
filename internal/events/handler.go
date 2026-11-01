package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaximumBodyBytes bounds a posted event's body.
const MaximumBodyBytes = 16 << 10

type HTTPTelemetry interface {
	StartHTTP(context.Context, string) (context.Context, func(string, int))
}

type eventRequest struct {
	Title     string  `json:"title"`
	Text      string  `json:"text"`
	Severity  string  `json:"severity"`
	DedupeKey *string `json:"dedupe_key"`
}

type acceptedResponse struct {
	ID        string `json:"id"`
	Duplicate bool   `json:"duplicate"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Register mounts POST /api/events on mux. The bearer token is the boundary: it names one source,
// whose owner alone receives the event, and is checked against that owner's allowlisting on every
// request. Requests are logged by source ID alone, never with the token or the body.
func Register(mux *http.ServeMux, service *Service, telemetry HTTPTelemetry) {
	mux.HandleFunc("POST /api/events", func(w http.ResponseWriter, r *http.Request) {
		ctx, end := telemetry.StartHTTP(r.Context(), "events_post")
		// The deadline comes first, so no client, with a token or without one, holds the
		// connection with a body it trickles in.
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(time.Now().Add(service.bodyTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			service.logger.ErrorContext(ctx, "events.api.failed", "stage", "read_deadline", "error", err)
			writeError(w, end, http.StatusInternalServerError, "internal")
			return
		}
		token, found := bearerToken(r.Header.Get("Authorization"))
		if !found {
			writeError(w, end, http.StatusUnauthorized, "unauthorized")
			return
		}
		source, authenticated, err := service.Authenticate(ctx, token)
		if err != nil {
			service.logger.ErrorContext(ctx, "events.api.failed", "stage", "authenticate", "error", err)
			writeError(w, end, http.StatusInternalServerError, "internal")
			return
		}
		if !authenticated {
			service.logger.WarnContext(ctx, "events.api.rejected", "reason", "unauthorized")
			writeError(w, end, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !source.Enabled {
			service.logger.InfoContext(ctx, "events.api.rejected", "source_id", source.ID, "reason", "paused")
			writeError(w, end, http.StatusForbidden, "paused")
			return
		}
		input, problem := readInput(w, r)
		if problem != "" {
			service.logger.InfoContext(ctx, "events.api.rejected", "source_id", source.ID, "reason", "invalid")
			writeError(w, end, http.StatusBadRequest, problem)
			return
		}
		// The whole body is read, so a kept-alive connection's next request reads without a deadline.
		_ = controller.SetReadDeadline(time.Time{})
		event, admission, err := service.Accept(ctx, source, input)
		if err != nil {
			service.logger.ErrorContext(ctx, "events.api.failed", "source_id", source.ID, "stage", "record", "error", err)
			writeError(w, end, http.StatusInternalServerError, "internal")
			return
		}
		if admission == Limited {
			service.logger.WarnContext(ctx, "events.api.rejected", "source_id", source.ID, "reason", "rate_limited")
			w.Header().Set("Retry-After", "3600")
			writeError(w, end, http.StatusTooManyRequests, "rate_limited")
			return
		}
		service.logger.InfoContext(ctx, "events.api.accepted", "source_id", source.ID, "event_id", event.ID, "duplicate", admission == Duplicated)
		writeJSON(w, http.StatusAccepted, acceptedResponse{ID: event.ID, Duplicate: admission == Duplicated})
		end("accepted", http.StatusAccepted)
	})
}

// bearerToken reads the token of an Authorization header, whose scheme is case-insensitive.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// readInput reads and validates the body within the size limit and the handler's read deadline.
func readInput(w http.ResponseWriter, r *http.Request) (Input, string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaximumBodyBytes))
	if err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			return Input{}, "body_too_large"
		}
		return Input{}, "invalid_body"
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request eventRequest
	if err := decoder.Decode(&request); err != nil || decoder.More() {
		return Input{}, "invalid_json"
	}
	input := Input{Title: strings.TrimSpace(request.Title), Text: strings.TrimSpace(request.Text), Severity: request.Severity}
	if input.Severity == "" {
		input.Severity = SeverityInfo
	}
	switch {
	case !utf8.ValidString(input.Title) || input.Title == "" || utf8.RuneCountInString(input.Title) > MaximumTitleRunes || hasControl(input.Title, false):
		return Input{}, "invalid_title"
	case !utf8.ValidString(input.Text) || utf8.RuneCountInString(input.Text) > MaximumTextRunes || hasControl(input.Text, true):
		return Input{}, "invalid_text"
	case input.Severity != SeverityInfo && input.Severity != SeverityWarning && input.Severity != SeverityCritical:
		return Input{}, "invalid_severity"
	}
	if request.DedupeKey != nil {
		input.DedupeKey = strings.TrimSpace(*request.DedupeKey)
		if input.DedupeKey == "" || !utf8.ValidString(input.DedupeKey) || utf8.RuneCountInString(input.DedupeKey) > MaximumDedupeRunes || hasControl(input.DedupeKey, false) {
			return Input{}, "invalid_dedupe_key"
		}
	}
	return input, ""
}

// hasControl reports a control character, such as NUL, in text; lines and tabs may break a
// multi-line text.
func hasControl(text string, multiline bool) bool {
	return strings.ContainsFunc(text, func(r rune) bool {
		if multiline && (r == '\n' || r == '\r' || r == '\t') {
			return false
		}
		return unicode.IsControl(r)
	})
}

// writeError replies with an error and closes the connection after it, so the server neither
// waits for a body the handler did not read nor keeps a client it refused.
func writeError(w http.ResponseWriter, end func(string, int), statusCode int, code string) {
	w.Header().Set("Connection", "close")
	writeJSON(w, statusCode, errorResponse{Error: code})
	end(code, statusCode)
}

func writeJSON(w http.ResponseWriter, statusCode int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}
