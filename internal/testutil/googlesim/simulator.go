// Package googlesim is a loopback simulator of the Google endpoints the Google and Gmail connectors
// use: the consent screen as a function a test calls, the token, revocation, and userinfo
// endpoints with PKCE checks, and the read-only Gmail API over a set of mailboxes. It records every
// request without tokens.
package googlesim

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ClientID      = "test-client.apps.googleusercontent.com"
	ClientSecret  = "test-client-secret"
	RedirectURL   = "http://localhost:8080/callback"
	mailScope     = "https://www.googleapis.com/auth/gmail.readonly"
	calendarScope = "https://www.googleapis.com/auth/calendar.readonly"
	requested     = "openid email " + mailScope + " " + calendarScope
)

// Grant is what the user leaves ticked on the consent screen.
type Grant struct {
	Mail     bool
	Calendar bool
}

func (g Grant) scope() string {
	scope := "openid https://www.googleapis.com/auth/userinfo.email"
	if g.Mail {
		scope += " " + mailScope
	}
	if g.Calendar {
		scope += " " + calendarScope
	}
	return scope
}

type Message struct {
	ID          string
	ThreadID    string
	From        string
	To          string
	Subject     string
	Snippet     string
	Labels      []string
	Received    time.Time
	PlainText   string
	HTML        string
	Attachments map[string]int
}

type Request struct {
	Method string
	Path   string
	Query  url.Values
	// Account is the mailbox the bearer token belonged to, empty for OAuth calls.
	Account string
}

type grant struct {
	account   string
	challenge string
	scope     string
}

type token struct {
	account string
	scope   string
}

type Simulator struct {
	server *httptest.Server
	t      *testing.T

	mu        sync.Mutex
	mailboxes map[string][]Message
	codes     map[string]grant
	refreshes map[string]token
	accesses  map[string]token
	subjects  map[string]string
	revoked   []string
	requests  []Request
	counter   int
}

func New(t *testing.T) *Simulator {
	t.Helper()
	sim := &Simulator{
		t:         t,
		mailboxes: make(map[string][]Message),
		codes:     make(map[string]grant),
		refreshes: make(map[string]token),
		accesses:  make(map[string]token),
		subjects:  make(map[string]string),
	}
	sim.server = httptest.NewServer(http.HandlerFunc(sim.serve))
	t.Cleanup(sim.server.Close)
	return sim
}

func (s *Simulator) URL() string          { return s.server.URL }
func (s *Simulator) Client() *http.Client { return s.server.Client() }
func (s *Simulator) AuthURL() string      { return s.server.URL + "/o/oauth2/v2/auth" }
func (s *Simulator) TokenURL() string     { return s.server.URL + "/token" }
func (s *Simulator) RevokeURL() string    { return s.server.URL + "/revoke" }
func (s *Simulator) UserInfoURL() string  { return s.server.URL + "/v1/userinfo" }

// Subject is the stable Google subject id of the account, assigned on first use.
func (s *Simulator) Subject(account string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subject(account)
}

func (s *Simulator) subject(account string) string {
	if id, found := s.subjects[account]; found {
		return id
	}
	id := strconv.Itoa(100000000000000000 + len(s.subjects) + 1)
	s.subjects[account] = id
	return id
}

// AccountFor reports which account an access token belongs to, for a simulator of another Google
// API that accepts this one's tokens.
func (s *Simulator) AccountFor(access string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issued, found := s.accesses[access]
	return issued.account, found
}
func (s *Simulator) AddMailbox(account string, messages ...Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes[account] = append(s.mailboxes[account], messages...)
}

// Consent plays the user on Google's consent screen for the given link: it checks the link the
// connector built and returns the address the browser would be sent to. grantGmail false is the
// products the user left ticked.
func (s *Simulator) Consent(link, account string, granted Grant) string {
	s.t.Helper()
	parsed, err := url.Parse(link)
	if err != nil {
		s.t.Fatalf("consent link: %v", err)
	}
	query := parsed.Query()
	if query.Get("client_id") != ClientID || query.Get("redirect_uri") != RedirectURL || query.Get("scope") != requested || query.Get("include_granted_scopes") != "true" ||
		query.Get("access_type") != "offline" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("state") == "" {
		s.t.Fatalf("consent link carries unexpected parameters: %v", query)
	}
	scope := granted.scope()
	s.mu.Lock()
	s.counter++
	code := fmt.Sprintf("4/code-%d", s.counter)
	s.codes[code] = grant{account: account, challenge: query.Get("code_challenge"), scope: scope}
	s.mu.Unlock()
	return RedirectURL + "?" + url.Values{"state": {query.Get("state")}, "code": {code}, "scope": {scope}}.Encode()
}

// RevokeGrant is the user removing the app from their Google account.
func (s *Simulator) RevokeGrant(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for value, issued := range s.refreshes {
		if issued.account == account {
			delete(s.refreshes, value)
		}
	}
	for value, issued := range s.accesses {
		if issued.account == account {
			delete(s.accesses, value)
		}
	}
}

