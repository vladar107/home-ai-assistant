// Package chatgptsim simulates the ChatGPT backend endpoints for transcription and hosted image
// generation, recording what each request carried.
package chatgptsim

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// PNG is the smallest picture the simulator returns: an 8-byte signature is enough for content
// sniffing.
var PNG = []byte("\x89PNG\r\n\x1a\n pretend image bytes")

// Request is one recorded call.
type Request struct {
	Path          string
	Authorization string
	AccountID     string
	UserAgent     string
	// FileName, MimeType, and Audio are set for a transcription.
	FileName string
	MimeType string
	Audio    []byte
	// Body is the decoded JSON of an image request.
	Body map[string]any
}

type Simulator struct {
	server *httptest.Server

	mu         sync.Mutex
	requests   []Request
	transcript string
	status     int
	refusal    string
}

func New(t *testing.T) *Simulator {
	t.Helper()
	s := &Simulator{transcript: "hello from a voice note"}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *Simulator) URL() string          { return s.server.URL }
func (s *Simulator) Client() *http.Client { return s.server.Client() }

// SetTranscript sets what the next transcriptions return.
func (s *Simulator) SetTranscript(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transcript = text
}

// FailWith makes every later request fail with the status.
func (s *Simulator) FailWith(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// Refuse makes image requests finish with this message and no picture.
func (s *Simulator) Refuse(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusal = message
}

func (s *Simulator) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Simulator) serve(w http.ResponseWriter, r *http.Request) {
	request := Request{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), AccountID: r.Header.Get("ChatGPT-Account-Id"), UserAgent: r.Header.Get("User-Agent")}
	switch r.URL.Path {
	case "/transcribe":
		file, header, err := r.FormFile("file")
		if err == nil {
			request.FileName = header.Filename
			request.MimeType = header.Header.Get("Content-Type")
			request.Audio, _ = io.ReadAll(file)
		}
	case "/codex/responses":
		_ = json.NewDecoder(r.Body).Decode(&request.Body)
	}

	s.mu.Lock()
	s.requests = append(s.requests, request)
	status, transcript, refusal := s.status, s.transcript, s.refusal
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"detail":"simulated failure"}`)
		return
	}
	switch r.URL.Path {
	case "/transcribe":
		if request.Audio == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": transcript})
	case "/codex/responses":
		w.Header().Set("Content-Type", "text/event-stream")
		var item map[string]any
		if refusal != "" {
			item = map[string]any{"type": "message", "content": []map[string]string{{"type": "output_text", "text": refusal}}}
		} else {
			item = map[string]any{"type": "image_generation_call", "status": "completed", "result": base64.StdEncoding.EncodeToString(PNG)}
		}
		writeEvent(w, map[string]any{"type": "response.created"})
		writeEvent(w, map[string]any{"type": "response.output_item.done", "item": item})
		writeEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeEvent(w io.Writer, event map[string]any) {
	data, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
}
