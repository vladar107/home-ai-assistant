package telegram

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/events"
	"github.com/vladar107/home-ai-assistant/internal/observability"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

var eventTokenPattern = regexp.MustCompile(`hae_[A-Za-z0-9_-]{43}`)

// newEventsHarness runs the chat with inbound events on, and the events endpoint on a listener of
// its own, as the application mounts it.
func newEventsHarness(t *testing.T, runtime agentruntime.AgentRuntime, options harnessOptions) (*conversationHarness, string) {
	t.Helper()
	options.events = true
	h := newConversationHarnessWithOptions(t, runtime, options)
	mux := http.NewServeMux()
	events.Register(mux, h.events, observability.Noop())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return h, server.URL + "/api/events"
}

// addEventSource adds a source through /settings and returns its token, taken from the message
// that showed it, and that message's ID.
func (h *conversationHarness) addEventSource(updateID, userID int64, name string) (string, int64) {
	h.t.Helper()
	h.requestSettings(updateID, userID)
	h.press(updateID+1, userID, settingsMessageID(updateID), "events:add")
	if prompt := h.nextMessage(); prompt.chatID != userID || prompt.text != EventsNameReply {
		h.t.Fatalf("add prompt = %+v", prompt)
	}
	h.push(textUpdate(updateID+2, userID, name))
	shown := h.nextMessage()
	token := eventTokenPattern.FindString(shown.text)
	if shown.chatID != userID || token == "" || shown.parseMode != "HTML" || !strings.Contains(shown.text, "<b>"+name+"</b>") {
		h.t.Fatalf("token message = %+v", shown)
	}
	if created := h.nextMessage(); created.chatID != userID || created.text != renderMarkup(EventsCreatedReply(name)) {
		h.t.Fatalf("created reply = %+v", created)
	}
	return token, shown.messageID
}

