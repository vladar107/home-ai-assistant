package judgment

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recorded struct {
	purpose, outcome string
}

type fakeRecorder struct{ calls []recorded }

func (f *fakeRecorder) RecordJudgment(_ context.Context, purpose, outcome string, _ time.Duration) {
	f.calls = append(f.calls, recorded{purpose, outcome})
}

func (f *fakeRecorder) RecordJudgmentDecision(context.Context, string, string, string) {}

func newTestClient(t *testing.T, server *httptest.Server, logs io.Writer, recorder Recorder) *Client {
	t.Helper()
	client, err := NewClient(Config{
		APIKey:    "test-key",
		Endpoint:  server.URL,
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		Telemetry: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAskSendsPinnedRedactedRequestAndParsesAnswers(t *testing.T) {
	var got struct {
		State     map[string]string          `json:"state"`
		Model     string                     `json:"model"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{
			"asked":{"type":"noul","noul":0.95},
			"tool":{"type":"choice","choice":"nas_search","confidence":0.9,"probabilities":{"nas_search":0.95,"none":0.05}}
		},"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer server.Close()

	var logs bytes.Buffer
	recorder := &fakeRecorder{}
	client := newTestClient(t, server, &logs, recorder)
	answers, err := client.Ask(context.Background(), "intent",
		map[string]Redacted{"message": Redact("mail me at jan@example.com")},
		map[string]Question{
			"asked": Noul("Did the user ask?", "asked", "not asked"),
			"tool":  Choice("Which tool?", map[string]string{"nas_search": "search files", "none": "no tool"}),
		})
	if err != nil {
		t.Fatal(err)
	}

	if authorization != "Bearer test-key" {
		t.Errorf("authorization = %q", authorization)
	}
	if got.Model != "jev-1.13.0" {
		t.Errorf("model = %q", got.Model)
	}
	if got.State["message"] != "mail me at [email]" {
		t.Errorf("state = %v", got.State)
	}
	var noul struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	}
	if err := json.Unmarshal(got.Questions["asked"], &noul); err != nil || noul.Type != "noul" || noul.Criteria["true"] != "asked" {
		t.Errorf("noul question = %s (%v)", got.Questions["asked"], err)
	}

	if value, ok := answers.Noul("asked"); !ok || value != 0.95 {
		t.Errorf("noul = %v, %v", value, ok)
	}
	if option, confidence, ok := answers.Choice("tool"); !ok || option != "nas_search" || confidence != 0.9 {
		t.Errorf("choice = %q, %v, %v", option, confidence, ok)
	}
	if _, ok := answers.Noul("tool"); ok {
		t.Error("choice answer must not read as noul")
	}
	if _, ok := answers.Noul("missing"); ok {
		t.Error("missing answer must not be found")
	}
	if answers.Model() != "jev-1.13.0" {
		t.Errorf("model = %q", answers.Model())
	}

	if len(recorder.calls) != 1 || recorder.calls[0] != (recorded{"intent", "success"}) {
		t.Errorf("recorded = %v", recorder.calls)
	}
	logged := logs.String()
	for _, want := range []string{"judgment.request.completed", "purpose=intent", "asked=0.95"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q: %s", want, logged)
		}
	}
	for _, leaked := range []string{"Did the user ask", "example.com", "[email]", "test-key"} {
		if strings.Contains(logged, leaked) {
			t.Errorf("log leaks %q: %s", leaked, logged)
		}
	}
}

func TestAskReturnsStatusOnlyOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "secret-body-detail")
	}))
	defer server.Close()

	var logs bytes.Buffer
	recorder := &fakeRecorder{}
	_, err := newTestClient(t, server, &logs, recorder).Ask(context.Background(), "screen", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret-body-detail") || strings.Contains(logs.String(), "secret-body-detail") {
		t.Error("response body leaked")
	}
	if len(recorder.calls) != 1 || recorder.calls[0].outcome != "error" {
		t.Errorf("recorded = %v", recorder.calls)
	}
}

func TestAskTimesOutAndCancels(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	recorder := &fakeRecorder{}
	client := newTestClient(t, server, io.Discard, recorder)

	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelTimeout()
	if _, err := client.Ask(timeoutCtx, "hint", nil, nil); err == nil {
		t.Fatal("expected timeout error")
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := client.Ask(cancelCtx, "hint", nil, nil); err == nil {
		t.Fatal("expected cancel error")
	}

	if len(recorder.calls) != 2 || recorder.calls[0].outcome != "timeout" || recorder.calls[1].outcome != "cancelled" {
		t.Errorf("recorded = %v", recorder.calls)
	}
}

func TestAskRejectsInvalidAnswers(t *testing.T) {
	cases := map[string]string{
		"noul out of range": `{"model":"m","answers":{"q":{"type":"noul","noul":1.5}}}`,
		"unknown choice":    `{"model":"m","answers":{"c":{"type":"choice","choice":"other","confidence":0.9}}}`,
		"type mismatch":     `{"model":"m","answers":{"q":{"type":"choice","choice":"a","confidence":0.9}}}`,
	}
	questions := map[string]Question{
		"q": Noul("?", "yes", "no"),
		"c": Choice("?", map[string]string{"a": "a", "none": "none"}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			if _, err := newTestClient(t, server, io.Discard, nil).Ask(context.Background(), "intent", nil, questions); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}
