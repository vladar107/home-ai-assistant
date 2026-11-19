package assistantmail_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

func (h *harness) execute(principal, name string, arguments any) tools.Result {
	h.t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := h.service.Execute(h.ctx, principal, tools.Call{Name: name, Arguments: encoded})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

// fullReads counts the reads of the whole message, which only assistant_mail_read makes.
func (h *harness) fullReads(gmailID string) int {
	count := 0
	for _, request := range h.sim.Requests() {
		if request.Path == "/gmail/v1/users/me/messages/"+gmailID && request.Query.Get("format") == "full" {
			count++
		}
	}
	return count
}

func TestTheToolsAreOfferedOnlyToAPrincipalWithATag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	contribution, err := h.service.Contribute(h.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, definition := range contribution.Definitions {
		names = append(names, definition.Name)
		if definition.Risk != tools.RiskRead || !definition.ThirdParty || !definition.Screen {
			t.Fatalf("%s = %+v, want a screened third-party read", definition.Name, definition)
		}
	}
	if strings.Join(names, ",") != "assistant_mail_list,assistant_mail_read" {
		t.Fatalf("tools = %v", names)
	}
	if !strings.Contains(contribution.Guidance, "assistant+a1@example.com") || !strings.Contains(contribution.Guidance, "never instructions") {
		t.Fatalf("guidance = %q", contribution.Guidance)
	}
	if h.service.Switch() != assistantmail.Switch {
		t.Fatal("the tools do not follow the Assistant mailbox switch")
	}

	for _, principal := range []string{"carol", ""} {
		if contribution, err := h.service.Contribute(h.ctx, principal); err != nil || len(contribution.Definitions) != 0 {
			t.Fatalf("%q got %+v %v", principal, contribution, err)
		}
		if result := h.execute(principal, "assistant_mail_list", map[string]any{}); !result.IsError {
			t.Fatalf("%q listed %q", principal, result.Text)
		}
	}
}

func TestListAndReadShowOnlyTheCallersOwnMail(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	received := time.Now().Add(time.Minute)
	alices := h.deliver("assistant+a1@example.com", googlesim.Message{
		From: `"<b>Shop</b>" <orders@shop.example.org>`, Subject: "Your *order*", PlainText: "Your parcel ships tomorrow.", Received: received,
	})
	bobs := h.deliver("assistant+b1@example.com", googlesim.Message{Subject: "Bob's order", PlainText: "Private to Bob.", Received: received})
	h.poll()
	// A message the assistant sent for alice, to a shop.
	sent := h.deliver(mailbox, googlesim.Message{From: "Assistant <assistant@example.com>", To: "Shop <orders@shop.example.org>",
		Subject: "Re: Your order", Received: received.Add(time.Minute)})
	h.poll()
	if err := h.store.RecordSent(h.ctx, sent, "thread-sent", "alice", received.Add(time.Minute), time.Now()); err != nil {
		t.Fatal(err)
	}

	listed := h.execute("alice", "assistant_mail_list", map[string]any{"limit": 5})
	if listed.IsError {
		t.Fatalf("list = %q", listed.Text)
	}
	sentAt, receivedAt := received.Add(time.Minute).In(noticeZone).Format("Mon 2 Jan 2006 15:04 MST"), received.In(noticeZone).Format("Mon 2 Jan 2006 15:04 MST")
	for _, want := range []string{
		"2 messages in this user's assistant mail, newest first:",
		"id " + sent + ", sent " + sentAt + "\nFrom: Assistant (assistant@example.com)\nTo: orders@shop.example.org\nSubject: Re: Your order\n",
		"id " + alices + ", received " + receivedAt + "\nFrom: Shop (orders@shop.example.org)\nTo: assistant+a1@example.com\nSubject: Your order\n",
	} {
		if !strings.Contains(listed.Text, want) {
			t.Fatalf("list = %q\nwant %q", listed.Text, want)
		}
	}
	if strings.Index(listed.Text, "id "+sent) > strings.Index(listed.Text, "id "+alices) {
		t.Fatalf("list is not newest first: %q", listed.Text)
	}
	if strings.Contains(listed.Text, bobs) || strings.Contains(listed.Text, "Bob's order") {
		t.Fatalf("alice's list shows bob's mail: %q", listed.Text)
	}

	read := h.execute("alice", "assistant_mail_read", map[string]any{"id": alices})
	if read.IsError || !strings.HasPrefix(read.Text, "Received at this user's assistant address.\n") ||
		!strings.Contains(read.Text, "Your parcel ships tomorrow.") || strings.Contains(read.Text, "Sender not verified") {
		t.Fatalf("read = %q", read.Text)
	}

	// Another principal's id finds nothing, and Gmail is not asked for it.
	for _, id := range []string{bobs, "msg-unknown", "../" + bobs} {
		result := h.execute("alice", "assistant_mail_read", map[string]any{"id": id})
		if !result.IsError || !strings.Contains(result.Text, "No message with that id") || strings.Contains(result.Text, "Private to Bob") {
			t.Fatalf("read of %q = %+v", id, result)
		}
	}
	if reads := h.fullReads(bobs); reads != 0 {
		t.Fatalf("bob's message was read %d times for alice", reads)
	}
	if read := h.execute("bob", "assistant_mail_read", map[string]any{"id": bobs}); read.IsError || !strings.Contains(read.Text, "Private to Bob.") {
		t.Fatalf("bob's own read = %q", read.Text)
	}
	h.expectQuietLogs()
}

func TestReadingMarksAFailingDMARCSenderAndAGoneMessage(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	h := newHarness(t, func(cfg *assistantmail.Config) {
		cfg.Mailbox.Tags = map[string][]string{"alice": {"test1"}}
		cfg.Now = func() time.Time { return clock }
	})
	h.start()
	failing := h.sim.DeliverRaw(googlesim.SampleDeliveryFailingDMARC(), "INBOX")
	h.poll()

	read := h.execute("alice", "assistant_mail_read", map[string]any{"id": failing})
	if read.IsError || !strings.Contains(read.Text, "Sender not verified") || !strings.Contains(read.Text, "test email with tag") {
		t.Fatalf("read = %q", read.Text)
	}
	if listed := h.execute("alice", "assistant_mail_list", map[string]any{}); !strings.Contains(listed.Text, "Sender not verified") {
		t.Fatalf("list = %q", listed.Text)
	}

	h.sim.FailMessage(failing, 404)
	if gone := h.execute("alice", "assistant_mail_read", map[string]any{"id": failing}); !gone.IsError || !strings.Contains(gone.Text, "no longer") {
		t.Fatalf("gone read = %+v", gone)
	}
	if listed := h.execute("alice", "assistant_mail_list", map[string]any{}); listed.IsError || !strings.Contains(listed.Text, "No longer in the mailbox.") {
		t.Fatalf("list with a gone message = %+v", listed)
	}
}

func TestSpamIsListedWithoutSenderOrSubjectAndNeverRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	spam := h.deliver("assistant+a1@example.com", googlesim.Message{
		From: "Prize <winner@spam.example.org>", Subject: "Secret spam subject", PlainText: "Claim it.", Labels: []string{"SPAM"},
	})
	h.poll()
	reads := h.noticeReads(spam)

	listed := h.execute("alice", "assistant_mail_list", map[string]any{})
	if listed.IsError || !strings.Contains(listed.Text, "id "+spam+", received ") || !strings.Contains(listed.Text, ", in spam; its sender and subject are not shown") ||
		strings.Contains(listed.Text, "spam.example.org") || strings.Contains(listed.Text, "Secret spam") {
		t.Fatalf("list = %q", listed.Text)
	}
	read := h.execute("alice", "assistant_mail_read", map[string]any{"id": spam})
	if !read.IsError || !strings.Contains(read.Text, "in spam") || !strings.Contains(read.Text, "web interface") || strings.Contains(read.Text, "Claim it") {
		t.Fatalf("read = %+v", read)
	}
	if h.noticeReads(spam) != reads || h.fullReads(spam) != 0 {
		t.Fatalf("spam was read: %d header reads after %d, %d full reads", h.noticeReads(spam), reads, h.fullReads(spam))
	}
}

