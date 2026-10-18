// Package chatgpt calls the ChatGPT backend endpoints a principal's own subscription reaches
// beyond the conversation Pi holds: transcription of voice and hosted image generation. Neither
// endpoint is documented, so every response is checked before it is trusted. The client keeps no
// credentials; each call carries the requesting principal's own.
package chatgpt

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
)

const (
	DefaultBaseURL = "https://chatgpt.com/backend-api"

	// originator matches what Pi sends for the same subscription, so these requests look like
	// the client the account already uses.
	originator = "pi"
	// userAgent names this application. Cloudflare challenges the Go default on some backend
	// paths, such as transcription, before the credential is even read.
	userAgent = "home-ai-assistant (+https://github.com/vladar107/home-ai-assistant)"

	transcribePath = "/transcribe"
	responsesPath  = "/codex/responses"

	maximumTranscriptBytes = 64 << 10
	maximumErrorBytes      = 4 << 10
	// maximumEventBytes bounds one streamed event, which carries a whole base64 image.
	maximumEventBytes = 32 << 20

	imageInstructions = "You are an image generator. Create exactly the image the user describes."
)

var (
	// ErrUnauthorized reports that the subscription refused the credential.
	ErrUnauthorized = errors.New("ChatGPT refused the credential")
	// ErrUsageLimit reports that the subscription's usage limit is reached.
	ErrUsageLimit = errors.New("ChatGPT usage limit reached")
	// ErrNoImage reports that the request finished without a picture, usually a refusal.
	ErrNoImage = errors.New("ChatGPT returned no image")
)

// Credentials resolves a principal's own subscription; it never falls back to anyone else's.
type Credentials interface {
	Credential(ctx context.Context, principalID string) (connection.Credential, error)
	// CurrentCredential never refreshes: it fails with connection.ErrCredentialStale instead.
	CurrentCredential(ctx context.Context, principalID string) (connection.Credential, error)
}

type Config struct {
	BaseURL    string
	HTTPClient *http.Client
}

type Client struct {
	baseURL     string
	httpClient  *http.Client
	credentials Credentials
	now         func() time.Time

	usageMu    sync.Mutex
	usageCache map[string]usageEntry
}

func New(cfg Config, credentials Credentials) (*Client, error) {
	if credentials == nil {
		return nil, fmt.Errorf("ChatGPT credentials are required")
	}
	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// Callers bound each request with their context; image generation alone can take a minute.
		httpClient = &http.Client{Timeout: 3 * time.Minute}
	}
	return &Client{baseURL: baseURL, httpClient: httpClient, credentials: credentials, now: time.Now, usageCache: make(map[string]usageEntry)}, nil
}

func normalizeBaseURL(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return DefaultBaseURL, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("ChatGPT base URL must be a plain HTTP or HTTPS URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(scheme == "http" && isLoopback(parsed.Hostname())) {
		return "", fmt.Errorf("ChatGPT base URL must use HTTPS, or HTTP for a loopback address")
	}
	parsed.Scheme = scheme
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Audio is one recording to transcribe.
type Audio struct {
	FileName string
	MimeType string
	Data     []byte
}

// Transcribe returns what the recording says. An empty result means it holds no speech.
func (c *Client) Transcribe(ctx context.Context, principalID string, audio Audio) (string, error) {
	if len(audio.Data) == 0 {
		return "", fmt.Errorf("audio to transcribe is empty")
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, cleanFileName(audio.FileName)))
	header.Set("Content-Type", cmp.Or(audio.MimeType, "application/octet-stream"))
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("create transcription request: %w", err)
	}
	if _, err := part.Write(audio.Data); err != nil {
		return "", fmt.Errorf("create transcription request: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("create transcription request: %w", err)
	}

	request, err := c.newRequest(ctx, principalID, transcribePath, writer.FormDataContentType(), &form)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maximumTranscriptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read transcription response: %w", err)
	}
	if len(body) > maximumTranscriptBytes {
		return "", fmt.Errorf("transcription response is too large")
	}
	var decoded struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Text == nil {
		return "", fmt.Errorf("transcription response has no text")
	}
	return strings.TrimSpace(*decoded.Text), nil
}

// Image is a picture passed to or returned from image generation.
type Image struct {
	MimeType string
	Data     []byte
}

// ImageRequest asks for one picture. References are pictures to edit or draw from.
type ImageRequest struct {
	Model      string
	Prompt     string
	References []Image
}

