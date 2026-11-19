package googlesim

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	historyStart           = 1000
	defaultHistoryPageSize = 100
)

// history is a mailbox's messageAdded changes, numbered by a history id that only grows.
type history struct {
	last uint64
	// expired is the oldest start id Gmail still answers for.
	expired uint64
	records []historyRecord
}

type historyRecord struct {
	id      uint64
	message string
}

// SentMessage is a message sent through the Gmail API, as the client handed it over.
type SentMessage struct {
	Account  string
	ID       string
	ThreadID string
	Raw      []byte
}

// historyOf returns the mailbox's history; s.mu is held.
func (s *Simulator) historyOf(account string) *history {
	h, found := s.histories[account]
	if !found {
		h = &history{last: historyStart}
		s.histories[account] = h
	}
	return h
}

// addHistory records that the message was added to the mailbox; s.mu is held.
func (s *Simulator) addHistory(account, messageID string) {
	h := s.historyOf(account)
	h.last++
	h.records = append(h.records, historyRecord{id: h.last, message: messageID})
}

// SetHistoryPageSize sets how many changes a history page holds, Gmail's 100 by default.
func (s *Simulator) SetHistoryPageSize(size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyPageSize = size
}

// ExpireHistoryBefore makes a history read of the mailbox from a start id older than historyID
// answer 404, as Gmail does once it no longer keeps those changes.
func (s *Simulator) ExpireHistoryBefore(mailbox, historyID string) {
	s.t.Helper()
	id, err := strconv.ParseUint(historyID, 10, 64)
	if err != nil {
		s.t.Fatalf("history id %q: %v", historyID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyOf(mailbox).expired = id
}

// SlowSends makes every send go out and answer only after the delay, so a client that gives up
// sooner cannot tell whether it was sent; 0 answers at once again.
func (s *Simulator) SlowSends(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slowSends = delay
}

// FailSends makes every send answer the status; 0 lets sends succeed again.
func (s *Simulator) FailSends(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failingSends = status
}

func (s *Simulator) Sent() []SentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	sent := make([]SentMessage, 0, len(s.sent))
	for _, message := range s.sent {
		message.Raw = bytes.Clone(message.Raw)
		sent = append(sent, message)
	}
	return sent
}

// DeliverTo delivers the message the way Gmail delivers mail sent to recipient: a plus-addressed
// recipient lands in the base mailbox, with Delivered-To carrying the tag. To defaults to the
// recipient, Labels to INBOX (SPAM files it as spam), Received to now, the ids to new ones, and
// the thread to the one whose message it replies to. It returns the message's id.
func (s *Simulator) DeliverTo(recipient string, message Message) string {
	if message.Received.IsZero() {
		message.Received = s.clock()
	}
	message.DeliveredTo = recipient
	if message.To == "" {
		message.To = recipient
	}
	mailbox := baseAddress(recipient)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identify(mailbox, &message)
	if len(message.Headers) == 0 {
		message.Headers = deliveryHeaders(message)
	}
	s.store(mailbox, message)
	return message.ID
}

// DeliverRaw delivers a whole message as Gmail stored it, such as SampleDelivery, to the mailbox
// its first Delivered-To names, and serves its headers as they are. Labels default to INBOX; the
// received time is read from its newest Received header. It returns the message's id.
func (s *Simulator) DeliverRaw(raw []byte, labels ...string) string {
	s.t.Helper()
	message, err := parseMessage(raw)
	if err != nil || message.DeliveredTo == "" {
		s.t.Fatalf("delivered message has no header block or Delivered-To: %v", err)
	}
	message.Labels = labels
	message.Received = receivedTime(message.Headers)
	if message.Received.IsZero() {
		message.Received = s.clock()
	}
	mailbox := baseAddress(message.DeliveredTo)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identify(mailbox, &message)
	s.store(mailbox, message)
	return message.ID
}

// identify fills what Gmail assigns: the id, a Message-ID when the message has none, the thread
// of the message it replies to or a new one, and the INBOX label; s.mu is held.
func (s *Simulator) identify(mailbox string, message *Message) {
	s.counter++
	if message.ID == "" {
		message.ID = fmt.Sprintf("msg-%d", s.counter)
	}
	if message.MessageID == "" {
		message.MessageID = "<" + message.ID + "@googlesim.example>"
		if len(message.Headers) > 0 {
			message.Headers = append(message.Headers, Header{"Message-ID", message.MessageID})
		}
	}
	if message.ThreadID == "" {
		message.ThreadID = message.ID
		for _, earlier := range s.mailboxes[mailbox] {
			if earlier.MessageID != "" && (strings.Contains(message.InReplyTo, earlier.MessageID) || strings.Contains(message.References, earlier.MessageID)) {
				message.ThreadID = earlier.ThreadID
				break
			}
		}
	}
	if len(message.Labels) == 0 {
		message.Labels = []string{"INBOX"}
	}
}

// store adds the message to the mailbox and its history; s.mu is held.
func (s *Simulator) store(mailbox string, message Message) {
	s.mailboxes[mailbox] = append(s.mailboxes[mailbox], message)
	s.addHistory(mailbox, message.ID)
}

func (s *Simulator) serveSend(w http.ResponseWriter, r *http.Request, account string) {
	var body struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400}})
		return
	}
	raw, err := base64.URLEncoding.DecodeString(body.Raw)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(body.Raw)
	}
	message, parseErr := parseMessage(raw)
	if err != nil || parseErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400}})
		return
	}
	if message.To == "" && message.Cc == "" && headerValue(message.Headers, "Bcc") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "message": "Recipient address required"}})
		return
	}
	now := s.clock()
	s.mu.Lock()
	if status := s.failingSends; status != 0 {
		s.mu.Unlock()
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": status}})
		return
	}
	thread, known := s.sendThread(account, body.ThreadID, message)
	if !known {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404}})
		return
	}
	s.counter++
	message.ID = fmt.Sprintf("msg-%d", s.counter)
	message.ThreadID = cmp.Or(thread, message.ID)
	message.MessageID = "<" + message.ID + "@googlesim.example>"
	message.Headers = slices.DeleteFunc(message.Headers, func(header Header) bool { return strings.EqualFold(header.Name, "Message-ID") })
	message.Headers = append(message.Headers, Header{"Message-ID", message.MessageID})
	message.Labels = []string{"SENT"}
	message.Received = now
	s.identify(account, &message)
	s.store(account, message)
	s.sent = append(s.sent, SentMessage{Account: account, ID: message.ID, ThreadID: message.ThreadID, Raw: raw})
	delay := s.slowSends
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": message.ID, "threadId": message.ThreadID, "labelIds": message.Labels})
}

