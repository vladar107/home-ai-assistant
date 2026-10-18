package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	DefaultAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL    = "https://oauth2.googleapis.com/token"
	DefaultRevokeURL   = "https://oauth2.googleapis.com/revoke"
	DefaultUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"

	MailScope     = "https://www.googleapis.com/auth/gmail.readonly"
	CalendarScope = "https://www.googleapis.com/auth/calendar.readonly"

	requestTimeout    = 10 * time.Second
	responseBodyLimit = 1 << 20
)

// scopes is what every sign-in asks for: the account's identity and both read-only products, which
// Google's consent screen lets the user untick one by one.
var scopes = []string{"openid", "email", MailScope, CalendarScope}

type ErrorClass string

const (
	ErrorUnauthorized ErrorClass = "unauthorized"
	ErrorForbidden    ErrorClass = "forbidden"
	ErrorNotFound     ErrorClass = "not_found"
	// ErrorInvalidGrant is the token endpoint refusing a code or a refresh token for good.
	ErrorInvalidGrant ErrorClass = "invalid_grant"
	ErrorRateLimited  ErrorClass = "rate_limited"
	ErrorInvalid      ErrorClass = "invalid"
	ErrorUnavailable  ErrorClass = "unavailable"
)

// Error is a normalized failure of a Google API, shared by the OAuth client and the product
// connectors. It carries the product, the class, and the HTTP status only, so Google's message
// text, which can echo a token or a query, never reaches a log or the model.
type Error struct {
	// Product names the API in the error text, such as Gmail; empty means Google itself.
	Product string
	Class   ErrorClass
	Status  int
}

func (e *Error) Error() string {
	product := e.Product
	if product == "" {
		product = "Google"
	}
	if e.Status != 0 {
		return fmt.Sprintf("%s request failed: %s (HTTP status %d)", product, e.Class, e.Status)
	}
	return fmt.Sprintf("%s request failed: %s", product, e.Class)
}

// ProductError finds a failure of the named product's API in err. A failure of Google's own
// endpoints, such as a token refresh, is left out, because its class says nothing about the
// product's request.
func ProductError(err error, product string) (*Error, bool) {
	var googleErr *Error
	if !errors.As(err, &googleErr) || googleErr.Product != product {
		return nil, false
	}
	return googleErr, true
}

// Classify maps an HTTP status from any Google API to its error class.
func Classify(status int) ErrorClass {
	switch {
	case status == 400:
		return ErrorInvalid
	case status == 401:
		return ErrorUnauthorized
	case status == 403:
		return ErrorForbidden
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
	UserInfoURL  string
	HTTPClient   *http.Client
}

// Client talks to Google's OAuth endpoints with the operator's OAuth client; it holds no user
// credential itself.
type Client struct {
	clientID     string
	clientSecret string
	redirectURL  string
	authURL      string
	tokenURL     string
	revokeURL    string
	userInfoURL  string
	httpClient   *http.Client
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, fmt.Errorf("Google OAuth client ID and secret are required")
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Scheme != "http" || !IsLoopback(redirect.Hostname()) || redirect.RawQuery != "" {
		return nil, fmt.Errorf("Google redirect URL must be a plain http loopback address")
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
		{&client.userInfoURL, cfg.UserInfoURL, DefaultUserInfoURL},
	} {
		value, err := Endpoint(endpoint.value, endpoint.fallback)
		if err != nil {
			return nil, err
		}
		*endpoint.target = value
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: requestTimeout + time.Second, Transport: httpretry.New(nil, httpretry.Options{Service: "google"})}
	}
	return client, nil
}

// Endpoint validates a configured Google endpoint, falling back to the default when it is empty:
// HTTPS, or HTTP for a loopback test server.
func Endpoint(value, fallback string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		value = fallback
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		!(parsed.Scheme == "https" || (parsed.Scheme == "http" && IsLoopback(parsed.Hostname()))) {
		return "", fmt.Errorf("Google endpoint must use HTTPS, or HTTP for a loopback address")
	}
	return value, nil
}

func IsLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// AuthorizationURL is the consent link for one sign-in: offline access so a refresh token is
// issued, forced consent so it is issued again on a reconnect, and a PKCE challenge.
func (c *Client) AuthorizationURL(state, challenge string) string {
	query := url.Values{
		"client_id":              {c.clientID},
		"redirect_uri":           {c.redirectURL},
		"response_type":          {"code"},
		"scope":                  {strings.Join(scopes, " ")},
		"access_type":            {"offline"},
		"prompt":                 {"consent select_account"},
		"include_granted_scopes": {"true"},
		"state":                  {state},
		"code_challenge":         {challenge},
		"code_challenge_method":  {"S256"},
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
	return c.token(httpretry.Idempotent(ctx), url.Values{
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
		return Tokens{}, &Error{Class: Classify(status), Status: status}
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
		return &Error{Class: Classify(status), Status: status}
	}
	return nil
}

// Identity is the account behind an access token: Google's stable subject id and its address.
type Identity struct {
	Subject string
	Email   string
}

func (c *Client) UserInfo(ctx context.Context, access string) (Identity, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, c.userInfoURL, nil)
	if err != nil {
		return Identity{}, &Error{Class: ErrorUnavailable}
	}
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Accept", "application/json")
	var body struct {
		Subject string `json:"sub"`
		Email   string `json:"email"`
	}
	status, err := c.send(request, &body)
	if err != nil {
		return Identity{}, err
	}
	if status != http.StatusOK {
		return Identity{}, &Error{Class: Classify(status), Status: status}
	}
	return Identity{Subject: body.Subject, Email: body.Email}, nil
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