func (h *conversationHarness) eventSourceID(principalID, name string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM event_sources WHERE principal_id = ? AND name = ?`, principalID, name).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *conversationHarness) inboundEvents(outcome string) int {
	h.t.Helper()
	return h.countRows(`SELECT count(*) FROM inbound_events WHERE outcome = ?`, outcome)
}

func (h *conversationHarness) allInboundEvents() int {
	h.t.Helper()
	return h.countRows(`SELECT count(*) FROM inbound_events`)
}

// waitEventOutcome waits for the event's delivery to end with the outcome.
func (h *conversationHarness) waitEventOutcome(eventID, outcome string) {
	h.t.Helper()
	for {
		var got string
		if err := h.db.QueryRowContext(h.ctx, `SELECT outcome FROM inbound_events WHERE id = ?`, eventID).Scan(&got); err != nil {
			h.t.Fatal(err)
		}
		if got == outcome {
			return
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			h.t.Fatalf("event %s is %s, want %s", eventID, got, outcome)
		}
	}
}

type eventReply struct {
	ID        string `json:"id"`
	Duplicate bool   `json:"duplicate"`
	Error     string `json:"error"`
}

func postEvent(t *testing.T, url, authorization, body string) (int, eventReply) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var reply eventReply
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		t.Fatalf("decode event reply: %v", err)
	}
	return response.StatusCode, reply
}

func eventBody(title, text, severity, dedupe string) string {
	body := map[string]string{"title": title, "text": text, "severity": severity}
	if dedupe != "" {
		body["dedupe_key"] = dedupe
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func TestEventsReachOnlyTheirSourcesOwnerAndNeedAValidToken(t *testing.T) {
	t.Parallel()
	h, url := newEventsHarness(t, newControlledRuntime(), harnessOptions{topics: true})
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	alice, _ := h.addEventSource(1, 101, "Backups")
	bob, _ := h.addEventSource(10, 202, "Home automations")

	forged := "hae_" + strings.Repeat("A", 43)
	for _, authorization := range []string{"", "Basic " + alice, "Bearer " + forged, "Bearer " + alice[:20], "Bearer " + alice + "x"} {
		if status, reply := postEvent(t, url, authorization, eventBody("Backup done", "", "info", "")); status != http.StatusUnauthorized || reply.Error != "unauthorized" {
			t.Fatalf("authorization %q got %d %+v", authorization, status, reply)
		}
	}
	if recorded := h.allInboundEvents(); recorded != 0 {
		t.Fatalf("rejected posts recorded %d events", recorded)
	}

	// Event text reaches the chat as it is: nothing in it becomes formatting or a link.
	text := "Disk **full** <b>now</b> [details](https://example.com/x) `rm -rf`\n> quoted"
	status, reply := postEvent(t, url, "Bearer "+alice, eventBody("Nightly *backup* <i>failed</i>", text, "warning", ""))
	if status != http.StatusAccepted || reply.ID == "" || reply.Duplicate {
		t.Fatalf("valid post = %d %+v", status, reply)
	}
	feed := h.wantFeedTab("Events", thread.FeedEvents)
	delivered := h.nextMessage()
	want := "⚠️ <b>Backups</b>\n<b>Nightly *backup* &lt;i&gt;failed&lt;/i&gt;</b>\n\n" +
		"Disk **full** &lt;b&gt;now&lt;/b&gt; [details](https://example.com/x) `rm -rf`\n&gt; quoted"
	if delivered.chatID != 101 || delivered.threadID != feed || delivered.parseMode != "HTML" || delivered.text != want {
		t.Fatalf("delivered = %+v\nwant text %q in tab %d", delivered, want, feed)
	}
	h.waitEventOutcome(reply.ID, events.OutcomeDelivered)

	status, reply = postEvent(t, url, "bearer "+bob, eventBody("Lights left on", "", "info", ""))
	if status != http.StatusAccepted {
		t.Fatalf("bob's post = %d %+v", status, reply)
	}
	if delivered := h.nextMessage(); delivered.chatID != 202 || !strings.Contains(delivered.text, "Lights left on") || strings.Contains(delivered.text, "Backups") {
		t.Fatalf("bob's event = %+v", delivered)
	}
	h.waitEventOutcome(reply.ID, events.OutcomeDelivered)
	if counted := h.countRows(`SELECT count(*) FROM inbound_events WHERE principal_id = 'alice'`); counted != 1 {
		t.Fatalf("alice has %d events", counted)
	}

	// A paused source is refused and records nothing; resumed, it is taken again.
	sourceID := h.eventSourceID("alice", "Backups")
	if edit := h.press(20, 101, settingsMessageID(1), "events:pause:"+sourceID); !strings.Contains(edit.text, "Backups: forward, paused") {
		t.Fatalf("paused page = %q", edit.text)
	}
	before := h.allInboundEvents()
	if status, reply := postEvent(t, url, "Bearer "+alice, eventBody("Backup done", "", "info", "")); status != http.StatusForbidden || reply.Error != "paused" {
		t.Fatalf("paused post = %d %+v", status, reply)
	}
	if h.allInboundEvents() != before {
		t.Fatal("a paused source recorded an event")
	}
	h.press(21, 101, settingsMessageID(1), "events:resume:"+sourceID)

	// A new token replaces the old one at once.
	h.push(callbackUpdate(22, 101, settingsMessageID(1), "events:token:"+sourceID))
	h.wantAnswer(22)
	shown := h.nextMessage()
	rotated := eventTokenPattern.FindString(shown.text)
	if rotated == "" || rotated == alice {
		t.Fatalf("rotated token message = %+v", shown)
	}
	h.nextEdit()
	if status, _ := postEvent(t, url, "Bearer "+alice, eventBody("Backup done", "", "info", "")); status != http.StatusUnauthorized {
		t.Fatalf("revoked token got %d", status)
	}
	if status, _ := postEvent(t, url, "Bearer "+rotated, eventBody("Backup done", "", "info", "")); status != http.StatusAccepted {
		t.Fatalf("rotated token got %d", status)
	}
	h.nextMessage()

	// A token pasted into the chat is deleted and never reaches the model.
	h.push(textUpdate(23, 101, "is this right? "+rotated))
	if deleted := receive(t, h.ctx, h.deletes); deleted.chatID != 101 || deleted.messageID != requestMessageID(23) {
		t.Fatalf("deleted %+v", deleted)
	}
	if guard := h.nextMessage(); guard.text != EventsTokenGuard {
		t.Fatalf("guard reply = %+v", guard)
	}

	// A principal taken off the allowlist loses their sources' tokens with it.
	if _, err := h.db.ExecContext(h.ctx, `UPDATE telegram_allowlist SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+1 second') WHERE principal_id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	before = h.allInboundEvents()
	if status, _ := postEvent(t, url, "Bearer "+rotated, eventBody("Backup done", "", "info", "")); status != http.StatusUnauthorized {
		t.Fatalf("revoked principal's token got %d", status)
	}
	if h.allInboundEvents() != before {
		t.Fatal("a revoked principal's post recorded an event")
	}

	logs := h.logs.String()
	for _, token := range []string{alice, bob, rotated} {
		if strings.Contains(logs, token) {
			t.Fatal("a token reached the logs")
		}
	}
	if strings.Contains(logs, "Disk **full**") || strings.Contains(logs, "Lights left on") {
		t.Fatal("an event body reached the logs")
	}
}

