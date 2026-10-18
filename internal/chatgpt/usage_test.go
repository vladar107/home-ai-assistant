package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
)

const (
	secretUserID    = "user-00000000-secret"
	secretAccountID = "acct-11111111-secret"
	secretEmail     = "person@example.invalid"
)

const usageResponse = `{
	"user_id": "` + secretUserID + `",
	"account_id": "` + secretAccountID + `",
	"email": "` + secretEmail + `",
	"plan_type": "plus",
	"rate_limit": {
		"allowed": true,
		"limit_reached": false,
		"primary_window": {"used_percent": 12.5, "limit_window_seconds": 18000, "reset_after_seconds": 100, "reset_at": 1790000000},
		"secondary_window": {"used_percent": 40, "limit_window_seconds": 604800, "reset_after_seconds": 200, "reset_at": 1790500000}
	},
	"model_usage": {"model-a": {"available": true, "available_at": null}, "model-b": {"available": false}},
	"credits": {"balance": "5"},
	"rate_limit_reached_type": null
}`

type fakeCredentials struct {
	mu          sync.Mutex
	credentials map[string]connection.Credential
	err         error
	block       bool
	stale       bool
	calls       int
	refreshes   int
}

func (f *fakeCredentials) Credential(ctx context.Context, principalID string) (connection.Credential, error) {
	f.mu.Lock()
	f.refreshes++
	f.mu.Unlock()
	return f.CurrentCredential(ctx, principalID)
}

func (f *fakeCredentials) CurrentCredential(ctx context.Context, principalID string) (connection.Credential, error) {
	f.mu.Lock()
	f.calls++
	block, err, stale := f.block, f.err, f.stale
	credential, ok := f.credentials[principalID]
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return connection.Credential{}, ctx.Err()
	}
	if stale {
		return connection.Credential{}, connection.ErrCredentialStale
	}
	if err != nil {
		return connection.Credential{}, err
	}
	if !ok {
		return connection.Credential{}, connection.ErrNotConnected
	}
	return credential, nil
}

func (f *fakeCredentials) setAccount(principalID, accountID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credentials[principalID] = connection.Credential{AccessToken: "token-" + principalID, AccountID: accountID}
}

type usageFixture struct {
	client      *Client
	credentials *fakeCredentials
	requests    atomic.Int32
	logs        *bytes.Buffer
	now         time.Time
	status      int
	body        string
	handler     func(http.ResponseWriter, *http.Request)
	lastRequest atomic.Pointer[http.Request]
}

func newUsageFixture(t *testing.T) *usageFixture {
	t.Helper()
	f := &usageFixture{
		credentials: &fakeCredentials{credentials: map[string]connection.Credential{}},
		logs:        &bytes.Buffer{},
		now:         time.Unix(1789990000, 0),
		status:      http.StatusOK,
		body:        usageResponse,
	}
	f.credentials.setAccount("alice", "acct-a")

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.lastRequest.Store(r)
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{BaseURL: server.URL}, f.credentials)
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return f.now }
	f.client = client
	return f
}

func (f *usageFixture) assertNoIdentifiers(t *testing.T, values ...any) {
	t.Helper()
	for _, value := range values {
		text := ""
		switch v := value.(type) {
		case error:
			if v != nil {
				text = v.Error()
			}
		case Usage:
			text = strings.Join([]string{v.PlanType}, "")
			for model := range v.ModelAvailable {
				text += model
			}
		}
		for _, secret := range []string{secretUserID, secretAccountID, secretEmail} {
			if strings.Contains(text, secret) {
				t.Fatalf("%q leaked into %T", secret, value)
			}
		}
	}
	for _, secret := range []string{secretUserID, secretAccountID, secretEmail} {
		if strings.Contains(f.logs.String(), secret) {
			t.Fatalf("%q leaked into logs", secret)
		}
	}
}

