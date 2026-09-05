// Package codexauth simulates the provider endpoints of the openai-codex device-code login.
package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateDenied   = "denied"
)

// Request is one recorded call: the path and the decoded JSON or form body.
type Request struct {
	Path string
	Body map[string]string
}

type device struct {
	id           string
	userCode     string
	state        string
	accountID    string
	slowDownOnce bool
	pendingAsErr bool
	code         string
	verifier     string
}

type Simulator struct {
	server   *httptest.Server
	mu       sync.Mutex
	interval int
	devices  []*device
	requests []Request
}

func New(t *testing.T) *Simulator {
	t.Helper()
	s := &Simulator{interval: 1}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *Simulator) URL() string          { return s.server.URL }
func (s *Simulator) Client() *http.Client { return s.server.Client() }

// LatestUserCode returns the code of the most recently started device authorization.
func (s *Simulator) LatestUserCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.devices) == 0 {
		return ""
	}
	return s.devices[len(s.devices)-1].userCode
}

func (s *Simulator) Approve(userCode, accountID string) {
	s.update(userCode, func(d *device) { d.state, d.accountID = StateApproved, accountID })
}

func (s *Simulator) Deny(userCode string) {
	s.update(userCode, func(d *device) { d.state = StateDenied })
}

// SlowDownOnce makes the next poll answer slow_down before the device's real state applies.
func (s *Simulator) SlowDownOnce(userCode string) {
	s.update(userCode, func(d *device) { d.slowDownOnce = true })
}

// PendingAsError answers pending polls with the JSON error code instead of a bare 403.
func (s *Simulator) PendingAsError(userCode string) {
	s.update(userCode, func(d *device) { d.pendingAsErr = true })
}

func (s *Simulator) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func AccessToken(accountID string) string {
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID}})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature-" + accountID
}

func RefreshToken(accountID string) string { return "refresh-token-" + accountID }

func (s *Simulator) update(userCode string, apply func(*device)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devices {
		if d.userCode == userCode {
			apply(d)
			return
		}
	}
	panic(fmt.Sprintf("codexauth: unknown user code %q", userCode))
}

func (s *Simulator) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	fields := decodeBody(r.Header.Get("Content-Type"), body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Path: r.URL.Path, Body: fields})
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/accounts/deviceauth/usercode":
		n := len(s.devices) + 1
		d := &device{id: fmt.Sprintf("device-%d", n), userCode: fmt.Sprintf("CODE-%04d", n), state: StatePending, code: fmt.Sprintf("auth-code-%d", n), verifier: fmt.Sprintf("verifier-%d", n)}
		s.devices = append(s.devices, d)
		writeJSON(w, http.StatusOK, map[string]any{"device_auth_id": d.id, "user_code": d.userCode, "interval": s.interval})
	case "/api/accounts/deviceauth/token":
		d := s.find(fields["device_auth_id"])
		switch {
		case d == nil || d.userCode != fields["user_code"]:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_device"})
		case d.slowDownOnce:
			d.slowDownOnce = false
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "slow_down"}})
		case d.state == StateDenied:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "access_denied"})
		case d.state == StateApproved:
			writeJSON(w, http.StatusOK, map[string]any{"authorization_code": d.code, "code_verifier": d.verifier})
		case d.pendingAsErr:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "deviceauth_authorization_pending"})
		default:
			writeJSON(w, http.StatusForbidden, map[string]any{})
		}
	case "/oauth/token":
		d := s.findByCode(fields["code"])
		if d == nil || fields["grant_type"] != "authorization_code" || fields["code_verifier"] != d.verifier ||
			fields["client_id"] == "" || fields["redirect_uri"] != s.server.URL+"/deviceauth/callback" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": AccessToken(d.accountID), "refresh_token": RefreshToken(d.accountID), "expires_in": 3600})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}

func (s *Simulator) find(id string) *device {
	for _, d := range s.devices {
		if d.id == id {
			return d
		}
	}
	return nil
}

func (s *Simulator) findByCode(code string) *device {
	for _, d := range s.devices {
		if d.code == code && d.state == StateApproved {
			return d
		}
	}
	return nil
}

func decodeBody(contentType string, body []byte) map[string]string {
	fields := map[string]string{}
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		values, _ := url.ParseQuery(string(body))
		for key := range values {
			fields[key] = values.Get(key)
		}
		return fields
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) == nil {
		for key, value := range raw {
			fields[key] = fmt.Sprint(value)
		}
	}
	return fields
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