func TestEventBodiesDuplicatesAndTheHourlyLimit(t *testing.T) {
	t.Parallel()
	h, url := newEventsHarness(t, newControlledRuntime(), harnessOptions{eventBodyTimeout: 300 * time.Millisecond})
	h.start()
	token, _ := h.addEventSource(1, 101, "Backups")
	bearer := "Bearer " + token

	for name, body := range map[string]string{
		"oversized":     eventBody("Backup done", strings.Repeat("x", events.MaximumBodyBytes), "info", ""),
		"not JSON":      "title=Backup",
		"unknown field": `{"title":"Backup","link":"https://example.com"}`,
		"no title":      eventBody(" ", "text", "info", ""),
		"long title":    eventBody(strings.Repeat("t", 201), "", "info", ""),
		"long text":     eventBody("Backup", strings.Repeat("é", 4001), "info", ""),
		"severity":      eventBody("Backup", "", "fatal", ""),
		"empty dedupe":  `{"title":"Backup","dedupe_key":""}`,
		"NUL in title":  eventBody("Backup\x00done", "", "info", ""),
		"NUL in text":   eventBody("Backup", "line one\nline\x00two", "info", ""),
		"NUL in dedupe": eventBody("Backup", "", "info", "key\x00"),
		"bell in title": eventBody("Backup\adone", "", "info", ""),
	} {
		if status, reply := postEvent(t, url, bearer, body); status != http.StatusBadRequest || reply.Error == "" {
			t.Fatalf("%s body got %d %+v", name, status, reply)
		}
	}

	// A body that trickles in past the read deadline is cut off, with a token or without one.
	trickle(t, url, bearer, http.StatusBadRequest)
	trickle(t, url, "Bearer hae_"+strings.Repeat("B", 43), http.StatusUnauthorized)
	if recorded := h.allInboundEvents(); recorded != 0 {
		t.Fatalf("rejected bodies recorded %d events", recorded)
	}

	// The same dedupe key within the hour is counted but delivered once.
	status, first := postEvent(t, url, bearer, eventBody("Backup failed", "", "warning", "backup-1"))
	if status != http.StatusAccepted || first.Duplicate {
		t.Fatalf("first post = %d %+v", status, first)
	}
	if delivered := h.nextMessage(); !strings.Contains(delivered.text, "Backup failed") {
		t.Fatalf("delivered = %+v", delivered)
	}
	h.waitEventOutcome(first.ID, events.OutcomeDelivered)
	for range events.HourlyLimit - 1 {
		if status, reply := postEvent(t, url, bearer, eventBody("Backup failed", "", "warning", "backup-1")); status != http.StatusAccepted || !reply.Duplicate {
			t.Fatalf("duplicate post = %d %+v", status, reply)
		}
	}
	if status, reply := postEvent(t, url, bearer, eventBody("Backup failed", "", "warning", "backup-2")); status != http.StatusTooManyRequests || reply.Error != "rate_limited" {
		t.Fatalf("post over the limit = %d %+v", status, reply)
	}
	if delivered, duplicates, all := h.inboundEvents(events.OutcomeDelivered), h.inboundEvents(events.OutcomeDuplicate), h.allInboundEvents(); delivered != 1 || duplicates != events.HourlyLimit-1 || all != events.HourlyLimit {
		t.Fatalf("events: %d delivered, %d duplicates, %d in all", delivered, duplicates, all)
	}
	select {
	case extra := <-h.messages:
		t.Fatalf("a duplicate was delivered: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}

	// Posts at once still admit exactly the hourly limit.
	second, _ := h.addEventSource(20, 101, "Second")
	sourceID := h.eventSourceID("alice", "Second")
	statuses := make(chan int, events.HourlyLimit+10)
	var posting sync.WaitGroup
	for range events.HourlyLimit + 10 {
		posting.Go(func() {
			status, _ := postEvent(t, url, "Bearer "+second, eventBody("Disk check", "", "info", "disk"))
			statuses <- status
		})
	}
	posting.Wait()
	close(statuses)
	counted := map[int]int{}
	for status := range statuses {
		counted[status]++
	}
	if counted[http.StatusAccepted] != events.HourlyLimit || counted[http.StatusTooManyRequests] != 10 || len(counted) != 2 {
		t.Fatalf("concurrent posts = %v", counted)
	}
	if recorded := h.countRows(`SELECT count(*) FROM inbound_events WHERE source_id = ?`, sourceID); recorded != events.HourlyLimit {
		t.Fatalf("concurrent posts recorded %d events", recorded)
	}
	if delivered := h.nextMessage(); !strings.Contains(delivered.text, "Disk check") {
		t.Fatalf("delivered = %+v", delivered)
	}
}

// trickle sends a post whose body arrives a byte at a time, and wants it refused with the status
// long before the body could have been read to its end.
func trickle(t *testing.T, url, authorization string, want int) {
	t.Helper()
	address := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/api/events")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	body := eventBody("Slow", strings.Repeat("s", 40), "info", "")
	if _, err := fmt.Fprintf(connection, "POST /api/events HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		address, authorization, len(body), body[:3]); err != nil {
		t.Fatal(err)
	}
	cut := make(chan int, 1)
	go func() {
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			cut <- 0
			return
		}
		_ = response.Body.Close()
		cut <- response.StatusCode
	}()
	for _, b := range []byte(body[3:]) {
		select {
		case status := <-cut:
			if status != want {
				t.Fatalf("trickled body with %q got %d, want %d", authorization[:10], status, want)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
		_, _ = connection.Write([]byte{b})
	}
	t.Fatalf("the trickled body with %q was read to its end", authorization[:10])
}

func TestAssessedEventsAreToldOnlyWhenWorthIt(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, url := newEventsHarness(t, runtime, harnessOptions{})
	h.provision("alice", "acct-alice-1111")
	h.start()
	token, _ := h.addEventSource(1, 101, "Backups")
	bearer := "Bearer " + token
	sourceID := h.eventSourceID("alice", "Backups")
	if edit := h.press(4, 101, settingsMessageID(1), "events:mode:"+sourceID); !strings.Contains(edit.text, "Backups: assess, on") {
		t.Fatalf("assess page = %q", edit.text)
	}

	assessed := func(title, severity, answer string) string {
		t.Helper()
		status, reply := postEvent(t, url, bearer, eventBody(title, "Took 3h instead of 1h.", severity, ""))
		if status != http.StatusAccepted {
			t.Fatalf("post = %d %+v", status, reply)
		}
		call := receive(t, h.ctx, runtime.calls)
		if call.request.PrincipalID != "alice" || len(call.request.Tools.Definitions) != 0 || call.request.WebSearch ||
			!strings.Contains(call.request.Message, `event source "Backups"`) || !strings.Contains(call.request.Message, "Title: "+title) {
			t.Fatalf("assessment request = %+v", call.request)
		}
		call.answer <- agentruntime.RunResult{Final: answer}
		return reply.ID
	}

	quiet := assessed("Backup done", "info", `{"notify":false,"message":""}`)
	h.waitEventOutcome(quiet, events.OutcomeSuppressed)

	critical := assessed("Backup disk failing", "critical", `{"notify":false,"message":""}`)
	if delivered := h.nextMessage(); !strings.HasPrefix(delivered.text, "🚨 <b>Backups</b>\n<b>Backup disk failing</b>") || !strings.Contains(delivered.text, "Took 3h") {
		t.Fatalf("critical event = %+v", delivered)
	}
	h.waitEventOutcome(critical, events.OutcomeDelivered)

	worded := assessed("Backup slow", "warning", "Worth a look.\n```json\n{\"notify\":true,\"message\":\"Your backup ran <b>3h</b>; check the disk.\"}\n```")
	if delivered := h.nextMessage(); delivered.text != "⚠️ <b>Backups</b>\n<b>Backup slow</b>\n\nYour backup ran &lt;b&gt;3h&lt;/b&gt;; check the disk." {
		t.Fatalf("worded event = %+v", delivered)
	}
	h.waitEventOutcome(worded, events.OutcomeDelivered)

	// An assessment without a decision, or a failed turn, forwards the event as it is.
	failed := assessed("Backup stuck", "info", "I am not sure.")
	if delivered := h.nextMessage(); delivered.text != "ℹ️ <b>Backups</b>\n<b>Backup stuck</b>\n\nTook 3h instead of 1h." {
		t.Fatalf("forwarded after a failed assessment = %+v", delivered)
	}
	h.waitEventOutcome(failed, events.OutcomeDelivered)
	broken := assessed("Backup paused", "info", "")
	if delivered := h.nextMessage(); delivered.text != "ℹ️ <b>Backups</b>\n<b>Backup paused</b>\n\nTook 3h instead of 1h." {
		t.Fatalf("forwarded after a failed turn = %+v", delivered)
	}
	h.waitEventOutcome(broken, events.OutcomeDelivered)

	// The next chat turn hears of the delivered events as outside content.
	h.push(textUpdate(5, 101, "what did the backup say?"))
	call := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, `The user's event source "Backups" just sent them this message`) ||
		!strings.Contains(call.request.Message, "Backup paused") || strings.Contains(call.request.Message, "Backup done") {
		t.Fatalf("chat turn message = %q", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: "It is stuck.", Final: "It is stuck."}
	h.nextMessage()
	if h.externalSessions() != 1 {
		t.Fatal("the conversation that heard of an event is not marked as having read outside content")
	}
}

func TestATokenMessageDueDuringARestartIsDeletedAfterStart(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, mondayMorning)
	h, _ := newEventsHarness(t, newControlledRuntime(), harnessOptions{clock: clock})
	h.start()
	_, messageID := h.addEventSource(1, 101, "Backups")
	h.stop()
	select {
	case deleted := <-h.deletes:
		t.Fatalf("the token message was deleted early: %+v", deleted)
	default:
	}
	if due := h.countRows(`SELECT count(*) FROM event_token_messages WHERE message_id = ?`, messageID); due != 1 {
		t.Fatalf("%d deletions recorded", due)
	}

	clock.Set(t, "2026-09-28T08:11:00+02:00")
	h.start()
	if deleted := receive(t, h.ctx, h.deletes); deleted.chatID != 101 || deleted.messageID != messageID {
		t.Fatalf("deleted %+v, want message %d", deleted, messageID)
	}
	for h.countRows(`SELECT count(*) FROM event_token_messages`) != 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			t.Fatal("the deleted token message was never forgotten")
		}
	}
}

