package backlog

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Telemetry interface {
	StartHTTP(context.Context, string) (context.Context, func(string, int))
}

type itemResponse struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	Title       string `json:"title"`
	Details     string `json:"details"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type listResponse struct {
	Items []itemResponse `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Register mounts the backlog API on mux. It serves every principal's items without
// authentication: network reach is the boundary.
func Register(mux *http.ServeMux, service *Service, telemetry Telemetry) {
	mux.HandleFunc("GET /api/backlog", func(w http.ResponseWriter, r *http.Request) {
		ctx, end := telemetry.StartHTTP(r.Context(), "backlog_list")
		items, err := service.AllItems(ctx)
		if err != nil {
			service.internalError(w, end, "backlog.api.list_failed", err)
			return
		}
		response := listResponse{Items: make([]itemResponse, 0, len(items))}
		for _, item := range items {
			response.Items = append(response.Items, newItemResponse(item))
		}
		writeJSON(w, http.StatusOK, response)
		end("ok", http.StatusOK)
	})
	mux.HandleFunc("GET /api/backlog/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, end := telemetry.StartHTTP(r.Context(), "backlog_get")
		item, found, err := service.Item(ctx, r.PathValue("id"))
		if err != nil {
			service.internalError(w, end, "backlog.api.get_failed", err)
			return
		}
		if !found {
			writeError(w, end, http.StatusNotFound, "not_found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, newItemResponse(item))
		end("ok", http.StatusOK)
	})
	mux.HandleFunc("POST /api/backlog/{id}/done", func(w http.ResponseWriter, r *http.Request) {
		ctx, end := telemetry.StartHTTP(r.Context(), "backlog_done")
		id := r.PathValue("id")
		item, found, err := service.Item(ctx, id)
		if err != nil {
			service.internalError(w, end, "backlog.api.done_failed", err)
			return
		}
		if !found {
			writeError(w, end, http.StatusNotFound, "not_found", "not_found")
			return
		}
		if item.Status != StatusOpen {
			writeError(w, end, http.StatusConflict, "already_done", "already_done")
			return
		}
		closed, err := service.MarkDone(ctx, item.PrincipalID, id)
		if err != nil {
			service.internalError(w, end, "backlog.api.done_failed", err)
			return
		}
		if !closed {
			writeError(w, end, http.StatusConflict, "already_done", "already_done")
			return
		}
		item, found, err = service.Item(ctx, id)
		if err != nil {
			service.internalError(w, end, "backlog.api.done_failed", err)
			return
		}
		if !found {
			writeError(w, end, http.StatusNotFound, "not_found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, newItemResponse(item))
		end("ok", http.StatusOK)
	})
}

func newItemResponse(item Item) itemResponse {
	return itemResponse{
		ID:          item.ID,
		PrincipalID: item.PrincipalID,
		Title:       item.Title,
		Details:     item.Details,
		Status:      item.Status,
		CreatedAt:   item.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   item.UpdatedAt.Format(time.RFC3339),
	}
}

func (s *Service) internalError(w http.ResponseWriter, end func(string, int), event string, err error) {
	s.logger.Error(event, "error", err)
	writeError(w, end, http.StatusInternalServerError, "internal", "error")
}

func writeError(w http.ResponseWriter, end func(string, int), statusCode int, code, result string) {
	writeJSON(w, statusCode, errorResponse{Error: code})
	end(result, statusCode)
}

func writeJSON(w http.ResponseWriter, statusCode int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}
