package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
)

const (
	usagePath = "/wham/usage"

	// usageTimeout bounds a whole usage read, because Telegram updates are handled one at a time
	// for the whole household. The read never refreshes the token: a refresh cut short after the
	// token endpoint rotated the refresh token would lose it.
	usageTimeout = 2 * time.Second
	usageTTL     = time.Minute

	maximumUsageBytes = 64 << 10
)

// ErrUsageUnavailable reports that the subscription's usage could not be read. It carries no
// provider text: the response also holds account identifiers that must never surface.
var ErrUsageUnavailable = errors.New("ChatGPT usage unavailable")

// UsageReader is what callers depend on to show a principal's subscription usage.
type UsageReader interface {
	// Usage returns the principal's usage, from the cache while it is fresh.
	Usage(ctx context.Context, principalID string) (Usage, error)
	// CachedUsage returns the last usage read for the principal without any network or
	// credential work, if it is no older than maxAge.
	CachedUsage(principalID string, maxAge time.Duration) (Usage, bool)
}

// Usage is the part of the subscription's usage read that the application shows.
type Usage struct {
	PlanType     string
	Allowed      bool
	LimitReached bool
	// Primary is the five-hour window and Secondary the weekly one; either is nil when absent.
	Primary   *UsageWindow
	Secondary *UsageWindow
	// ModelAvailable maps a model id to whether the subscription can use it now.
	ModelAvailable map[string]bool
	FetchedAt      time.Time
}

type UsageWindow struct {
	UsedPercent   float64
	WindowSeconds int
	ResetAt       time.Time
}

type usageWindowBody struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// usageBody lists only the fields that are used. The response also holds account identifiers
// and the email, which must stay undecoded.
type usageBody struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         bool             `json:"allowed"`
		LimitReached    bool             `json:"limit_reached"`
		PrimaryWindow   *usageWindowBody `json:"primary_window"`
		SecondaryWindow *usageWindowBody `json:"secondary_window"`
	} `json:"rate_limit"`
	ModelUsage map[string]struct {
		Available bool `json:"available"`
	} `json:"model_usage"`
}

type usageEntry struct {
	accountID string
	usage     Usage
}

// Usage reads the principal's own subscription usage. Errors are connection.ErrNotConnected,
// ErrUnauthorized or ErrUsageUnavailable. Results are cached for about a minute per principal
// and account; failures are not cached.
func (c *Client) Usage(ctx context.Context, principalID string) (Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()

	credential, err := c.credentials.CurrentCredential(ctx, principalID)
	if err != nil {
		if errors.Is(err, connection.ErrNotConnected) {
			c.forget(principalID)
			return Usage{}, connection.ErrNotConnected
		}
		return Usage{}, ErrUsageUnavailable
	}
	if credential.AccessToken == "" || credential.AccountID == "" {
		c.forget(principalID)
		return Usage{}, connection.ErrNotConnected
	}

	c.usageMu.Lock()
	entry, cached := c.usageCache[principalID]
	c.usageMu.Unlock()
	if cached && entry.accountID != credential.AccountID {
		c.forget(principalID)
	} else if cached && c.now().Sub(entry.usage.FetchedAt) < usageTTL {
		return entry.usage.clone(), nil
	}

	usage, err := c.fetchUsage(ctx, credential)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			c.forget(principalID)
		}
		return Usage{}, err
	}
	c.usageMu.Lock()
	c.usageCache[principalID] = usageEntry{accountID: credential.AccountID, usage: usage}
	c.usageMu.Unlock()
	return usage.clone(), nil
}

func (c *Client) CachedUsage(principalID string, maxAge time.Duration) (Usage, bool) {
	c.usageMu.Lock()
	entry, ok := c.usageCache[principalID]
	c.usageMu.Unlock()
	if !ok || c.now().Sub(entry.usage.FetchedAt) > maxAge {
		return Usage{}, false
	}
	return entry.usage.clone(), true
}

func (c *Client) forget(principalID string) {
	c.usageMu.Lock()
	delete(c.usageCache, principalID)
	c.usageMu.Unlock()
}

func (u Usage) clone() Usage {
	u.ModelAvailable = maps.Clone(u.ModelAvailable)
	return u
}

func (c *Client) fetchUsage(ctx context.Context, credential connection.Credential) (Usage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+usagePath, nil)
	if err != nil {
		return Usage{}, ErrUsageUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	request.Header.Set("ChatGPT-Account-Id", credential.AccountID)
	request.Header.Set("originator", originator)
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return Usage{}, ErrUsageUnavailable
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return Usage{}, ErrUnauthorized
	case response.StatusCode != http.StatusOK:
		return Usage{}, ErrUsageUnavailable
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maximumUsageBytes+1))
	if err != nil || len(body) > maximumUsageBytes {
		return Usage{}, ErrUsageUnavailable
	}
	var decoded usageBody
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.RateLimit == nil {
		return Usage{}, ErrUsageUnavailable
	}
	usage := Usage{
		PlanType:       decoded.PlanType,
		Allowed:        decoded.RateLimit.Allowed,
		LimitReached:   decoded.RateLimit.LimitReached,
		Primary:        decoded.RateLimit.PrimaryWindow.window(),
		Secondary:      decoded.RateLimit.SecondaryWindow.window(),
		ModelAvailable: make(map[string]bool, len(decoded.ModelUsage)),
		FetchedAt:      c.now(),
	}
	for model, state := range decoded.ModelUsage {
		usage.ModelAvailable[model] = state.Available
	}
	return usage, nil
}

func (w *usageWindowBody) window() *UsageWindow {
	if w == nil {
		return nil
	}
	window := &UsageWindow{UsedPercent: w.UsedPercent, WindowSeconds: w.LimitWindowSeconds}
	if w.ResetAt != 0 {
		window.ResetAt = time.Unix(w.ResetAt, 0)
	}
	return window
}