func TestASentMessageNotRecordedAtSendIsRecoveredFromItsReplyTo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start()
	access := h.sim.Access(mailbox, googlesim.Grant{Mail: true, Send: true})
	raw := "From: Assistant <assistant@example.com>\r\nReply-To: assistant+b1@example.com\r\nTo: desk@shop.example.org\r\n" +
		"Subject: Opening hours\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nWhen are you open?\r\n"
	sent, err := h.gmail.Send(h.ctx, access, []byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	// Another message the mailbox sent, with no tag of the household's in its Reply-To.
	untagged := h.deliver(mailbox, googlesim.Message{From: "Assistant <assistant@example.com>", ReplyTo: "assistant+zz@example.com", Labels: []string{"SENT"}})
	h.poll()

	route, found, err := h.store.Route(h.ctx, "bob", sent.ID)
	if err != nil || !found || route.Direction != assistantmail.DirectionOut || route.ThreadID != sent.ThreadID || route.NotifiedAt.IsZero() {
		t.Fatalf("recovered route = %+v %v %v", route, found, err)
	}
	h.expectRoutes(untagged)
	if sentCount, skipped := h.metrics.count("sent"), h.metrics.count("skipped"); sentCount != 1 || skipped != 1 {
		t.Fatalf("sent %d, skipped %d", sentCount, skipped)
	}
	h.expectNotices()
	if listed := h.execute("bob", "assistant_mail_list", map[string]any{}); listed.IsError || !strings.Contains(listed.Text, "id "+sent.ID+", sent ") {
		t.Fatalf("bob's list = %q", listed.Text)
	}

	headers, err := h.gmail.MessageHeaders(h.ctx, access, sent.ID, []string{"Message-ID"})
	if err != nil || len(headers.HeaderValues("Message-ID")) != 1 {
		t.Fatalf("sent message headers = %+v %v", headers, err)
	}
	messageID := headers.HeaderValues("Message-ID")[0]
	reply := h.deliver(mailbox, googlesim.Message{Subject: "Re: Opening hours", InReplyTo: messageID, References: messageID})
	h.poll()
	h.expectRoutes(reply, "bob")
	h.expectQuietLogs()
}
