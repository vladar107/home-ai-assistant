// Package googlesim is a loopback simulator of the Google endpoints the Google and Gmail connectors
// use: the consent screen as a function a test calls, the token, revocation, and userinfo
// endpoints with PKCE checks, and the Gmail API over a set of mailboxes: reads, history, and the
// send the assistant's own mailbox uses through its separate OAuth client. It records every
// request without tokens.
package googlesim

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simt"
)

const (
	ClientID      = "test-client.apps.googleusercontent.com"
	ClientSecret  = "test-client-secret"
	RedirectURL   = "http://localhost:8080/callback"
	mailScope     = "https://www.googleapis.com/auth/gmail.readonly"
	calendarScope = "https://www.googleapis.com/auth/calendar.readonly"
	requested     = "openid email " + mailScope + " " + calendarScope

	// AssistantClientID and AssistantClientSecret are the separate Desktop app client of the
	// assistant's own mailbox, which asks for the mailbox scopes only and redirects to any
	// loopback port.
	AssistantClientID     = "assistant-client.apps.googleusercontent.com"
	AssistantClientSecret = "assistant-client-secret"
	sendScope             = "https://www.googleapis.com/auth/gmail.send"
)

// Grant is what the user leaves ticked on the consent screen.
type Grant struct {
	Mail     bool
	Calendar bool
	// Send is gmail.send, which only the assistant mailbox's client asks for.
	Send bool
}

func (g Grant) scope() string {
	scope := "openid https://www.googleapis.com/auth/userinfo.email"
	if g.Mail {
		scope += " " + mailScope
	}
	if g.Calendar {
		scope += " " + calendarScope
	}
	if g.Send {
		scope += " " + sendScope
	}
	return scope
}

func (g Grant) assistantScope() string {
	var scopes []string
	if g.Mail {
		scopes = append(scopes, mailScope)
	}
	if g.Send {
		scopes = append(scopes, sendScope)
	}
	return strings.Join(scopes, " ")
}

func hasScope(scope, want string) bool {
	return slices.Contains(strings.Fields(scope), want)
}

type Message struct {
	ID        string
	ThreadID  string
	From      string
	To        string
	Subject   string
	Snippet   string
	Labels    []string
	Received  time.Time
	PlainText string
	HTML      string
	// Charset, when set, is declared on both alternatives, whose text then holds bytes in it.
	Charset     string
	Attachments map[string]int

	Cc                    string
	ReplyTo               string
	DeliveredTo           string
	AuthenticationResults string
	MessageID             string
	InReplyTo             string
	References            string
	// Headers, when set, is the message's whole header block in order and spelling, served in
	// place of headers made from the fields.
	Headers []Header
}

type Header struct {
	Name  string
	Value string
}

type Request struct {
	Method string
	Path   string
	Query  url.Values
	// Account is the mailbox the bearer token belonged to, empty for OAuth calls.
	Account string
}

type grant struct {
	client    string
	redirect  string
	account   string
	challenge string
	scope     string
}

type token struct {
	client  string
	account string
	scope   string
}

type Simulator struct {
	server *httptest.Server
	t      simt.T

	mu          sync.Mutex
	newestFirst bool
	mailboxes   map[string][]Message
	failing     map[string]int
	codes       map[string]grant
	refreshes   map[string]token
	accesses    map[string]token
	subjects    map[string]string
	revoked     []string
	requests    []Request
	counter     int
	now         func() time.Time

	histories       map[string]*history
	historyPageSize int
	// unindexed is how many more searches leave each message out.
	unindexed    map[string]int
	failingSends int
	sent         []SentMessage
}

func New(t simt.T) *Simulator {
	t.Helper()
	sim := &Simulator{
		t:               t,
		mailboxes:       make(map[string][]Message),
		failing:         make(map[string]int),
		unindexed:       make(map[string]int),
		codes:           make(map[string]grant),
		refreshes:       make(map[string]token),
		accesses:        make(map[string]token),
		subjects:        make(map[string]string),
		histories:       make(map[string]*history),
		historyPageSize: defaultHistoryPageSize,
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
	for _, message := range messages {
		s.addHistory(account, message.ID)
	}
}

// ListNewestFirst makes a search list the newest matches first, as Gmail does, so a search with a
// limit keeps the newest messages; by default a mailbox lists in the order its messages were added.
func (s *Simulator) ListNewestFirst() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newestFirst = true
}