// GenerateImage draws one picture through the hosted image tool of the Responses endpoint.
// ErrNoImage carries the model's explanation, such as a refusal, when one was given.
func (c *Client) GenerateImage(ctx context.Context, principalID string, imageRequest ImageRequest) (Image, error) {
	if strings.TrimSpace(imageRequest.Model) == "" || strings.TrimSpace(imageRequest.Prompt) == "" {
		return Image{}, fmt.Errorf("image request needs a model and a prompt")
	}
	content := []map[string]string{{"type": "input_text", "text": imageRequest.Prompt}}
	for _, reference := range imageRequest.References {
		content = append(content, map[string]string{
			"type":      "input_image",
			"detail":    "auto",
			"image_url": "data:" + reference.MimeType + ";base64," + base64.StdEncoding.EncodeToString(reference.Data),
		})
	}
	action := "generate"
	if len(imageRequest.References) > 0 {
		action = "edit"
	}
	payload, err := json.Marshal(map[string]any{
		"model":        imageRequest.Model,
		"instructions": imageInstructions,
		"input":        []map[string]any{{"role": "user", "content": content}},
		"tools":        []map[string]string{{"type": "image_generation", "action": action, "output_format": "png"}},
		"tool_choice":  map[string]string{"type": "image_generation"},
		"store":        false,
		"stream":       true,
	})
	if err != nil {
		return Image{}, fmt.Errorf("encode image request: %w", err)
	}

	request, err := c.newRequest(ctx, principalID, responsesPath, "application/json", bytes.NewReader(payload))
	if err != nil {
		return Image{}, err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	response, err := c.do(request)
	if err != nil {
		return Image{}, err
	}
	defer response.Body.Close()
	return readImageStream(response.Body)
}

type streamItem struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type streamEvent struct {
	Type     string     `json:"type"`
	Item     streamItem `json:"item"`
	Response struct {
		Output []streamItem `json:"output"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

func readImageStream(body io.Reader) (Image, error) {
	var items []streamItem
	completed := false
	err := readEvents(body, func(data []byte) error {
		var event streamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("image stream event is malformed")
		}
		switch event.Type {
		case "error":
			return fmt.Errorf("image request failed: %s", cmp.Or(event.Error.Message, event.Message, "unknown error"))
		case "response.failed":
			return fmt.Errorf("image request failed: %s", cmp.Or(event.Response.Error.Message, "unknown error"))
		case "response.output_item.done":
			items = append(items, event.Item)
		case "response.completed", "response.done":
			completed = true
			if len(event.Response.Output) > 0 {
				items = event.Response.Output
			}
		}
		return nil
	})
	if err != nil {
		return Image{}, err
	}
	if !completed {
		return Image{}, fmt.Errorf("image stream ended before the response completed")
	}

	var explanation []string
	for _, item := range items {
		switch item.Type {
		case "image_generation_call":
			if item.Result == "" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(item.Result)
			if err != nil {
				return Image{}, fmt.Errorf("generated image is not valid base64")
			}
			mimeType := http.DetectContentType(data)
			if !strings.HasPrefix(mimeType, "image/") {
				return Image{}, fmt.Errorf("generated image is not a picture")
			}
			return Image{MimeType: mimeType, Data: data}, nil
		case "message":
			for _, part := range item.Content {
				explanation = append(explanation, strings.TrimSpace(part.Text+part.Refusal))
			}
		}
	}
	if text := strings.TrimSpace(strings.Join(explanation, "\n")); text != "" {
		return Image{}, fmt.Errorf("%w: %s", ErrNoImage, text)
	}
	return Image{}, ErrNoImage
}

// readEvents hands each server-sent event's data to handle, joining multi-line data as the
// protocol requires.
func readEvents(body io.Reader, handle func([]byte) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maximumEventBytes)
	var data []byte
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		event := data
		data = nil
		if string(event) == "[DONE]" {
			return nil
		}
		return handle(event)
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if value, found := bytes.CutPrefix(line, []byte("data:")); found {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read image stream: %w", err)
	}
	return flush()
}

func (c *Client) newRequest(ctx context.Context, principalID, path, contentType string, body io.Reader) (*http.Request, error) {
	credential, err := c.credentials.Credential(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if credential.AccessToken == "" || credential.AccountID == "" {
		return nil, connection.ErrNotConnected
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("create ChatGPT request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	request.Header.Set("ChatGPT-Account-Id", credential.AccountID)
	request.Header.Set("originator", originator)
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Content-Type", contentType)
	return request, nil
}

// do sends a request and turns a refusal into an error that never carries the credential.
func (c *Client) do(request *http.Request) (*http.Response, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctxErr := request.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("ChatGPT request failed")
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer response.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(response.Body, maximumErrorBytes))
	switch {
	case response.Header.Get("cf-mitigated") != "":
		return nil, fmt.Errorf("ChatGPT %s was stopped by Cloudflare's bot check with status %d", request.URL.Path, response.StatusCode)
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case response.StatusCode == http.StatusTooManyRequests:
		return nil, ErrUsageLimit
	}
	return nil, fmt.Errorf("ChatGPT %s returned status %d: %s", request.URL.Path, response.StatusCode, strings.Join(strings.Fields(string(detail)), " "))
}

func cleanFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, name)
	return cmp.Or(strings.TrimSpace(name), "audio")
}