// assessSource makes the principal's only source assess its events.
func (h *conversationHarness) assessSource(updateID, userID int64, principalID, name string) {
	h.t.Helper()
	sourceID := h.eventSourceID(principalID, name)
	if edit := h.press(updateID, userID, settingsMessageID(updateID), "events:mode:"+sourceID); !strings.Contains(edit.text, name+": assess") {
		h.t.Fatalf("assess page = %q", edit.text)
	}
}

func (h *conversationHarness) failNextSend() {
	h.mu.Lock()
	h.sendStatuses = []int{http.StatusInternalServerError}
	h.mu.Unlock()
}

func TestARetriedDeliveryReusesItsAssessment(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, url := newEventsHarness(t, runtime, harnessOptions{})
	h.provision("alice", "acct-alice-1111")
	h.start()
	token, _ := h.addEventSource(1, 101, "Backups")
	h.assessSource(4, 101, "alice", "Backups")

	h.failNextSend()
	status, reply := postEvent(t, url, "Bearer "+token, eventBody("Backup slow", "Took 3h.", "warning", ""))
	if status != http.StatusAccepted {
		t.Fatalf("post = %d %+v", status, reply)
	}
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Final: `{"notify":true,"message":"Check the backup disk."}`}
	if refused := h.nextMessage(); refused.status != http.StatusInternalServerError || !strings.Contains(refused.text, "Check the backup disk.") {
		t.Fatalf("first send = %+v", refused)
	}
	for h.countRows(`SELECT count(*) FROM inbound_events WHERE id = ? AND assessment = 'notify' AND assessed_message = 'Check the backup disk.'`, reply.ID) != 1 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			t.Fatal("the assessment was never kept")
		}
	}

	h.events.Wake()
	if retried := h.nextMessage(); retried.status != http.StatusOK || !strings.Contains(retried.text, "Check the backup disk.") {
		t.Fatalf("retried send = %+v", retried)
	}
	h.waitEventOutcome(reply.ID, events.OutcomeDelivered)
	select {
	case call := <-runtime.calls:
		t.Fatalf("the retry assessed the event again: %+v", call.request)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestASlowAssessmentDoesNotHoldAnotherUsersEvents(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, url := newEventsHarness(t, runtime, harnessOptions{})
	h.provision("alice", "acct-alice-1111")
	h.start()
	alice, _ := h.addEventSource(1, 101, "Backups")
	h.assessSource(4, 101, "alice", "Backups")
	bob, _ := h.addEventSource(10, 202, "Home automations")

	if status, _ := postEvent(t, url, "Bearer "+alice, eventBody("Backup done", "", "info", "")); status != http.StatusAccepted {
		t.Fatalf("alice's post = %d", status)
	}
	held := receive(t, h.ctx, runtime.calls)
	status, reply := postEvent(t, url, "Bearer "+bob, eventBody("Lights left on", "", "info", ""))
	if status != http.StatusAccepted {
		t.Fatalf("bob's post = %d", status)
	}
	if delivered := h.nextMessage(); delivered.chatID != 202 || !strings.Contains(delivered.text, "Lights left on") {
		t.Fatalf("bob's event = %+v", delivered)
	}
	h.waitEventOutcome(reply.ID, events.OutcomeDelivered)
	held.answer <- agentruntime.RunResult{Final: `{"notify":false,"message":""}`}
	if h.waitEventOutcome(h.eventIDOf("alice", "Backup done"), events.OutcomeSuppressed); len(h.messages) != 0 {
		t.Fatal("a suppressed event was sent")
	}
}

func (h *conversationHarness) eventIDOf(principalID, title string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM inbound_events WHERE principal_id = ? AND title = ?`, principalID, title).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func TestAnAssessmentPastItsLimitForwardsTheEvent(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h, url := newEventsHarness(t, runtime, harnessOptions{eventAssessTimeout: 200 * time.Millisecond})
	h.provision("alice", "acct-alice-1111")
	h.start()
	token, _ := h.addEventSource(1, 101, "Backups")
	h.assessSource(4, 101, "alice", "Backups")

	status, reply := postEvent(t, url, "Bearer "+token, eventBody("Backup done", "All fine.", "info", ""))
	if status != http.StatusAccepted {
		t.Fatalf("post = %d", status)
	}
	receive(t, h.ctx, runtime.calls)
	if delivered := h.nextMessage(); delivered.text != "ℹ️ <b>Backups</b>\n<b>Backup done</b>\n\nAll fine." {
		t.Fatalf("forwarded after a slow assessment = %+v", delivered)
	}
	h.waitEventOutcome(reply.ID, events.OutcomeDelivered)
	if kept := h.countRows(`SELECT count(*) FROM inbound_events WHERE id = ? AND assessment = 'failed'`, reply.ID); kept != 1 {
		t.Fatal("the timed-out assessment was not kept as failed")
	}
}

func TestPendingEventsAreRetriedSurviveRestartsAndAgeOut(t *testing.T) {
	t.Parallel()
	clock := newTestClock(t, mondayMorning)
	h, url := newEventsHarness(t, newControlledRuntime(), harnessOptions{clock: clock})
	h.start()
	token, _ := h.addEventSource(1, 101, "Backups")
	bearer := "Bearer " + token

	// A transient failure is retried on a later cycle.
	h.failNextSend()
	status, retried := postEvent(t, url, bearer, eventBody("Backup failed", "", "warning", ""))
	if status != http.StatusAccepted {
		t.Fatalf("post = %d", status)
	}
	if refused := h.nextMessage(); refused.status != http.StatusInternalServerError {
		t.Fatalf("first send = %+v", refused)
	}
	h.events.Wake()
	if sent := h.nextMessage(); sent.status != http.StatusOK || !strings.Contains(sent.text, "Backup failed") {
		t.Fatalf("retried send = %+v", sent)
	}
	h.waitEventOutcome(retried.ID, events.OutcomeDelivered)

	// One that cannot be sent for an hour is dropped.
	h.failNextSend()
	status, late := postEvent(t, url, bearer, eventBody("Backup late", "", "info", ""))
	if status != http.StatusAccepted {
		t.Fatalf("post = %d", status)
	}
	if refused := h.nextMessage(); refused.status != http.StatusInternalServerError {
		t.Fatalf("first send = %+v", refused)
	}
	clock.Set(t, "2026-09-28T09:01:00+02:00")
	h.events.Wake()
	h.waitEventOutcome(late.ID, events.OutcomeDropped)

	// One posted while the service is down is sent after it starts.
	h.stop()
	status, waiting := postEvent(t, url, bearer, eventBody("Backup during restart", "", "info", ""))
	if status != http.StatusAccepted {
		t.Fatalf("post while stopped = %d", status)
	}
	h.start()
	if sent := h.nextMessage(); !strings.Contains(sent.text, "Backup during restart") {
		t.Fatalf("send after restart = %+v", sent)
	}
	h.waitEventOutcome(waiting.ID, events.OutcomeDelivered)
	if len(h.messages) != 0 {
		t.Fatal("a dropped event was sent after the restart")
	}

	// Events are pruned 30 days after they arrived.
	clock.Set(t, "2026-10-28T09:02:00+01:00")
	h.events.Wake()
	for h.allInboundEvents() != 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			t.Fatalf("%d events outlived their 30 days", h.allInboundEvents())
		}
	}
}