// sendThread is the thread a sent message joins, following Gmail: the named thread only when the
// message's In-Reply-To or References points into it and its subject matches without Re: and
// Fwd: prefixes, else a new one, given as "". known is false for a thread the mailbox lacks; s.mu
// is held.
func (s *Simulator) sendThread(account, threadID string, message Message) (thread string, known bool) {
	if threadID == "" {
		return "", true
	}
	for _, earlier := range s.mailboxes[account] {
		if earlier.ThreadID != threadID {
			continue
		}
		known = true
		if earlier.MessageID != "" && baseSubject(earlier.Subject) == baseSubject(message.Subject) &&
			(strings.Contains(message.InReplyTo, earlier.MessageID) || strings.Contains(message.References, earlier.MessageID)) {
			return threadID, true
		}
	}
	return "", known
}

func baseSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	for {
		lower := strings.ToLower(subject)
		trimmed := false
		for _, prefix := range []string{"re:", "fwd:", "fw:"} {
			if strings.HasPrefix(lower, prefix) {
				subject, trimmed = strings.TrimSpace(subject[len(prefix):]), true
				break
			}
		}
		if !trimmed {
			return subject
		}
	}
}

// RepeatInHistory adds another messageAdded record for a message already in the mailbox, as Gmail
// sometimes reports one message in more than one history record.
func (s *Simulator) RepeatInHistory(mailbox, messageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addHistory(mailbox, messageID)
}

