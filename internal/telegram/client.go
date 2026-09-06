package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultBaseURL    = "https://api.telegram.org"
	responseBodyLimit = 4 << 20
	httpTimeoutGrace  = 10 * time.Second
)

type ClientConfig struct {
	BaseURL     string
	Token       string
	PollTimeout time.Duration
	HTTPClient  *http.Client
	Telemetry   Telemetry
}

type Client struct {
	baseURL     *url.URL
	token       string
	pollSeconds int64
	httpClient  *http.Client
	telemetry   Telemetry
}

type Bot struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type Chat struct {
	ID   int64    `json:"id"`
	Type ChatType `json:"type"`
}

type ChatType string

const ChatTypePrivate ChatType = "private"

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type apiResponse[T any] struct {
	OK        bool `json:"ok"`
	Result    T    `json:"result"`
	ErrorCode int  `json:"error_code"`
}

type apiError struct {
	method     string
	httpStatus int
	errorCode  int
}

func (e *apiError) Error() string {
	if e.errorCode != 0 {
		return fmt.Sprintf("Telegram %s returned HTTP status %d (API error code %d)", e.method, e.httpStatus, e.errorCode)
	}
	return fmt.Sprintf("Telegram %s returned HTTP status %d", e.method, e.httpStatus)
}

func (e *apiError) Permanent() bool {
	if e.httpStatus == http.StatusTooManyRequests || e.httpStatus >= 500 ||
		e.errorCode == http.StatusTooManyRequests || e.errorCode >= 500 {
		return false
	}
	return e.errorCode >= 400 && e.errorCode < 500 ||
		e.httpStatus >= 400 && e.httpStatus < 500
}

func isPermanentAPIError(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Permanent()
}

func NewClient(cfg ClientConfig) (*Client, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil || (parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https") || parsedBaseURL.Host == "" {
		return nil, fmt.Errorf("Telegram base API URL must be an absolute HTTP or HTTPS URL")
	}
	if parsedBaseURL.User != nil || parsedBaseURL.RawQuery != "" || parsedBaseURL.Fragment != "" {
		return nil, fmt.Errorf("Telegram base API URL must not contain user information, a query, or a fragment")
	}
	if err := validateToken(cfg.Token); err != nil {
		return nil, err
	}
	if cfg.PollTimeout < time.Second || cfg.PollTimeout%time.Second != 0 {
		return nil, fmt.Errorf("Telegram poll timeout must be a positive whole number of seconds")
	}
	if cfg.PollTimeout > time.Duration(1<<63-1)-httpTimeoutGrace {
		return nil, fmt.Errorf("Telegram poll timeout is too large")
	}

	requiredHTTPTimeout := cfg.PollTimeout + httpTimeoutGrace
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       requiredHTTPTimeout,
			CheckRedirect: rejectRedirect,
		}
	} else {
		clientCopy := *httpClient
		if clientCopy.Timeout == 0 {
			clientCopy.Timeout = requiredHTTPTimeout
		}
		if clientCopy.Timeout <= cfg.PollTimeout {
			return nil, fmt.Errorf("Telegram HTTP client timeout must exceed the poll timeout")
		}
		clientCopy.CheckRedirect = rejectRedirect
		httpClient = &clientCopy
	}

	return &Client{
		baseURL:     parsedBaseURL,
		token:       cfg.Token,
		pollSeconds: int64(cfg.PollTimeout / time.Second),
		httpClient:  httpClient,
		telemetry:   telemetryOrNoop(cfg.Telemetry),
	}, nil
}

func (c *Client) GetMe(ctx context.Context) (Bot, error) {
	return execute[Bot](c, ctx, "getMe", nil)
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	params := url.Values{
		"allowed_updates": {`["message"]`},
		"offset":          {strconv.FormatInt(offset, 10)},
		"timeout":         {strconv.FormatInt(c.pollSeconds, 10)},
	}
	return execute[[]Update](c, ctx, "getUpdates", params)
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	return c.SendReply(ctx, chatID, 0, text)
}