// ExpireAccessTokens makes every issued access token fail with 401, as an hour passing does.
func (s *Simulator) ExpireAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accesses = make(map[string]token)
}

func (s *Simulator) Revoked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.revoked...)
}

func (s *Simulator) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Simulator) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/token":
		s.serveToken(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/revoke":
		_ = r.ParseForm()
		s.mu.Lock()
		value := r.PostForm.Get("token")
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path})
		account := s.refreshes[value].account
		if account == "" {
			account = s.accesses[value].account
		}
		s.mu.Unlock()
		if account != "" {
			s.RevokeGrant(account)
			s.mu.Lock()
			s.revoked = append(s.revoked, account)
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/userinfo":
		s.mu.Lock()
		issued, found := s.accesses[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Account: issued.account})
		var subject string
		if found {
			subject = s.subject(issued.account)
		}
		s.mu.Unlock()
		if !found {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sub": subject, "email": issued.account, "email_verified": true})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/"):
		s.serveGmail(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Simulator) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: url.Values{"grant_type": {r.PostForm.Get("grant_type")}}})
	var account, scope, refresh string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		issued, exists := s.codes[code]
		delete(s.codes, code)
		challenge := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !exists || base64.RawURLEncoding.EncodeToString(challenge[:]) != issued.challenge || r.PostForm.Get("redirect_uri") != RedirectURL {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		account, scope = issued.account, issued.scope
		s.counter++
		refresh = fmt.Sprintf("refresh-%d", s.counter)
		s.refreshes[refresh] = token{account: account, scope: scope}
	case "refresh_token":
		issued := s.refreshes[r.PostForm.Get("refresh_token")]
		if issued.account == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		account, scope = issued.account, issued.scope
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	s.counter++
	access := fmt.Sprintf("access-%d", s.counter)
	s.accesses[access] = token{account: account, scope: scope}
	body := map[string]any{"access_token": access, "expires_in": 3599, "scope": scope, "token_type": "Bearer"}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Simulator) serveGmail(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	issued := s.accesses[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	account := issued.account
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Account: account})
	mailbox := append([]Message(nil), s.mailboxes[account]...)
	s.mu.Unlock()
	if account == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401}})
		return
	}
	if !strings.Contains(issued.scope, mailScope) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 403}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")
	switch {
	case path == "messages":
		limit, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		var matches []map[string]string
		more := false
		for _, message := range mailbox {
			if !matchesQuery(message, r.URL.Query().Get("q")) {
				continue
			}
			if limit > 0 && len(matches) == limit {
				more = true
				break
			}
			matches = append(matches, map[string]string{"id": message.ID, "threadId": message.ThreadID})
		}
		body := map[string]any{"messages": matches, "resultSizeEstimate": len(matches)}
		if more {
			body["nextPageToken"] = "next"
		}
		writeJSON(w, http.StatusOK, body)
	case strings.HasPrefix(path, "messages/"):
		id := strings.TrimPrefix(path, "messages/")
		for _, message := range mailbox {
			if message.ID == id {
				writeJSON(w, http.StatusOK, resource(message, r.URL.Query().Get("format")))
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404}})
	default:
		http.NotFound(w, r)
	}
}

// matchesQuery understands is:unread and plain words matched against subject, sender, and
// snippet, which is all the tests search with.
func matchesQuery(message Message, query string) bool {
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if term == "is:unread" {
			if !contains(message.Labels, "UNREAD") {
				return false
			}
			continue
		}
		haystack := strings.ToLower(message.Subject + " " + message.From + " " + message.Snippet)
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func resource(message Message, format string) map[string]any {
	headers := []map[string]string{
		{"name": "From", "value": message.From},
		{"name": "To", "value": message.To},
		{"name": "Subject", "value": message.Subject},
		{"name": "Date", "value": message.Received.Format(time.RFC1123Z)},
	}
	payload := map[string]any{"mimeType": "multipart/mixed", "headers": headers}
	if format == "full" {
		var parts []map[string]any
		alternative := []map[string]any{}
		if message.PlainText != "" {
			alternative = append(alternative, bodyPart("text/plain", message.PlainText))
		}
		if message.HTML != "" {
			alternative = append(alternative, bodyPart("text/html", message.HTML))
		}
		parts = append(parts, map[string]any{"mimeType": "multipart/alternative", "parts": alternative})
		for name, size := range message.Attachments {
			parts = append(parts, map[string]any{"mimeType": "application/pdf", "filename": name, "body": map[string]any{"size": size, "attachmentId": "att-" + name}})
		}
		payload["parts"] = parts
	}
	return map[string]any{
		"id": message.ID, "threadId": message.ThreadID, "labelIds": message.Labels, "snippet": message.Snippet,
		"internalDate": strconv.FormatInt(message.Received.UnixMilli(), 10), "payload": payload,
	}
}

func bodyPart(mimeType, text string) map[string]any {
	return map[string]any{"mimeType": mimeType, "body": map[string]any{"size": len(text), "data": base64.URLEncoding.EncodeToString([]byte(text))}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