func headerValue(headers []Header, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

func (s *Simulator) serveHistory(w http.ResponseWriter, r *http.Request, account string) {
	query := r.URL.Query()
	start, err := strconv.ParseUint(query.Get("startHistoryId"), 10, 64)
	after := start
	if token := query.Get("pageToken"); err == nil && token != "" {
		after, err = strconv.ParseUint(token, 10, 64)
	}
	if err != nil || after < start {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400}})
		return
	}
	s.mu.Lock()
	h := s.historyOf(account)
	if start < h.expired {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404}})
		return
	}
	messages := make(map[string]Message, len(s.mailboxes[account]))
	for _, message := range s.mailboxes[account] {
		messages[message.ID] = message
	}
	records := []map[string]any{}
	next := ""
	for _, record := range h.records {
		if record.id <= after {
			continue
		}
		if len(records) == s.historyPageSize {
			next = records[len(records)-1]["id"].(string)
			break
		}
		message := messages[record.message]
		records = append(records, map[string]any{
			"id":            strconv.FormatUint(record.id, 10),
			"messages":      []map[string]any{{"id": message.ID, "threadId": message.ThreadID}},
			"messagesAdded": []map[string]any{{"message": map[string]any{"id": message.ID, "threadId": message.ThreadID, "labelIds": message.Labels}}},
		})
	}
	current := h.last
	s.mu.Unlock()
	body := map[string]any{"history": records, "historyId": strconv.FormatUint(current, 10)}
	if next != "" {
		body["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Simulator) serveProfile(w http.ResponseWriter, account string) {
	s.mu.Lock()
	current := s.historyOf(account).last
	threads := make(map[string]bool)
	for _, message := range s.mailboxes[account] {
		threads[message.ThreadID] = true
	}
	total := len(s.mailboxes[account])
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"emailAddress": account, "messagesTotal": total, "threadsTotal": len(threads), "historyId": strconv.FormatUint(current, 10),
	})
}

// parseMessage reads an RFC 5322 message into the simulator's message, keeping its header block
// in order and spelling.
func parseMessage(raw []byte) (Message, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	head, body, _ := strings.Cut(text, "\n\n")
	var headers []Header
	for _, line := range strings.Split(head, "\n") {
		switch {
		case line == "":
		case line[0] == ' ' || line[0] == '\t':
			if len(headers) == 0 {
				return Message{}, errors.New("continuation before the first header")
			}
			headers[len(headers)-1].Value += " " + strings.TrimSpace(line)
		default:
			name, value, found := strings.Cut(line, ":")
			if !found || name == "" {
				return Message{}, fmt.Errorf("malformed header line %q", line)
			}
			headers = append(headers, Header{Name: name, Value: strings.TrimSpace(value)})
		}
	}
	if len(headers) == 0 {
		return Message{}, errors.New("no headers")
	}
	value := func(name string) string { return headerValue(headers, name) }
	subject, err := new(mime.WordDecoder).DecodeHeader(value("Subject"))
	if err != nil {
		subject = value("Subject")
	}
	plain := decodeTransfer(body, value("Content-Transfer-Encoding"))
	snippet := strings.Join(strings.Fields(plain), " ")
	if runes := []rune(snippet); len(runes) > 100 {
		snippet = string(runes[:100])
	}
	return Message{
		From: value("From"), To: value("To"), Cc: value("Cc"), ReplyTo: value("Reply-To"), Subject: subject,
		DeliveredTo: value("Delivered-To"), AuthenticationResults: value("Authentication-Results"),
		MessageID: value("Message-ID"), InReplyTo: value("In-Reply-To"), References: value("References"),
		PlainText: plain, Snippet: snippet, Headers: headers,
	}, nil
}

func decodeTransfer(body, encoding string) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		if decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), "")); err == nil {
			return string(decoded)
		}
	case "quoted-printable":
		if decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body))); err == nil {
			return string(decoded)
		}
	}
	return body
}

// receivedTime is when the newest Received header says the message arrived, which Gmail keeps as
// its internal date, or the Date header when there is none; zero when neither parses.
func receivedTime(headers []Header) time.Time {
	for _, header := range headers {
		if !strings.EqualFold(header.Name, "Received") {
			continue
		}
		if at := strings.LastIndex(header.Value, ";"); at >= 0 {
			if received, err := mail.ParseDate(strings.TrimSpace(header.Value[at+1:])); err == nil {
				return received
			}
		}
		break
	}
	for _, header := range headers {
		if strings.EqualFold(header.Name, "Date") {
			if date, err := mail.ParseDate(header.Value); err == nil {
				return date
			}
		}
	}
	return time.Time{}
}

// baseAddress is the mailbox a plus-addressed recipient belongs to.
func baseAddress(recipient string) string {
	address := plainAddress(recipient)
	local, domain, found := strings.Cut(address, "@")
	if !found {
		return address
	}
	local, _, _ = strings.Cut(local, "+")
	return local + "@" + domain
}

func plainAddress(value string) string {
	if parsed, err := mail.ParseAddress(value); err == nil {
		return parsed.Address
	}
	return strings.TrimSpace(value)
}
