package gmail

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

const assistant = "assistant@example.com"

// recorder keeps every attempt that reached the transport, with its body.
type recorder struct {
	base     http.RoundTripper
	mu       sync.Mutex
	attempts []string
	bodies   [][]byte
}

func (r *recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	r.mu.Lock()
	r.attempts = append(r.attempts, request.Method+" "+request.URL.Path)
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	return r.base.RoundTrip(request)
}

func (r *recorder) count(attempt string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, made := range r.attempts {
		if made == attempt {
			n++
		}
	}
	return n
}

// mailboxClient is the client as production builds it, retrying transport included, in front of
// the recorder.
func mailboxClient(t *testing.T, base string, transport http.RoundTripper) (*Client, *recorder) {
	t.Helper()
	httpretry.WithoutWaits(t)
	rec := &recorder{base: transport}
	client, err := NewClient(ClientConfig{APIBaseURL: base, HTTPClient: &http.Client{Transport: httpretry.New(rec, httpretry.Options{Service: "gmail"})}})
	if err != nil {
		t.Fatal(err)
	}
	return client, rec
}

const sendPath = "POST /gmail/v1/users/me/messages/send"

func TestSendSendsTheRawMessageAndKeepsTheThread(t *testing.T) {
	sim := googlesim.New(t)
	client, rec := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true, Send: true})
	question := []byte("From: Assistant <assistant@example.com>\r\nTo: shop@example.org\r\nReply-To: assistant+abc@example.com\r\n" +
		"Subject: Order question\r\nMessage-ID: <client-1@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Is the order on its way?\r\n")

	first, err := client.Send(t.Context(), access, question, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ThreadID == "" || !slices.Equal(first.LabelIDs, []string{"SENT"}) {
		t.Fatalf("sent = %+v", first)
	}
	if strings.Contains(string(rec.bodies[0]), "threadId") {
		t.Fatalf("a new message named a thread: %s", rec.bodies[0])
	}

	headers, err := client.MessageHeaders(t.Context(), access, first.ID, InboundHeaders)
	if err != nil {
		t.Fatal(err)
	}
	messageID := headers.HeaderValues("Message-ID")
	if len(messageID) != 1 || messageID[0] == "<client-1@example.com>" || !slices.Equal(headers.HeaderValues("Reply-To"), []string{"assistant+abc@example.com"}) {
		t.Fatalf("headers = %+v", headers.Payload.Headers)
	}

	answer := sim.DeliverTo("assistant+abc@example.com", googlesim.Message{
		From: "Shop <shop@example.org>", Subject: "Re: Order question", InReplyTo: messageID[0], References: messageID[0],
	})
	followUp := []byte("From: Assistant <assistant@example.com>\r\nTo: shop@example.org\r\nSubject: Re: Order question\r\n" +
		"In-Reply-To: <reply@example.org>\r\nReferences: " + messageID[0] + " <reply@example.org>\r\n\r\nThank you.\r\n")
	second, err := client.Send(t.Context(), access, followUp, first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.ThreadID != first.ThreadID || !strings.Contains(string(rec.bodies[len(rec.bodies)-1]), `"threadId":"`+first.ThreadID+`"`) {
		t.Fatalf("follow-up = %+v after %+v", second, first)
	}
	reply, err := client.MessageMetadata(t.Context(), access, answer)
	if err != nil || reply.ThreadID != first.ThreadID {
		t.Fatalf("the reply landed in thread %q, not %q: %v", reply.ThreadID, first.ThreadID, err)
	}

	sent := sim.Sent()
	if len(sent) != 2 || !bytes.Equal(sent[0].Raw, question) || !bytes.Equal(sent[1].Raw, followUp) ||
		sent[0].Account != assistant || sent[1].ThreadID != first.ThreadID || sent[1].ID != second.ID {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestSendStartsANewThreadUnlessTheReplyMatchesIt(t *testing.T) {
	sim := googlesim.New(t)
	client, _ := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true, Send: true})
	first, err := client.Send(t.Context(), access, []byte("To: shop@example.org\r\nSubject: Order question\r\n\r\nWhere is it?\r\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	headers, err := client.MessageHeaders(t.Context(), access, first.ID, InboundHeaders)
	if err != nil {
		t.Fatal(err)
	}
	messageID := headers.HeaderValues("Message-ID")[0]

	for name, raw := range map[string]string{
		"no In-Reply-To":    "To: shop@example.org\r\nSubject: Re: Order question\r\n\r\nAny news?\r\n",
		"another subject":   "To: shop@example.org\r\nSubject: Re: Invoice\r\nIn-Reply-To: " + messageID + "\r\n\r\nAny news?\r\n",
		"unrelated replies": "To: shop@example.org\r\nSubject: Re: Order question\r\nIn-Reply-To: <other@example.org>\r\n\r\nAny news?\r\n",
	} {
		sent, err := client.Send(t.Context(), access, []byte(raw), first.ThreadID)
		if err != nil || sent.ThreadID == first.ThreadID {
			t.Fatalf("%s: sent %+v into thread %q: %v", name, sent, first.ThreadID, err)
		}
	}
	joined, err := client.Send(t.Context(), access, []byte("To: shop@example.org\r\nSubject: RE: Fwd: Order question\r\nIn-Reply-To: "+messageID+"\r\n\r\nAny news?\r\n"), first.ThreadID)
	if err != nil || joined.ThreadID != first.ThreadID {
		t.Fatalf("a matching reply landed in %+v: %v", joined, err)
	}

	before := len(sim.Sent())
	if _, err := client.Send(t.Context(), access, []byte("To: shop@example.org\r\nSubject: Hello\r\n\r\nHello.\r\n"), "unknown-thread"); errorClass(err) != google.ErrorNotFound {
		t.Fatalf("send to an unknown thread: %v", err)
	}
	if _, err := client.Send(t.Context(), access, []byte("Subject: Hello\r\n\r\nHello.\r\n"), ""); errorClass(err) != google.ErrorInvalid {
		t.Fatalf("send without recipients: %v", err)
	}
	if len(sim.Sent()) != before {
		t.Fatal("a refused send was recorded")
	}
}

func TestSendIsNeverRepeated(t *testing.T) {
	sim := googlesim.New(t)
	client, rec := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true, Send: true})
	id := sim.DeliverRaw(googlesim.SampleDelivery())

	sim.FailSends(http.StatusServiceUnavailable)
	if _, err := client.Send(t.Context(), access, []byte("To: shop@example.org\r\nSubject: Hello\r\n\r\nHello.\r\n"), ""); errorClass(err) != google.ErrorUnavailable {
		t.Fatalf("send error = %v", err)
	}
	if got := rec.count(sendPath); got != 1 || len(sim.Sent()) != 0 {
		t.Fatalf("a send failing with 503 was attempted %d times", got)
	}

	sim.FailMessage(id, http.StatusServiceUnavailable)
	if _, err := client.MessageMetadata(t.Context(), access, id); err == nil {
		t.Fatal("a failing read succeeded")
	}
	if got := rec.count("GET /gmail/v1/users/me/messages/" + id); got != httpretry.MaximumAttempts {
		t.Fatalf("a read failing with 503 was attempted %d times; the transport retries reads", got)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unreachable, rec := mailboxClient(t, closed.URL, http.DefaultTransport)
	if _, err := unreachable.Send(t.Context(), access, []byte("To: shop@example.org\r\nSubject: Hello\r\n\r\nHello.\r\n"), ""); errorClass(err) != google.ErrorUnavailable {
		t.Fatalf("send error = %v", err)
	}
	if _, err := unreachable.Profile(t.Context(), access); err == nil {
		t.Fatal("an unreachable profile read succeeded")
	}
	if send, profile := rec.count(sendPath), rec.count("GET /gmail/v1/users/me/profile"); send != 1 || profile != httpretry.MaximumAttempts {
		t.Fatalf("a refused connection was attempted %d times for a send and %d for a read", send, profile)
	}
}

func TestSendNeedsTheSendScope(t *testing.T) {
	sim := googlesim.New(t)
	client, _ := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true})

	if _, err := client.Send(t.Context(), access, []byte("To: shop@example.org\r\nSubject: Hello\r\n\r\nHello.\r\n"), ""); errorClass(err) != google.ErrorForbidden {
		t.Fatalf("send error = %v", err)
	}
	if len(sim.Sent()) != 0 {
		t.Fatal("a token without gmail.send sent mail")
	}
}

