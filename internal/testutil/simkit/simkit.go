// Package simkit holds the small pieces every seeded loopback fake shares: strict seed decoding, a
// loopback HTTP server that works outside tests, and JSON answers. A fake built on it is started
// from a seed by a command or a test alike.
package simkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

// Decode reads a seed into into, rejecting unknown fields and trailing data. An empty seed leaves
// into untouched, so every fake works with no seed at all.
func Decode(seed json.RawMessage, into any) error {
	if len(bytes.TrimSpace(seed)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(seed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("decode seed: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("decode seed: unexpected data after the seed")
	}
	return nil
}

// Object checks that raw, a part of the seed passed through to the client as the service's own
// JSON, is an object.
func Object(name string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return fmt.Errorf("%s must be a JSON object", name)
	}
	return nil
}

// Server is a loopback HTTP server a fake runs on.
type Server struct {
	http *http.Server
	url  string
	once sync.Once
	done chan struct{}
}

// Serve starts handler on a random loopback port.
func Serve(handler http.Handler) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on loopback: %w", err)
	}
	s := &Server{
		http: &http.Server{Handler: handler},
		url:  "http://" + listener.Addr().String(),
		done: make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		_ = s.http.Serve(listener)
	}()
	return s, nil
}

// URL is the server's base address without a trailing slash.
func (s *Server) URL() string { return s.url }

// Close stops the server and drops its open connections.
func (s *Server) Close() {
	s.once.Do(func() {
		_ = s.http.Close()
		<-s.done
	})
}

// Cleanup closes the server when the test ends.
func (s *Server) Cleanup(t testing.TB) { t.Cleanup(s.Close) }

// WriteJSON answers with value as JSON under status.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// UnknownParameters returns the sorted names of the query parameters that are not in allowed.
func UnknownParameters(query url.Values, allowed ...string) []string {
	var unknown []string
	for name := range query {
		if !slices.Contains(allowed, name) {
			unknown = append(unknown, name)
		}
	}
	slices.Sort(unknown)
	return unknown
}

// AlwaysOn is an integration switch reader that has every switch on, for driving a connector
// without a store.
type AlwaysOn struct{}

func (AlwaysOn) Enabled(context.Context, string, integration.Switch) (bool, error) { return true, nil }