// SendReply quotes the request message with the reply. Telegram still delivers the text
// when that message was deleted, and a zero message ID sends without a quote.
func (c *Client) SendReply(ctx context.Context, chatID, replyToMessageID int64, text string) error {
	params := url.Values{
		"chat_id": {strconv.FormatInt(chatID, 10)},
		"text":    {text},
	}
	if replyToMessageID > 0 {
		params.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyToMessageID))
	}
	_, err := execute[struct{}](c, ctx, "sendMessage", params)
	return err
}

func (c *Client) SendTyping(ctx context.Context, chatID int64) error {
	params := url.Values{
		"action":  {"typing"},
		"chat_id": {strconv.FormatInt(chatID, 10)},
	}
	_, err := execute[bool](c, ctx, "sendChatAction", params)
	return err
}

// CommandScope names a Bot API BotCommandScope type; the empty scope is the default scope.
type CommandScope string

const CommandScopeAllPrivateChats CommandScope = "all_private_chats"

func (c *Client) SetMyCommands(ctx context.Context, scope CommandScope, commands []BotCommand) error {
	encoded, err := json.Marshal(commands)
	if err != nil {
		return fmt.Errorf("Telegram setMyCommands command list could not be encoded")
	}
	params := url.Values{"commands": {string(encoded)}}
	if scope != "" {
		encodedScope, err := json.Marshal(struct {
			Type string `json:"type"`
		}{string(scope)})
		if err != nil {
			return fmt.Errorf("Telegram setMyCommands scope could not be encoded")
		}
		params.Set("scope", string(encodedScope))
	}
	_, err = execute[bool](c, ctx, "setMyCommands", params)
	return err
}

func execute[T any](c *Client, ctx context.Context, method string, params url.Values) (result T, resultErr error) {
	var zero T
	ctx, endTelemetry := c.telemetry.StartTelegramAPI(ctx, method)
	defer func() {
		endTelemetry(resultErr)
	}()

	if params == nil {
		params = make(url.Values)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.endpointURL(method),
		strings.NewReader(params.Encode()),
	)
	if err != nil {
		return zero, fmt.Errorf("Telegram %s request could not be created", method)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, fmt.Errorf("Telegram %s request canceled: %w", method, ctxErr)
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return zero, fmt.Errorf("Telegram %s request timed out", method)
		}
		cause := err
		if requestError, ok := err.(*url.Error); ok && requestError.Err != nil {
			cause = requestError.Err
		}
		return zero, fmt.Errorf("Telegram %s request failed: %w", method, cause)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil {
		return zero, fmt.Errorf("Telegram %s response could not be read (HTTP status %d)", method, response.StatusCode)
	}
	if len(body) > responseBodyLimit {
		return zero, fmt.Errorf("Telegram %s response exceeded the size limit (HTTP status %d)", method, response.StatusCode)
	}

	var envelope apiResponse[T]
	if err := json.Unmarshal(body, &envelope); err != nil {
		return zero, fmt.Errorf("Telegram %s returned invalid JSON (HTTP status %d)", method, response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return zero, &apiError{method: method, httpStatus: response.StatusCode, errorCode: envelope.ErrorCode}
	}
	if !envelope.OK {
		return zero, &apiError{method: method, httpStatus: response.StatusCode, errorCode: envelope.ErrorCode}
	}

	return envelope.Result, nil
}

func (c *Client) endpointURL(method string) string {
	endpoint := *c.baseURL
	basePath := strings.TrimRight(endpoint.Path, "/")
	escapedBasePath := strings.TrimRight(endpoint.EscapedPath(), "/")
	endpoint.Path = basePath + "/bot" + c.token + "/" + method
	endpoint.RawPath = escapedBasePath + "/bot" + url.PathEscape(c.token) + "/" + method
	return endpoint.String()
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func validateToken(token string) error {
	if token == "" || !utf8.ValidString(token) {
		return fmt.Errorf("Telegram token is invalid")
	}
	if strings.IndexFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("Telegram token is invalid")
	}
	return nil
}
