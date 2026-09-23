package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAPIBaseURL = "https://gmail.googleapis.com"
	DefaultAuthURL    = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL   = "https://oauth2.googleapis.com/token"
	DefaultRevokeURL  = "https://oauth2.googleapis.com/revoke"
	Scope             = "https://www.googleapis.com/auth/gmail.readonly"

	requestTimeout    = 10 * time.Second
	responseBodyLimit = 4 << 20
)

type ErrorClass string

const (
	ErrorUnauthorized ErrorClass = "unauthorized"
	// ErrorInvalidGrant is the token endpoint refusing a code or a refresh token for good.
	ErrorInvalidGrant ErrorClass = "invalid_grant"
	ErrorNotFound     ErrorClass = "not_found"
	ErrorRateLimited  ErrorClass = "rate_limited"
	ErrorInvalid      ErrorClass = "invalid"
	ErrorUnavailable  ErrorClass = "unavailable"
)

// Error is a normalized Google failure. It carries the class and HTTP status only, so Google's
// message text, which can echo a token or a query, never reaches a log or the model.
type Error struct {
	Class  ErrorClass
	Status int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("Gmail request failed: %s (HTTP status %d)", e.Class, e.Status)
	}
	return fmt.Sprintf("Gmail request failed: %s", e.Class)
}

func classify(status int) ErrorClass {
	switch {
	case status == 400:
		return ErrorInvalid
	case status == 401:
		return ErrorUnauthorized
	case status == 404:
		return ErrorNotFound
	case status == 429:
		return ErrorRateLimited
	default:
		return ErrorUnavailable
	}
}

type ClientConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	AuthURL      string
	TokenURL     string
	RevokeURL    string
	APIBaseURL   string
	HTTPClient   *http.Client
}

// Client talks to Google's OAuth endpoints with the operator's OAuth client, and to the Gmail API
// with a principal's access token passed on every call; it holds no user credential itself.
type Client struct {
	clientID     string
	clientSecret string
	redirectURL  string
	authURL      string
	tokenURL     string
	revokeURL    string
	apiBaseURL   string
	httpClient   *http.Client
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, fmt.Errorf("Gmail OAuth client ID and secret are required")
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Scheme != "http" || !isLoopback(redirect.Hostname()) || redirect.RawQuery != "" {
		return nil, fmt.Errorf("Gmail redirect URL must be a plain http loopback address")
	}
	client := &Client{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		httpClient:   cfg.HTTPClient,
	}
	for _, endpoint := range []struct {
		target   *string
		value    string
		fallback string
	}{
		{&client.authURL, cfg.AuthURL, DefaultAuthURL},
		{&client.tokenURL, cfg.TokenURL, DefaultTokenURL},
		{&client.revokeURL, cfg.RevokeURL, DefaultRevokeURL},
		{&client.apiBaseURL, cfg.APIBaseURL, DefaultAPIBaseURL},
	} {
		value := strings.TrimRight(strings.TrimSpace(endpoint.value), "/")
		if value == "" {
			value = endpoint.fallback
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
			!(parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
			return nil, fmt.Errorf("Gmail endpoint must use HTTPS, or HTTP for a loopback address")
		}
		*endpoint.target = value
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: requestTimeout + time.Second}
	}
	return client, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// AuthorizationURL is the consent link for one sign-in: offline access so a refresh token is
// issued, forced consent so it is issued again on a reconnect, and a PKCE challenge.
func (c *Client) AuthorizationURL(state, challenge string) string {
	query := url.Values{
		"client_id":             {c.clientID},
		"redirect_uri":          {c.redirectURL},
		"response_type":         {"code"},
		"scope":                 {Scope},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return c.authURL + "?" + query.Encode()
}

// Tokens is what the token endpoint returned for a code or a refresh.
type Tokens struct {
	Access  string
	Refresh string
	Expiry  time.Time
	Scopes  []string
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (Tokens, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {c.redirectURL},
	})
}

func (c *Client) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
}

