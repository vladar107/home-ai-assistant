package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/eval"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
)

const account = "account-eval"

// login runs the device-code flow against the simulator, approving as soon as the code is shown.
func login(t *testing.T, profiles eval.Profiles, sim *codexauth.Simulator) string {
	t.Helper()
	sim.SetInterval(0.01)
	output := &syncBuffer{written: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- eval.Login(t.Context(), profiles, eval.Endpoints{AuthBaseURL: sim.URL(), PollInterval: time.Millisecond}, output)
	}()
	// Login prints the verification code once the device authorization has started. Opening the
	// eval store runs every migration first, which is slow under the race detector, so this waits
	// for that output rather than for a fixed time.
	select {
	case <-output.written:
	case err := <-done:
		t.Fatalf("login ended before a device authorization started: %v", err)
	case <-t.Context().Done():
		t.Fatal("login never started a device authorization")
	}
	sim.Approve(sim.LatestUserCode(), account)
	if err := <-done; err != nil {
		t.Fatalf("login: %v", err)
	}
	return output.String()
}

type syncBuffer struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	written chan struct{}
	once    sync.Once
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written != nil {
		b.once.Do(func() { close(b.written) })
	}
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestLoginCreatesAPrivateBoundProfile(t *testing.T) {
	sim := codexauth.New(t)
	profiles, err := eval.PrepareProfiles(filepath.Join(t.TempDir(), "profiles"), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	output := login(t, profiles, sim)
	if !strings.Contains(output, "CODE-0001") || !strings.Contains(output, sim.URL()+"/codex/device") {
		t.Fatalf("terminal output lacks the address and code: %q", output)
	}

	info, err := os.Lstat(profiles.Dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("profile directory = %v %v", info, err)
	}
	authPath := filepath.Join(profiles.Dir, "auth.json")
	if info, err := os.Stat(authPath); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("credential file = %v %v", info, err)
	}
	contents, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	var providers map[string]struct {
		Access    string `json:"access"`
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(contents, &providers); err != nil || providers["openai-codex"].AccountID != account || providers["openai-codex"].Access != codexauth.AccessToken(account) {
		t.Fatalf("credential Pi reads = %+v %v", providers, err)
	}

	store, err := profiles.OpenStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, bound, err := store.LoadModelConnection(t.Context(), eval.PrincipalID)
	if err != nil || !bound || record.AccountID != account {
		t.Fatalf("binding = %+v %v %v", record, bound, err)
	}
}

func TestProfilesRootInsideTheDataDirectoryIsRefused(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(data, "pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{
		"the data directory": data,
		"inside it":          filepath.Join(data, "pi"),
		"not yet created":    filepath.Join(data, "new", "root"),
		"through a symlink":  filepath.Join(link, "pi"),
	} {
		if _, err := eval.PrepareProfiles(root, data); err == nil {
			t.Fatalf("%s: root accepted", name)
		}
	}
	sibling := filepath.Join(filepath.Dir(data), "data-eval")
	if _, err := eval.PrepareProfiles(sibling, data); err != nil {
		t.Fatalf("sibling with a shared name prefix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "new")); err == nil {
		t.Fatal("a refused root was created")
	}
}

func usageStub(t *testing.T, used float64, seen *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"allowed":          true,
				"primary_window":   map[string]any{"used_percent": 10, "limit_window_seconds": 18000, "reset_at": time.Now().Add(time.Hour).Unix()},
				"secondary_window": map[string]any{"used_percent": used, "limit_window_seconds": 604800, "reset_at": time.Now().Add(48 * time.Hour).Unix()},
			},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func expireCredential(t *testing.T, profiles eval.Profiles) {
	t.Helper()
	record, err := json.Marshal(map[string]any{"openai-codex": map[string]any{
		"type": "oauth", "access": "expired-access", "refresh": codexauth.RefreshToken(account),
		"expires": time.Now().Add(-time.Hour).UnixMilli(), "accountId": account,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles.Dir, "auth.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageRefreshesAnExpiredCredentialThenReads(t *testing.T) {
	sim := codexauth.New(t)
	profiles, err := eval.PrepareProfiles(filepath.Join(t.TempDir(), "profiles"), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	login(t, profiles, sim)
	expireCredential(t, profiles)

	var seen []string
	stub := usageStub(t, 20, &seen)
	endpoints := eval.Endpoints{AuthBaseURL: sim.URL(), ChatGPTBaseURL: stub.URL}
	windows, err := eval.Preflight(t.Context(), profiles, endpoints, eval.DefaultMaxUsedPercent)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(windows) != 2 || windows[1].Name != "weekly" || windows[1].UsedPercent != 20 {
		t.Fatalf("windows = %+v", windows)
	}
	if len(seen) != 1 || seen[0] != "Bearer "+codexauth.AccessToken(account) {
		t.Fatalf("usage read carried %v, want the refreshed token", seen)
	}
	refreshed := false
	for _, request := range sim.Requests() {
		refreshed = refreshed || request.Body["grant_type"] == "refresh_token"
	}
	if !refreshed {
		t.Fatal("the expired credential was not refreshed")
	}
}

func TestAWindowAboveTheThresholdRefusesAndIsNamed(t *testing.T) {
	sim := codexauth.New(t)
	profiles, err := eval.PrepareProfiles(filepath.Join(t.TempDir(), "profiles"), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	login(t, profiles, sim)

	var seen []string
	stub := usageStub(t, 80, &seen)
	_, err = eval.Preflight(t.Context(), profiles, eval.Endpoints{AuthBaseURL: sim.URL(), ChatGPTBaseURL: stub.URL}, 50)
	if err == nil || !strings.Contains(err.Error(), "weekly") || strings.Contains(err.Error(), "five-hour") {
		t.Fatalf("preflight error = %v, want the weekly window named alone", err)
	}
}

func TestUsageWithoutALoginFails(t *testing.T) {
	profiles, err := eval.PrepareProfiles(filepath.Join(t.TempDir(), "profiles"), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eval.ReadUsage(context.Background(), profiles, eval.Endpoints{}); err == nil || !strings.Contains(err.Error(), connection.ErrNotConnected.Error()) {
		t.Fatalf("usage without login: %v", err)
	}
}

func TestScanFindsAPlantedValueWithoutPrintingIt(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	const planted = "Quillfeather Lane"
	env := "HOME_AGENT_PLACES=Harbourview, " + planted + "\nHOME_AGENT_PORT=8080\nHOME_AGENT_FLAG=true\nHOME_AGENT_TIMEOUT=30s\nSHORT=abc\n"
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := filepath.Join(dir, "evals", "cases")
	if err := os.MkdirAll(cases, 0o755); err != nil {
		t.Fatal(err)
	}
	clean := `{"id":"clean","turns":[{"text":"What is on today? 8080 true 30s abc"}]}`
	dirty := `{"id":"dirty","turns":[{"text":"hello"},{"text":"Meet me on QUILLFEATHER lane at six"}]}`
	for name, body := range map[string]string{"clean.json": clean, "dirty.json": dirty} {
		if err := os.WriteFile(filepath.Join(cases, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	findings, found, err := eval.Scan([]string{envPath}, []string{cases, filepath.Join(dir, "evals", "variants")})
	if err != nil || !found {
		t.Fatalf("scan = %v %v", found, err)
	}
	if len(findings) != 1 || findings[0].File != filepath.Join(cases, "dirty.json") || findings[0].Field != "$.turns[1].text" {
		t.Fatalf("findings = %+v", findings)
	}
	if strings.Contains(strings.ToLower(findings[0].File+findings[0].Field), "quillfeather") {
		t.Fatal("a finding carries the value")
	}
}

func TestScanWithoutAnEnvFileHasNothingToCheck(t *testing.T) {
	findings, found, err := eval.Scan([]string{filepath.Join(t.TempDir(), ".env")}, []string{t.TempDir()})
	if err != nil || found || len(findings) != 0 {
		t.Fatalf("scan = %+v %v %v", findings, found, err)
	}
}
