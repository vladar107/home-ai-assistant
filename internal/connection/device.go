package connection

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPollInterval = 5 * time.Second
	minimumPollInterval = time.Second
	slowDownIncrement   = 5 * time.Second
	responseBodyLimit   = 64 << 10
)

var errDenied = errors.New("device authorization was denied")

// deviceClient speaks the device-code flow the pinned Pi release uses for openai-codex.
type deviceClient struct {
	baseURL    string
	clientID   string
	httpClient *http.Client
}

type deviceAuthorization struct {
	deviceAuthID string
	userCode     string
	interval     time.Duration
}

type deviceCode struct {
	authorizationCode string
	codeVerifier      string
}

func (c *deviceClient) verificationURL() string {
	return c.baseURL + "/codex/device"
}

func (c *deviceClient) start(ctx context.Context) (deviceAuthorization, error) {
	status, body, err := c.postJSON(ctx, "/api/accounts/deviceauth/usercode", map[string]string{"client_id": c.clientID})
	if err != nil {
		return deviceAuthorization{}, err
	}
	if status == http.StatusNotFound {
		return deviceAuthorization{}, fmt.Errorf("device authorization is not enabled by the provider")
	}
	if status < 200 || status >= 300 {
		return deviceAuthorization{}, fmt.Errorf("device authorization request failed with status %d", status)
	}
	var response struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.DeviceAuthID == "" || response.UserCode == "" {
		return deviceAuthorization{}, fmt.Errorf("device authorization response is invalid")
	}
	interval, err := parseInterval(response.Interval)
	if err != nil {
		return deviceAuthorization{}, err
	}
	return deviceAuthorization{deviceAuthID: response.DeviceAuthID, userCode: response.UserCode, interval: interval}, nil
}

func parseInterval(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return defaultPollInterval, nil
	}
	var seconds float64
	if err := json.Unmarshal(raw, &seconds); err != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, fmt.Errorf("device authorization interval is invalid")
		}
		seconds, err = strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return 0, fmt.Errorf("device authorization interval is invalid")
		}
	}
	if seconds < 0 {
		return 0, fmt.Errorf("device authorization interval is invalid")
	}
	return max(time.Duration(seconds*float64(time.Second)), minimumPollInterval), nil
}

// poll waits for the user to approve the device; the context carries the ceremony deadline.
func (c *deviceClient) poll(ctx context.Context, device deviceAuthorization) (deviceCode, error) {
	interval := device.interval
	for {
		if err := ctx.Err(); err != nil {
			return deviceCode{}, err
		}
		status, body, err := c.postJSON(ctx, "/api/accounts/deviceauth/token", map[string]string{
			"device_auth_id": device.deviceAuthID,
			"user_code":      device.userCode,
		})
		if err != nil {
			return deviceCode{}, err
		}
		switch {
		case status >= 200 && status < 300:
			var response struct {
				AuthorizationCode string `json:"authorization_code"`
				CodeVerifier      string `json:"code_verifier"`
			}
			if err := json.Unmarshal(body, &response); err != nil || response.AuthorizationCode == "" || response.CodeVerifier == "" {
				return deviceCode{}, fmt.Errorf("device token response is invalid")
			}
			return deviceCode{authorizationCode: response.AuthorizationCode, codeVerifier: response.CodeVerifier}, nil
		case status == http.StatusForbidden || status == http.StatusNotFound:
		default:
			switch errorCode(body) {
			case "deviceauth_authorization_pending":
			case "slow_down":
				interval += slowDownIncrement
			default:
				return deviceCode{}, errDenied
			}
		}
		if !wait(ctx, interval) {
			return deviceCode{}, ctx.Err()
		}
	}
}

func errorCode(body []byte) string {
	var response struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Error) == 0 {
		return ""
	}
	var code string
	if err := json.Unmarshal(response.Error, &code); err == nil {
		return code
	}
	var object struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Error, &object); err == nil {
		return object.Code
	}
	return ""
}

func (c *deviceClient) exchange(ctx context.Context, code deviceCode) (oauthCredential, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.clientID},
		"code":          {code.authorizationCode},
		"code_verifier": {code.codeVerifier},
		"redirect_uri":  {c.baseURL + "/deviceauth/callback"},
	}
	return c.token(ctx, "exchange", form)
}

// refresh trades a refresh token for a new record, as Pi does when an access token expires.
func (c *deviceClient) refresh(ctx context.Context, refreshToken string) (oauthCredential, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.clientID},
	}
	return c.token(ctx, "refresh", form)
}

func (c *deviceClient) token(ctx context.Context, operation string, form url.Values) (oauthCredential, error) {
	status, body, err := c.post(ctx, "/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return oauthCredential{}, err
	}
	if status < 200 || status >= 300 {
		return oauthCredential{}, fmt.Errorf("token %s failed with status %d", operation, status)
	}
	var response struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.AccessToken == "" || response.RefreshToken == "" || response.ExpiresIn <= 0 {
		return oauthCredential{}, fmt.Errorf("token %s response is invalid", operation)
	}
	accountID, err := accountIDFromJWT(response.AccessToken)
	if err != nil {
		return oauthCredential{}, err
	}
	return oauthCredential{
		Type:      "oauth",
		Access:    response.AccessToken,
		Refresh:   response.RefreshToken,
		Expires:   time.Now().Add(time.Duration(response.ExpiresIn * float64(time.Second))).UnixMilli(),
		AccountID: accountID,
	}, nil
}

func (c *deviceClient) postJSON(ctx context.Context, path string, payload map[string]string) (int, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("encode device authorization request: %w", err)
	}
	return c.post(ctx, path, "application/json", bytes.NewReader(encoded))
}

func (c *deviceClient) post(ctx context.Context, path, contentType string, body io.Reader) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("create provider request: %w", err)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("provider request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit))
	if err != nil {
		return 0, nil, fmt.Errorf("provider response could not be read")
	}
	return response.StatusCode, data, nil
}

// accountIDFromJWT reads the ChatGPT account claim without verifying the signature;
// the token was just issued by the provider over TLS and only identifies the account.
func accountIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		payload, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			return "", fmt.Errorf("access token payload is invalid")
		}
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Auth.AccountID == "" {
		return "", fmt.Errorf("access token carries no account identity")
	}
	return claims.Auth.AccountID, nil
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