func (c *Client) token(ctx context.Context, form url.Values) (Tokens, error) {
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	status, err := c.post(ctx, c.tokenURL, form, &body)
	if err != nil {
		return Tokens{}, err
	}
	if status != http.StatusOK {
		if body.Error == "invalid_grant" {
			return Tokens{}, &Error{Class: ErrorInvalidGrant, Status: status}
		}
		return Tokens{}, &Error{Class: classify(status), Status: status}
	}
	if body.AccessToken == "" || body.ExpiresIn <= 0 {
		return Tokens{}, &Error{Class: ErrorUnavailable, Status: status}
	}
	return Tokens{
		Access:  body.AccessToken,
		Refresh: body.RefreshToken,
		Expiry:  time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
		Scopes:  strings.Fields(body.Scope),
	}, nil
}

// Revoke ends the grant at Google, which removes the app from the account's third-party access.
func (c *Client) Revoke(ctx context.Context, token string) error {
	var ignored json.RawMessage
	status, err := c.post(ctx, c.revokeURL, url.Values{"token": {token}}, &ignored)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &Error{Class: classify(status), Status: status}
	}
	return nil
}

func (c *Client) post(ctx context.Context, endpoint string, form url.Values, out any) (int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, &Error{Class: ErrorUnavailable}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	return c.send(request, out)
}

func (c *Client) send(request *http.Request, out any) (int, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, &Error{Class: ErrorUnavailable}
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil || len(contents) > responseBodyLimit {
		return response.StatusCode, &Error{Class: ErrorUnavailable, Status: response.StatusCode}
	}
	if len(contents) > 0 && json.Unmarshal(contents, out) != nil && response.StatusCode == http.StatusOK {
		return response.StatusCode, &Error{Class: ErrorUnavailable, Status: response.StatusCode}
	}
	return response.StatusCode, nil
}

func (c *Client) get(ctx context.Context, access, path string, query url.Values, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	target := c.apiBaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		return &Error{Class: ErrorUnavailable}
	}
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Accept", "application/json")
	var raw json.RawMessage
	status, err := c.send(request, &raw)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &Error{Class: classify(status), Status: status}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Class: ErrorUnavailable, Status: status}
	}
	return nil
}

func (c *Client) Profile(ctx context.Context, access string) (string, error) {
	var profile struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := c.get(ctx, access, "/gmail/v1/users/me/profile", nil, &profile); err != nil {
		return "", err
	}
	if strings.TrimSpace(profile.EmailAddress) == "" {
		return "", &Error{Class: ErrorUnavailable}
	}
	return profile.EmailAddress, nil
}

type messageList struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	NextPageToken      string `json:"nextPageToken"`
	ResultSizeEstimate int    `json:"resultSizeEstimate"`
}

func (c *Client) ListMessages(ctx context.Context, access, query string, limit int) (messageList, error) {
	var list messageList
	err := c.get(ctx, access, "/gmail/v1/users/me/messages", url.Values{
		"q":          {query},
		"maxResults": {strconv.Itoa(limit)},
	}, &list)
	return list, err
}

// Message is the part of a Gmail message resource the tools read.
type Message struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	InternalDate string   `json:"internalDate"`
	Payload      Part     `json:"payload"`
}

type Part struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Size         int64  `json:"size"`
		Data         string `json:"data"`
		AttachmentID string `json:"attachmentId"`
	} `json:"body"`
	Parts []Part `json:"parts"`
}

var metadataHeaders = []string{"From", "To", "Cc", "Subject", "Date"}

// MessageMetadata reads headers, labels, and the snippet without the body.
func (c *Client) MessageMetadata(ctx context.Context, access, id string) (Message, error) {
	var message Message
	err := c.get(ctx, access, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{
		"format":          {"metadata"},
		"metadataHeaders": metadataHeaders,
	}, &message)
	return message, err
}

func (c *Client) MessageFull(ctx context.Context, access, id string) (Message, error) {
	var message Message
	err := c.get(ctx, access, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{"format": {"full"}}, &message)
	return message, err
}
