package judgment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	defaultEndpoint = "https://api.typesafe.ai/v1/systemone"
	defaultModel    = "jev-1.13.0"
	responseLimit   = 1 << 20
	defaultTimeout  = 5 * time.Second

	outcomeSuccess   = "success"
	outcomeError     = "error"
	outcomeTimeout   = "timeout"
	outcomeCancelled = "cancelled"
)

// Recorder receives one measurement per completed request.
type Recorder interface {
	RecordJudgment(ctx context.Context, purpose, outcome string, duration time.Duration)
	RecordJudgmentDecision(ctx context.Context, principal, purpose, result string)
}

// recordDecision counts what a completed judgment concluded for the principal.
func (c *Client) recordDecision(ctx context.Context, principalID, purpose, result string) {
	if c.telemetry == nil {
		return
	}
	c.telemetry.RecordJudgmentDecision(ctx, principalID, purpose, result)
}

// Question is one typed question in a request.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// Noul asks for a yes/no probability between 0 and 1.
func Noul(instructions, yes, no string) Question {
	return Question{
		Type:         "noul",
		Instructions: instructions,
		Criteria:     map[string]string{"true": yes, "false": no},
	}
}

// Choice asks for one option from a set, each with a description.
func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

type Config struct {
	APIKey     string
	Endpoint   string
	Model      string
	HTTPClient *http.Client
	Logger     *slog.Logger
	Telemetry  Recorder
}

type Client struct {
	apiKey     string
	endpoint   string
	model      string
	httpClient *http.Client
	logger     *slog.Logger
	telemetry  Recorder
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("judgment client API key is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("judgment client logger is required")
	}
	client := &Client{
		apiKey:     cfg.APIKey,
		endpoint:   cfg.Endpoint,
		model:      cfg.Model,
		httpClient: cfg.HTTPClient,
		logger:     cfg.Logger,
		telemetry:  cfg.Telemetry,
	}
	if client.endpoint == "" {
		client.endpoint = defaultEndpoint
	}
	if client.model == "" {
		client.model = defaultModel
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: defaultTimeout, Transport: httpretry.New(nil, httpretry.Options{Service: "typesafe"})}
	}
	return client, nil
}

type request struct {
	State     map[string]string   `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

type answer struct {
	Type       string  `json:"type"`
	Noul       float64 `json:"noul"`
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
}

// Answers holds the typed answers to one request.
type Answers struct {
	model   string
	answers map[string]answer
}

func (a Answers) Model() string { return a.model }

func (a Answers) Noul(id string) (float64, bool) {
	found, ok := a.answers[id]
	if !ok || found.Type != "noul" {
		return 0, false
	}
	return found.Noul, true
}

func (a Answers) Choice(id string) (option string, confidence float64, ok bool) {
	found, exists := a.answers[id]
	if !exists || found.Type != "choice" || found.Choice == "" {
		return "", 0, false
	}
	return found.Choice, found.Confidence, true
}

// Ask sends redacted state and questions to TypeSafe. The default HTTP client retries transient
// failures through the shared retry transport, within the caller's timeout; callers own that
// timeout and treat any error as "no judgment".
func (c *Client) Ask(ctx context.Context, purpose string, state map[string]Redacted, questions map[string]Question) (Answers, error) {
	started := time.Now()
	answers, err := c.ask(ctx, state, questions)
	outcome := outcomeFor(ctx, err)
	elapsed := time.Since(started)
	if c.telemetry != nil {
		c.telemetry.RecordJudgment(ctx, purpose, outcome, elapsed)
	}

	attrs := []any{
		"purpose", purpose,
		"model", answers.model,
		"outcome", outcome,
		"duration_ms", elapsed.Milliseconds(),
	}
	if err == nil {
		attrs = append(attrs, "answers", describeAnswers(answers))
	}
	c.logger.InfoContext(ctx, "judgment.request.completed", attrs...)
	return answers, err
}

func (c *Client) ask(ctx context.Context, state map[string]Redacted, questions map[string]Question) (Answers, error) {
	payload := request{State: make(map[string]string, len(state)), Model: c.model, Questions: questions}
	for key, value := range state {
		payload.State[key] = value.String()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Answers{}, fmt.Errorf("encode judgment request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(httpretry.Idempotent(ctx), http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Answers{}, fmt.Errorf("build judgment request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")

	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return Answers{}, fmt.Errorf("send judgment request: %w", unwrapURLError(err))
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, responseLimit))
		return Answers{}, fmt.Errorf("judgment request failed: status %d", httpResponse.StatusCode)
	}

	var decoded response
	if err := json.NewDecoder(io.LimitReader(httpResponse.Body, responseLimit)).Decode(&decoded); err != nil {
		return Answers{}, errors.New("decode judgment response: invalid JSON")
	}
	validated, err := validateAnswers(questions, decoded.Answers)
	if err != nil {
		return Answers{}, err
	}
	return Answers{model: decoded.Model, answers: validated}, nil
}

func validateAnswers(questions map[string]Question, answers map[string]answer) (map[string]answer, error) {
	validated := make(map[string]answer, len(questions))
	for id, question := range questions {
		found, ok := answers[id]
		if !ok {
			continue
		}
		if found.Type != question.Type {
			return nil, fmt.Errorf("judgment response invalid: answer type mismatch")
		}
		switch question.Type {
		case "noul":
			if !(found.Noul >= 0 && found.Noul <= 1) {
				return nil, fmt.Errorf("judgment response invalid: noul out of range")
			}
		case "choice":
			options, _ := question.Criteria.(map[string]string)
			if _, known := options[found.Choice]; !known {
				return nil, fmt.Errorf("judgment response invalid: unknown choice")
			}
			if !(found.Confidence >= 0 && found.Confidence <= 1) {
				return nil, fmt.Errorf("judgment response invalid: confidence out of range")
			}
		}
		validated[id] = found
	}
	return validated, nil
}

func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func outcomeFor(ctx context.Context, err error) string {
	if err == nil {
		return outcomeSuccess
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return outcomeTimeout
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return outcomeCancelled
	default:
		return outcomeError
	}
}

func describeAnswers(answers Answers) string {
	ids := make([]string, 0, len(answers.answers))
	for id := range answers.answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		found := answers.answers[id]
		switch found.Type {
		case "noul":
			parts = append(parts, fmt.Sprintf("%s=%.2f", id, found.Noul))
		case "choice":
			parts = append(parts, fmt.Sprintf("%s=%s(%.2f)", id, found.Choice, found.Confidence))
		}
	}
	return strings.Join(parts, " ")
}