// FailMessage makes every read of the message with the id fail with the status, while it is
// still listed by a search.
func (s *Simulator) FailMessage(id string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[id] = status
}

// IndexLate leaves the message out of the next searches, as Gmail's search does for a while
// after a message arrives; history and reads still find it.
func (s *Simulator) IndexLate(id string, searches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unindexed[id] = searches
}

// Consent plays the user on Google's consent screen for the given link: it checks the link the
// connector built and returns the address the browser would be sent to. granted is the products
// the user left ticked. A link of the assistant mailbox's client is checked against what that
// client asks for: gmail.readonly and gmail.send, either of which the user may untick.
func (s *Simulator) Consent(link, account string, granted Grant) string {
	s.t.Helper()
	parsed, err := url.Parse(link)
	if err != nil {
		s.t.Fatalf("consent link: %v", err)
	}
	query := parsed.Query()
	if query.Get("access_type") != "offline" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("state") == "" {
		s.t.Fatalf("consent link carries unexpected parameters: %v", query)
	}
	client, redirect := query.Get("client_id"), query.Get("redirect_uri")
	var scope string
	switch client {
	case AssistantClientID:
		asked := strings.Fields(query.Get("scope"))
		slices.Sort(asked)
		if !loopbackRedirect(redirect) || !slices.Equal(asked, []string{mailScope, sendScope}) {
			s.t.Fatalf("assistant consent link carries unexpected parameters: %v", query)
		}
		scope = granted.assistantScope()
	default:
		if client != ClientID || redirect != RedirectURL || query.Get("scope") != requested || query.Get("include_granted_scopes") != "true" || granted.Send {
			s.t.Fatalf("consent link carries unexpected parameters: %v", query)
		}
		scope = granted.scope()
	}
	s.mu.Lock()
	s.counter++
	code := fmt.Sprintf("4/code-%d", s.counter)
	s.codes[code] = grant{client: client, redirect: redirect, account: account, challenge: query.Get("code_challenge"), scope: scope}
	s.mu.Unlock()
	return redirect + "?" + url.Values{"state": {query.Get("state")}, "code": {code}, "scope": {scope}}.Encode()
}