func TestUsageReadsOnlyShownFields(t *testing.T) {
	f := newUsageFixture(t)

	usage, err := f.client.Usage(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if usage.PlanType != "plus" || !usage.Allowed || usage.LimitReached {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Primary == nil || usage.Primary.UsedPercent != 12.5 || usage.Primary.WindowSeconds != 18000 || usage.Primary.ResetAt.Unix() != 1790000000 {
		t.Fatalf("primary = %+v", usage.Primary)
	}
	if usage.Secondary == nil || usage.Secondary.UsedPercent != 40 || usage.Secondary.WindowSeconds != 604800 {
		t.Fatalf("secondary = %+v", usage.Secondary)
	}
	if !usage.ModelAvailable["model-a"] || usage.ModelAvailable["model-b"] {
		t.Fatalf("models = %v", usage.ModelAvailable)
	}
	if !usage.FetchedAt.Equal(f.now) {
		t.Fatalf("fetched at %v", usage.FetchedAt)
	}

	request := f.lastRequest.Load()
	if request.Method != http.MethodGet || request.URL.Path != "/wham/usage" {
		t.Fatalf("request = %s %s", request.Method, request.URL.Path)
	}
	if request.Header.Get("Authorization") != "Bearer token-alice" || request.Header.Get("ChatGPT-Account-Id") != "acct-a" {
		t.Fatalf("request carried the wrong credential: %v", request.Header)
	}
	f.assertNoIdentifiers(t, usage)
}

func TestUsageFailuresReturnSentinelsWithoutBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, usageResponse, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, usageResponse, ErrUnauthorized},
		{"server error", http.StatusInternalServerError, usageResponse, ErrUsageUnavailable},
		{"rate limited", http.StatusTooManyRequests, usageResponse, ErrUsageUnavailable},
		{"malformed", http.StatusOK, `{"email": "` + secretEmail + `", "rate_limit": `, ErrUsageUnavailable},
		{"no rate limit", http.StatusOK, `{"email": "` + secretEmail + `", "plan_type": "plus"}`, ErrUsageUnavailable},
		{"oversized", http.StatusOK, `{"email": "` + secretEmail + `", "pad": "` + strings.Repeat("x", maximumUsageBytes) + `"}`, ErrUsageUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageFixture(t)
			f.status, f.body = tc.status, tc.body

			usage, err := f.client.Usage(context.Background(), "alice")
			if err != tc.want {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if usage.PlanType != "" || usage.Primary != nil {
				t.Fatalf("failure returned usage %+v", usage)
			}
			if _, ok := f.client.CachedUsage("alice", time.Hour); ok {
				t.Fatal("a failure was cached")
			}
			f.assertNoIdentifiers(t, err)
		})
	}
}