func TestHistoryPagesTheMessagesAddedAfterTheStartID(t *testing.T) {
	sim := googlesim.New(t)
	client, _ := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true, Send: true})
	sim.DeliverTo("assistant+abc@example.com", googlesim.Message{From: "Shop <shop@example.org>", Subject: "Before the start"})

	start, err := client.Profile(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	if start.EmailAddress != assistant || start.HistoryID == "" {
		t.Fatalf("profile = %+v", start)
	}
	sim.SetHistoryPageSize(2)
	added := []string{
		sim.DeliverTo("assistant+abc@example.com", googlesim.Message{From: "Shop <shop@example.org>", Subject: "One"}),
		sim.DeliverTo("assistant+def@example.com", googlesim.Message{From: "Shop <shop@example.org>", Subject: "Two"}),
		sim.DeliverTo("assistant+abc@example.com", googlesim.Message{From: "Prize <prize@example.net>", Subject: "Three", Labels: []string{"SPAM"}}),
	}

	first, err := client.History(t.Context(), access, start.HistoryID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Added) != 2 || first.Added[0].ID != added[0] || first.Added[1].ID != added[1] ||
		!slices.Equal(first.Added[0].LabelIDs, []string{"INBOX"}) || first.NextPageToken == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.History(t.Context(), access, start.HistoryID, first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Added) != 1 || second.Added[0].ID != added[2] || !slices.Equal(second.Added[0].LabelIDs, []string{"SPAM"}) || second.NextPageToken != "" {
		t.Fatalf("second page = %+v", second)
	}

	now, err := client.Profile(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	if second.HistoryID != now.HistoryID || now.HistoryID == start.HistoryID {
		t.Fatalf("history id %q, profile %q, start %q", second.HistoryID, now.HistoryID, start.HistoryID)
	}
	if caughtUp, err := client.History(t.Context(), access, now.HistoryID, ""); err != nil || len(caughtUp.Added) != 0 || caughtUp.NextPageToken != "" {
		t.Fatalf("caught up = %+v, %v", caughtUp, err)
	}

	sim.RepeatInHistory(assistant, added[0])
	if repeated, err := client.History(t.Context(), access, now.HistoryID, ""); err != nil || len(repeated.Added) != 1 || repeated.Added[0].ID != added[0] {
		t.Fatalf("repeated = %+v, %v", repeated, err)
	}
}

func TestHistoryFromAnExpiredStartIDFailsDistinctly(t *testing.T) {
	sim := googlesim.New(t)
	client, _ := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true, Send: true})
	sim.DeliverTo(assistant, googlesim.Message{From: "Shop <shop@example.org>", Subject: "Old"})
	old, err := client.Profile(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	sim.DeliverTo(assistant, googlesim.Message{From: "Shop <shop@example.org>", Subject: "New"})
	current, err := client.Profile(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	sim.ExpireHistoryBefore(assistant, current.HistoryID)

	_, err = client.History(t.Context(), access, old.HistoryID, "")
	if !errors.Is(err, ErrHistoryExpired) || errorClass(err) != google.ErrorNotFound {
		t.Fatalf("history error = %v", err)
	}
	if page, err := client.History(t.Context(), access, current.HistoryID, ""); err != nil || len(page.Added) != 0 {
		t.Fatalf("history from the current id = %+v, %v", page, err)
	}

	sim.ExpireAccessTokens()
	if _, err := client.History(t.Context(), access, current.HistoryID, ""); errors.Is(err, ErrHistoryExpired) || errorClass(err) != google.ErrorUnauthorized {
		t.Fatalf("an expired token reads as %v", err)
	}
}

func TestMessageHeadersKeepThePlusTag(t *testing.T) {
	sim := googlesim.New(t)
	client, _ := mailboxClient(t, sim.URL(), sim.Client().Transport)
	access := sim.Access(assistant, googlesim.Grant{Mail: true, Send: true})
	delivered := sim.DeliverRaw(googlesim.SampleDelivery())

	message, err := client.MessageHeaders(t.Context(), access, delivered, InboundHeaders)
	if err != nil {
		t.Fatal(err)
	}
	tagged := []string{"assistant+test1@example.com"}
	if !slices.Equal(message.HeaderValues("Delivered-To"), tagged) || !slices.Equal(message.HeaderValues("To"), tagged) {
		t.Fatalf("the tag was lost: %+v", message.Payload.Headers)
	}
	results := message.HeaderValues("Authentication-Results")
	if len(results) != 1 || !strings.Contains(results[0], "dmarc=pass") ||
		!slices.Equal(message.HeaderValues("Message-ID"), []string{"<test-1@example.org>"}) ||
		!slices.Equal(message.HeaderValues("From"), []string{"Sam Sender <sender@example.org>"}) {
		t.Fatalf("headers = %+v", message.Payload.Headers)
	}
	if len(message.HeaderValues("Received")) != 0 || len(message.HeaderValues("Mime-Version")) != 0 {
		t.Fatalf("headers that were not asked for were read: %+v", message.Payload.Headers)
	}
	if want := time.Date(2026, 11, 19, 11, 46, 42, 0, time.UTC).UnixMilli(); message.InternalDate != strconv.FormatInt(want, 10) {
		t.Fatalf("internal date %s, want %d", message.InternalDate, want)
	}
	if !slices.Equal(message.LabelIDs, []string{"INBOX"}) {
		t.Fatalf("labels = %v", message.LabelIDs)
	}

	tools, err := client.MessageMetadata(t.Context(), access, delivered)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.HeaderValues("Delivered-To")) != 0 || len(tools.HeaderValues("Authentication-Results")) != 0 {
		t.Fatalf("the Gmail tools' read changed: %+v", tools.Payload.Headers)
	}

	failing, err := client.MessageHeaders(t.Context(), access, sim.DeliverRaw(googlesim.SampleDeliveryFailingDMARC()), InboundHeaders)
	if err != nil {
		t.Fatal(err)
	}
	if results := failing.HeaderValues("Authentication-Results"); len(results) != 1 || !strings.Contains(results[0], "dmarc=fail") {
		t.Fatalf("authentication results = %v", results)
	}

	several := sim.DeliverTo("assistant+abc@example.com", googlesim.Message{
		From: "Shop <shop@example.org>", To: "assistant+abc@example.com, assistant+def@example.com", Cc: "assistant+ghi@example.com",
		Subject: "Your order",
	})
	message, err = client.MessageHeaders(t.Context(), access, several, InboundHeaders)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(message.HeaderValues("Delivered-To"), []string{"assistant+abc@example.com"}) ||
		!slices.Equal(message.HeaderValues("To"), []string{"assistant+abc@example.com, assistant+def@example.com"}) ||
		!slices.Equal(message.HeaderValues("Cc"), []string{"assistant+ghi@example.com"}) ||
		len(message.HeaderValues("Authentication-Results")) != 1 || len(message.HeaderValues("Message-ID")) != 1 {
		t.Fatalf("headers = %+v", message.Payload.Headers)
	}
}