// loopbackRedirect reports whether a Desktop app client may redirect to the address: plain HTTP
// to the loopback interface on a port the app chose.
func loopbackRedirect(redirect string) bool {
	parsed, err := url.Parse(redirect)
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" || parsed.RawQuery != "" {
		return false
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// Access issues an access token for the account with the products granted, as a finished consent
// would, for a test that reads the APIs without playing the consent screen.
func (s *Simulator) Access(account string, granted Grant) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counter++
	access := fmt.Sprintf("access-%d", s.counter)
	s.accesses[access] = token{account: account, scope: granted.scope()}
	return access
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
	case (r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/")) ||
		(r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/messages/send"):
		s.serveGmail(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Simulator) serveToken(w http.ResponseWriter, r *http.Request) {
	secrets := map[string]string{ClientID: ClientSecret, AssistantClientID: AssistantClientSecret}
	if err := r.ParseForm(); err != nil || secrets[r.PostForm.Get("client_id")] == "" || r.PostForm.Get("client_secret") != secrets[r.PostForm.Get("client_id")] {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	client := r.PostForm.Get("client_id")
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
		if !exists || base64.RawURLEncoding.EncodeToString(challenge[:]) != issued.challenge || r.PostForm.Get("redirect_uri") != issued.redirect || issued.client != client {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		account, scope = issued.account, issued.scope
		s.counter++
		refresh = fmt.Sprintf("refresh-%d", s.counter)
		s.refreshes[refresh] = token{client: client, account: account, scope: scope}
	case "refresh_token":
		issued := s.refreshes[r.PostForm.Get("refresh_token")]
		if issued.account == "" || issued.client != client {
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
	s.accesses[access] = token{client: client, account: account, scope: scope}
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
	newestFirst := s.newestFirst
	failing := s.failing[strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")]
	var unindexed map[string]bool
	if r.URL.Path == "/gmail/v1/users/me/messages" {
		unindexed = make(map[string]bool)
		for id, searches := range s.unindexed {
			unindexed[id] = true
			if searches <= 1 {
				delete(s.unindexed, id)
			} else {
				s.unindexed[id] = searches - 1
			}
		}
	}
	s.mu.Unlock()
	if newestFirst {
		slices.SortStableFunc(mailbox, func(a, b Message) int { return b.Received.Compare(a.Received) })
	}
	if account == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")
	needed := mailScope
	if r.Method == http.MethodPost {
		needed = sendScope
	}
	if !hasScope(issued.scope, needed) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 403}})
		return
	}
	switch {
	case r.Method == http.MethodPost:
		s.serveSend(w, r, account)
	case path == "history":
		s.serveHistory(w, r, account)
	case path == "profile":
		s.serveProfile(w, account)
	case path == "messages":
		limit, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		// The page token is the number of matches the earlier pages held.
		offset, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("pageToken"), "0"))
		if err != nil || offset < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400}})
			return
		}
		var matches []map[string]string
		now := s.clock()
		more := false
		skip := offset
		for _, message := range mailbox {
			if unindexed[message.ID] || !matchesQuery(message, r.URL.Query().Get("q"), now) {
				continue
			}
			if skip > 0 {
				skip--
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
			body["nextPageToken"] = strconv.Itoa(offset + len(matches))
		}
		writeJSON(w, http.StatusOK, body)
	case strings.HasPrefix(path, "messages/"):
		id := strings.TrimPrefix(path, "messages/")
		if failing != 0 {
			writeJSON(w, failing, map[string]any{"error": map[string]any{"code": failing}})
			return
		}
		for _, message := range mailbox {
			if message.ID == id {
				writeJSON(w, http.StatusOK, resource(message, r.URL.Query().Get("format"), r.URL.Query()["metadataHeaders"]))
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404}})
	default:
		http.NotFound(w, r)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// resource is the message as Gmail serves it. A metadata read with metadataHeaders keeps only the
// headers named, matched without regard to case.
func resource(message Message, format string, wanted []string) map[string]any {
	headers := []map[string]string{}
	for _, header := range messageHeaders(message) {
		if format == "metadata" && len(wanted) > 0 && !slices.ContainsFunc(wanted, func(name string) bool { return strings.EqualFold(name, header.Name) }) {
			continue
		}
		headers = append(headers, map[string]string{"name": header.Name, "value": header.Value})
	}
	payload := map[string]any{"mimeType": "multipart/mixed", "headers": headers}
	if format == "full" {
		var parts []map[string]any
		alternative := []map[string]any{}
		if message.PlainText != "" {
			alternative = append(alternative, bodyPart("text/plain", message.PlainText, message.Charset))
		}
		if message.HTML != "" {
			alternative = append(alternative, bodyPart("text/html", message.HTML, message.Charset))
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

// messageHeaders is the message's header block: its own when it has one, else the headers its
// fields give, the optional ones only when set.
func messageHeaders(message Message) []Header {
	if len(message.Headers) > 0 {
		return message.Headers
	}
	headers := []Header{{"From", message.From}, {"To", message.To}}
	for _, optional := range []Header{
		{"Cc", message.Cc}, {"Reply-To", message.ReplyTo}, {"Delivered-To", message.DeliveredTo},
		{"Authentication-Results", message.AuthenticationResults}, {"Message-ID", message.MessageID},
		{"In-Reply-To", message.InReplyTo}, {"References", message.References},
	} {
		if optional.Value != "" {
			headers = append(headers, optional)
		}
	}
	return append(headers, Header{"Subject", message.Subject}, Header{"Date", message.Received.Format(time.RFC1123Z)})
}

func bodyPart(mimeType, text, charset string) map[string]any {
	part := map[string]any{"mimeType": mimeType, "body": map[string]any{"size": len(text), "data": base64.URLEncoding.EncodeToString([]byte(text))}}
	if charset != "" {
		part["headers"] = []map[string]string{{"name": "Content-Type", "value": mimeType + "; charset=" + charset}}
	}
	return part
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