func TestUsageTimesOutOnSlowBackend(t *testing.T) {
	f := newUsageFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.handler = func(http.ResponseWriter, *http.Request) { <-release }

	started := time.Now()
	_, err := f.client.Usage(context.Background(), "alice")
	if err != ErrUsageUnavailable {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > usageTimeout+time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

func TestUsageTimeoutIncludesCredentialRefresh(t *testing.T) {
	f := newUsageFixture(t)
	f.credentials.block = true

	started := time.Now()
	_, err := f.client.Usage(context.Background(), "alice")
	if err != ErrUsageUnavailable {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > usageTimeout+time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if f.requests.Load() != 0 {
		t.Fatal("the backend was called without a credential")
	}
}

func TestUsageIsCachedPerPrincipalAndAccount(t *testing.T) {
	f := newUsageFixture(t)
	f.credentials.setAccount("bob", "acct-b")

	if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(usageTTL - time.Second)
	if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load(); got != 1 {
		t.Fatalf("requests after cache hit = %d", got)
	}

	if _, err := f.client.Usage(context.Background(), "bob"); err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load(); got != 2 {
		t.Fatalf("another principal shared the cache: %d requests", got)
	}
	if got := f.lastRequest.Load().Header.Get("Authorization"); got != "Bearer token-bob" {
		t.Fatalf("bob was read with %q", got)
	}

	f.now = f.now.Add(2 * time.Second)
	if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load(); got != 3 {
		t.Fatalf("expired entry was not refetched: %d requests", got)
	}

	f.credentials.setAccount("alice", "acct-other")
	if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load(); got != 4 {
		t.Fatalf("an account change was served from the cache: %d requests", got)
	}
}

func TestCachedUsageNeverFetches(t *testing.T) {
	f := newUsageFixture(t)

	if _, ok := f.client.CachedUsage("alice", time.Hour); ok {
		t.Fatal("empty cache reported a hit")
	}
	if f.requests.Load() != 0 || f.credentials.calls != 0 {
		t.Fatal("the cached accessor did work")
	}

	if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	callsBefore := f.credentials.calls
	f.now = f.now.Add(time.Minute * 30)
	usage, ok := f.client.CachedUsage("alice", time.Hour)
	if !ok || usage.PlanType != "plus" {
		t.Fatalf("cached usage = %+v, %v", usage, ok)
	}
	if f.requests.Load() != 1 || f.credentials.calls != callsBefore {
		t.Fatal("the cached accessor did work")
	}
	if _, ok := f.client.CachedUsage("bob", time.Hour); ok {
		t.Fatal("another principal's usage was served")
	}
}

func TestUsageNotConnected(t *testing.T) {
	f := newUsageFixture(t)

	if _, err := f.client.Usage(context.Background(), "nobody"); !errors.Is(err, connection.ErrNotConnected) {
		t.Fatalf("unknown principal error = %v", err)
	}
	f.credentials.setAccount("empty", "")
	if _, err := f.client.Usage(context.Background(), "empty"); !errors.Is(err, connection.ErrNotConnected) {
		t.Fatalf("empty account error = %v", err)
	}
	if f.requests.Load() != 0 {
		t.Fatal("the backend was called without a credential")
	}

	f.credentials.err = errors.New("refresh failed for " + secretEmail)
	_, err := f.client.Usage(context.Background(), "alice")
	if err != ErrUsageUnavailable {
		t.Fatalf("credential failure error = %v", err)
	}
	f.assertNoIdentifiers(t, err)
}

func TestCachedUsageHonoursMaxAge(t *testing.T) {
	f := newUsageFixture(t)
	if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10 * time.Minute)
	if _, ok := f.client.CachedUsage("alice", 5*time.Minute); ok {
		t.Fatal("an entry older than maxAge was served")
	}
	if _, ok := f.client.CachedUsage("alice", 15*time.Minute); !ok {
		t.Fatal("an entry within maxAge was refused")
	}
}

func TestUsageForgetsAccountsThatAreGone(t *testing.T) {
	cases := []struct {
		name   string
		change func(f *usageFixture)
		want   error
	}{
		{"not connected", func(f *usageFixture) { delete(f.credentials.credentials, "alice") }, connection.ErrNotConnected},
		{"unauthorized", func(f *usageFixture) { f.status = http.StatusUnauthorized; f.now = f.now.Add(usageTTL + time.Second) }, ErrUnauthorized},
		{"account mismatch", func(f *usageFixture) {
			f.credentials.setAccount("alice", "acct-other")
			f.status = http.StatusInternalServerError
		}, ErrUsageUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageFixture(t)
			if _, err := f.client.Usage(context.Background(), "alice"); err != nil {
				t.Fatal(err)
			}
			tc.change(f)
			if _, err := f.client.Usage(context.Background(), "alice"); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if _, ok := f.client.CachedUsage("alice", time.Hour); ok {
				t.Fatal("the entry survived")
			}
		})
	}
}

func TestUsageNeverRefreshesTheCredential(t *testing.T) {
	f := newUsageFixture(t)
	f.credentials.stale = true

	if _, err := f.client.Usage(context.Background(), "alice"); err != ErrUsageUnavailable {
		t.Fatalf("error = %v", err)
	}
	if f.credentials.refreshes != 0 || f.requests.Load() != 0 {
		t.Fatalf("refreshes = %d, requests = %d", f.credentials.refreshes, f.requests.Load())
	}
}

func TestUsageMissingResetAndIsolatedModelMap(t *testing.T) {
	f := newUsageFixture(t)
	f.body = `{"plan_type": "plus", "rate_limit": {"primary_window": {"used_percent": 1}}, "model_usage": {"m": {"available": true}}}`

	usage, err := f.client.Usage(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Primary.ResetAt.IsZero() {
		t.Fatalf("reset at = %v", usage.Primary.ResetAt)
	}
	usage.ModelAvailable["m"] = false
	if again, _ := f.client.Usage(context.Background(), "alice"); !again.ModelAvailable["m"] {
		t.Fatal("a caller changed the cached map")
	}
}
