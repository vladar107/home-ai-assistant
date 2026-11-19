package assistantmail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// proposals stands in for the approval service: it keeps each proposal as the record a
// confirmation would execute.
type proposals struct {
	mu      sync.Mutex
	records []approval.Record
	intents []approval.Intent
}

func (p *proposals) Propose(_ context.Context, principalID string, proposal approval.Proposal) (approval.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record := approval.Record{
		ID: fmt.Sprintf("approval-%d", len(p.records)+1), PrincipalID: principalID, Tool: proposal.Tool,
		Arguments: proposal.Arguments, Summary: proposal.Summary,
	}
	p.records = append(p.records, record)
	p.intents = append(p.intents, proposal.Intent)
	return record, nil
}

func (p *proposals) lastIntent() approval.Intent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.intents[len(p.intents)-1]
}

func (p *proposals) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

func (p *proposals) last(t *testing.T) approval.Record {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.records) == 0 {
		t.Fatal("nothing was proposed")
	}
	return p.records[len(p.records)-1]
}

type outsideReads struct{ external atomic.Bool }

func (o *outsideReads) External(context.Context, string) bool { return o.external.Load() }

type sendHarness struct {
	*harness
	proposals *proposals
	outside   *outsideReads
}

func newSendHarness(t *testing.T, configure ...func(*assistantmail.Config)) *sendHarness {
	t.Helper()
	made, outside := &proposals{}, &outsideReads{}
	h := newHarness(t, append([]func(*assistantmail.Config){func(cfg *assistantmail.Config) {
		cfg.Proposer, cfg.Exposure = made, outside
		cfg.DisplayNames = map[string]string{"alice": "Alice Example", "bob": "Bob Example"}
	}}, configure...)...)
	h.start()
	return &sendHarness{harness: h, proposals: made, outside: outside}
}

// propose calls the send tool and returns the record it proposed.
func (h *sendHarness) propose(principal string, arguments map[string]any) approval.Record {
	h.t.Helper()
	result := h.execute(principal, "assistant_mail_send", arguments)
	if result.IsError || result.Text != approval.ProposedText {
		h.t.Fatalf("send = %+v", result)
	}
	return h.proposals.last(h.t)
}

// confirm executes the record as a confirmation does.
func (h *sendHarness) confirm(record approval.Record) (approval.Outcome, error) {
	return h.service.ExecuteApproved(h.ctx, record)
}

func (h *sendHarness) sendRequests() int {
	count := 0
	for _, request := range h.sim.Requests() {
		if request.Path == "/gmail/v1/users/me/messages/send" {
			count++
		}
	}
	return count
}

func actionReason(t *testing.T, err error) (string, string) {
	t.Helper()
	var failed *approval.ActionError
	if !errors.As(err, &failed) {
		t.Fatalf("execution error = %v, want an action error", err)
	}
	return failed.Reason, failed.Text
}

// sentMessage reads the raw message the simulator received back as headers and a decoded body.
func sentMessage(t *testing.T, raw []byte) (mail.Header, string) {
	t.Helper()
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatalf("the message has a bare line feed:\n%q", raw)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Fatalf("transfer encoding = %q", message.Header.Get("Content-Transfer-Encoding"))
	}
	body, err := io.ReadAll(quotedprintable.NewReader(message.Body))
	if err != nil {
		t.Fatal(err)
	}
	return message.Header, strings.ReplaceAll(string(body), "\r\n", "\n")
}

func TestSendIsOfferedOnlyWithConfirmationsAndNeverInAReadOnlySet(t *testing.T) {
	t.Parallel()
	h := newSendHarness(t)
	contribution, err := h.service.Contribute(h.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var send *tools.Definition
	for index, definition := range contribution.Definitions {
		if definition.Name == "assistant_mail_send" {
			send = &contribution.Definitions[index]
		}
	}
	if send == nil || send.Risk != tools.RiskWrite || send.ThirdParty || strings.Contains(string(send.InputSchema), "bcc") ||
		!strings.Contains(string(send.InputSchema), "at most about 2,000 characters") {
		t.Fatalf("send definition = %+v", send)
	}
	for _, want := range []string{`"Assistant"`, "assistant to Alice Example", "written in that same language", "AI assistant writing for Alice Example", "never claim it was sent", "Never claim or imply being human"} {
		if !strings.Contains(contribution.WriteGuidance, want) {
			t.Fatalf("write guidance = %q, want %q", contribution.WriteGuidance, want)
		}
	}
	if strings.Contains(contribution.WriteGuidance, "Assistant to") {
		t.Fatalf("write guidance still has a fixed English signature: %q", contribution.WriteGuidance)
	}

	broker, err := tools.NewBroker(h.cfg.Logger)
	if err != nil {
		t.Fatal(err)
	}
	broker.SetSwitches(h.switches)
	if err := broker.Register(h.service); err != nil {
		t.Fatal(err)
	}
	names := func(read bool) []string {
		t.Helper()
		set, err := broker.ToolsFor(h.ctx, "alice")
		if read {
			set, err = broker.ReadOnlyToolsFor(h.ctx, "alice")
		}
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, definition := range set.Definitions {
			names = append(names, definition.Name)
		}
		return names
	}
	if got := names(false); !slices.Contains(got, "assistant_mail_send") {
		t.Fatalf("chat tools = %v", got)
	}
	if got := names(true); slices.Contains(got, "assistant_mail_send") || !slices.Contains(got, "assistant_mail_read") {
		t.Fatalf("read-only tools = %v", got)
	}
	h.setSwitch("alice", false)
	if got := names(false); len(got) != 0 {
		t.Fatalf("tools with the switch off = %v", got)
	}

	// Without the approval service there is nothing to confirm a send with, so it is not offered.
	plain := newHarness(t)
	contribution, err = plain.service.Contribute(plain.ctx, "alice")
	if err != nil || len(contribution.Definitions) != 2 || contribution.WriteGuidance != "" {
		t.Fatalf("without confirmations = %+v %v", contribution, err)
	}
}

func TestSendRefusesUnsafeOrForeignInput(t *testing.T) {
	t.Parallel()
	h := newSendHarness(t)
	bobs := h.deliver("assistant+b1@example.com", googlesim.Message{Subject: "Bob's order"})
	spam := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Prize", Labels: []string{"SPAM"}})
	h.poll()

	valid := func(change func(map[string]any)) map[string]any {
		arguments := map[string]any{"to": []string{"orders@shop.example.org"}, "subject": "Order 1234", "body": "Hello."}
		change(arguments)
		return arguments
	}
	for name, test := range map[string]struct {
		arguments map[string]any
		want      string
	}{
		"line break in an address": {valid(func(a map[string]any) { a["to"] = []string{"orders@shop.example.org\r\nBcc: spy@example.net"} }), "line breaks"},
		"display name":             {valid(func(a map[string]any) { a["to"] = []string{"Shop <orders@shop.example.org>"} }), "not a plain address"},
		"two in one":               {valid(func(a map[string]any) { a["to"] = []string{"a@example.org, b@example.org"} }), "not a plain address"},
		"line break in subject":    {valid(func(a map[string]any) { a["subject"] = "Order\r\nBcc: spy@example.net" }), "one line"},
		"long subject":             {valid(func(a map[string]any) { a["subject"] = strings.Repeat("s", 201) }), "1 to 200"},
		"empty subject":            {valid(func(a map[string]any) { a["subject"] = " " }), "1 to 200"},
		"six recipients": {valid(func(a map[string]any) {
			a["to"] = []string{"a@example.org", "b@example.org", "c@example.org"}
			a["cc"] = []string{"d@example.org", "e@example.org", "f@example.org"}
		}), "at most 5"},
		"duplicate":           {valid(func(a map[string]any) { a["cc"] = []string{"Orders@Shop.example.org"} }), "twice"},
		"own address":         {valid(func(a map[string]any) { a["to"] = []string{"assistant@example.com"} }), "own address"},
		"own tagged address":  {valid(func(a map[string]any) { a["cc"] = []string{"Assistant+b1@example.com"} }), "own address"},
		"oversize body":       {valid(func(a map[string]any) { a["body"] = strings.Repeat("ü", 2001) }), "at most about 2,000 characters"},
		"control in body":     {valid(func(a map[string]any) { a["body"] = "Hello\x00" }), "control characters"},
		"bcc":                 {valid(func(a map[string]any) { a["bcc"] = []string{"spy@example.net"} }), "do not match the tool's schema"},
		"another's message":   {valid(func(a map[string]any) { a["reply_to_id"] = bobs }), "No message with that id"},
		"spam":                {valid(func(a map[string]any) { a["reply_to_id"] = spam }), "in spam"},
		"no recipient":        {valid(func(a map[string]any) { a["to"] = []string{} }), "at least one"},
		"unknown reply id":    {valid(func(a map[string]any) { a["reply_to_id"] = "../" + bobs }), "No message with that id"},
		"right-to-left trick": {valid(func(a map[string]any) { a["subject"] = "Invoice ‮gnp.exe" }), "one line"},
	} {
		result := h.execute("alice", "assistant_mail_send", test.arguments)
		if !result.IsError || !strings.Contains(result.Text, test.want) {
			t.Fatalf("%s = %+v, want an error with %q", name, result, test.want)
		}
	}
	if count := h.proposals.count(); count != 0 {
		t.Fatalf("%d refused sends were proposed", count)
	}

	// The same checks run again before a confirmed send, whatever the record holds.
	for _, arguments := range []string{
		`{"to":["assistant+a1@example.com"],"subject":"Hi","body":"Hello."}`,
		`{"to":["orders@shop.example.org"],"subject":"Hi\nBcc: spy@example.net","body":"Hello."}`,
		`{"to":["orders@shop.example.org"],"subject":"Hi","body":"Hello.","reply_to_id":"` + bobs + `"}`,
		`{"to":["orders@shop.example.org"],"bcc":["spy@example.net"],"subject":"Hi","body":"Hello."}`,
	} {
		_, err := h.confirm(approval.Record{ID: "forged", PrincipalID: "alice", Tool: "assistant_mail_send", Arguments: json.RawMessage(arguments)})
		if reason, _ := actionReason(t, err); reason != "invalid" {
			t.Fatalf("forged %s = %q", arguments, reason)
		}
	}
	if sends := h.sendRequests(); sends != 0 {
		t.Fatalf("%d refused sends reached Gmail", sends)
	}
}

func TestAConfirmedSendIsExactlyTheProposedMessage(t *testing.T) {
	t.Parallel()
	h := newSendHarness(t)
	h.outside.external.Store(true)
	body := "Hello,\n\nCould you resend the invoice for order 1234? Ünïcode and a long line " + strings.Repeat("x", 90) + "\n\n```not code```\n\nAssistant, Assistant to Alice Example"
	record := h.propose("alice", map[string]any{
		"to": []string{"<Orders@shop.example.org>"}, "cc": []string{"help@example.net"}, "subject": "Rechnung für Bestellung 1234", "body": body,
	})
	message := "From: Assistant <assistant@example.com>\nReply-To: assistant+a1@example.com\nTo: Orders@shop.example.org\nCc: help@example.net\n" +
		"Subject: Rechnung für Bestellung 1234\n\n" + body
	want := "Assistant mailbox · send email\n" +
		"⚠️ This conversation has read outside content, such as mail or web pages: check that the email says only what you want.\n" +
		"⚠️ First email to: Orders@shop.example.org, help@example.net.\n\n" +
		"````\n" + message + "\n````"
	if record.Summary != want {
		t.Fatalf("summary =\n%s\nwant\n%s", record.Summary, want)
	}
	if intent := h.proposals.lastIntent(); intent != (approval.Intent{Verb: "send", TargetKind: "email", TargetName: "Rechnung für Bestellung 1234"}) {
		t.Fatalf("intent = %+v", intent)
	}

	outcome, err := h.confirm(record)
	if err != nil || !strings.HasPrefix(outcome.Text, "Sent from the assistant mailbox (message id ") {
		t.Fatalf("outcome = %+v %v", outcome, err)
	}
	if outcome.UserText != "Sent from the assistant mailbox." {
		t.Fatalf("user text = %q, want no message id", outcome.UserText)
	}
	sent := h.sim.Sent()
	if len(sent) != 1 || sent[0].Account != mailbox {
		t.Fatalf("sent = %+v", sent)
	}
	header, sentBody := sentMessage(t, sent[0].Raw)
	subject, err := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
	if err != nil || subject != "Rechnung für Bestellung 1234" || header.Get("Subject") == subject {
		t.Fatalf("subject = %q (%q) %v", subject, header.Get("Subject"), err)
	}
	from, err := header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Assistant" || from[0].Address != mailbox {
		t.Fatalf("from = %+v %v", from, err)
	}
	for name, want := range map[string]string{
		"Reply-To": "assistant+a1@example.com", "To": "Orders@shop.example.org", "Cc": "help@example.net",
		"Mime-Version": "1.0", "Content-Type": "text/plain; charset=UTF-8", "Bcc": "", "In-Reply-To": "",
	} {
		if got := header.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := header.Date(); err != nil {
		t.Fatalf("date: %v", err)
	}
	if sentBody != body {
		t.Fatalf("body =\n%q\nwant\n%q", sentBody, body)
	}

	// The sent message is the principal's, and its recipients are known from now on.
	route, found, err := h.store.Route(h.ctx, "alice", sent[0].ID)
	if err != nil || !found || route.Direction != assistantmail.DirectionOut || route.ThreadID != sent[0].ThreadID {
		t.Fatalf("route = %+v %v %v", route, found, err)
	}
	h.outside.external.Store(false)
	again := h.propose("alice", map[string]any{"to": []string{"orders@shop.example.org"}, "subject": "Thanks", "body": "Thank you."})
	if strings.Contains(again.Summary, "⚠️") {
		t.Fatalf("a known recipient in a clean conversation is marked:\n%s", again.Summary)
	}
	if bobs := h.propose("bob", map[string]any{"to": []string{"orders@shop.example.org"}, "subject": "Hi", "body": "Hello."}); !strings.Contains(bobs.Summary, "First email to: orders@shop.example.org") {
		t.Fatalf("alice's contact counts for bob:\n%s", bobs.Summary)
	}
	h.poll()
	h.expectQuietLogs()
}

func TestTheDailyLimitIsPerPrincipalAndCountsUnknownOutcomes(t *testing.T) {
	t.Parallel()
	h := newSendHarness(t, func(cfg *assistantmail.Config) { cfg.Mailbox.DailySendLimit = 2 })
	propose := func(principal string) approval.Record {
		return h.propose(principal, map[string]any{"to": []string{"orders@shop.example.org"}, "subject": "Order", "body": "Hello."})
	}
	if _, err := h.confirm(propose("alice")); err != nil {
		t.Fatal(err)
	}

	// Gmail fails in a way that leaves the outcome unknown: reported as possibly sent, not tried
	// again, and counted.
	h.sim.FailSends(http.StatusServiceUnavailable)
	unknown := propose("alice")
	before := h.sendRequests()
	_, err := h.confirm(unknown)
	if reason, text := actionReason(t, err); reason != "possibly_sent" || !strings.Contains(text, "may have been sent") || !strings.Contains(text, "check the thread") {
		t.Fatalf("unknown outcome = %q %q", reason, text)
	}
	if sends := h.sendRequests() - before; sends != 1 {
		t.Fatalf("an unknown outcome made %d send requests", sends)
	}
	h.sim.FailSends(0)

	_, err = h.confirm(propose("alice"))
	if reason, text := actionReason(t, err); reason != "limit" || !strings.Contains(text, "limit of 2 emails a day") {
		t.Fatalf("over the limit = %q %q", reason, text)
	}
	if _, err := h.confirm(propose("bob")); err != nil {
		t.Fatalf("bob's send = %v", err)
	}
	if sent := h.sim.Sent(); len(sent) != 2 {
		t.Fatalf("sent %d messages", len(sent))
	}

	// A refusal by Gmail says nothing was sent.
	h.sim.FailSends(http.StatusBadRequest)
	_, err = h.confirm(propose("bob"))
	if reason, text := actionReason(t, err); reason != "refused" || !strings.Contains(text, "nothing was sent") {
		t.Fatalf("refused = %q %q", reason, text)
	}
	h.expectQuietLogs()
}

func TestAReplyJoinsTheThreadAndItsAnswerRoutesBack(t *testing.T) {
	t.Parallel()
	h := newSendHarness(t)
	inbound := h.deliver("assistant+a1@example.com", googlesim.Message{
		From: "Shop <orders@shop.example.org>", ReplyTo: "Help Desk <help@shop.example.org>", Cc: "billing@shop.example.org",
		Subject: "Your order", PlainText: "Is this an automated message?",
	})
	h.poll()

	record := h.propose("alice", map[string]any{
		"to": []string{"help@shop.example.org"}, "cc": []string{"friend@example.net"}, "subject": "About my order", "body": "Hello.", "reply_to_id": inbound,
	})
	for _, want := range []string{
		"Assistant mailbox · reply\n",
		"⚠️ Not in the thread being answered: friend@example.net.\n",
		"⚠️ The message answered asks for replies to help@shop.example.org, not to its sender orders@shop.example.org; help@shop.example.org is its Reply-To.\n",
		"\nSubject: Re: Your order\n",
	} {
		if !strings.Contains(record.Summary, want) {
			t.Fatalf("summary =\n%s\nwant %q", record.Summary, want)
		}
	}
	if strings.Contains(record.Summary, "outside content") {
		t.Fatalf("a clean conversation is marked:\n%s", record.Summary)
	}
	// The thread's subject was written by the shop, so the intent check never sees it.
	if intent := h.proposals.lastIntent(); intent != (approval.Intent{Verb: "reply to", TargetKind: "email"}) {
		t.Fatalf("reply intent = %+v", intent)
	}
	if fromSender := h.propose("alice", map[string]any{"to": []string{"billing@shop.example.org", "orders@shop.example.org"}, "subject": "Re: Your order", "body": "Hi.", "reply_to_id": inbound}); strings.Contains(fromSender.Summary, "Not in the thread") ||
		!strings.Contains(fromSender.Summary, "; orders@shop.example.org is its sender.") {
		t.Fatalf("summary for thread participants =\n%s", fromSender.Summary)
	}

	if _, err := h.confirm(record); err != nil {
		t.Fatal(err)
	}
	sent := h.sim.Sent()
	inboundThread, _, err := h.store.Route(h.ctx, "alice", inbound)
	if err != nil || len(sent) != 1 || sent[0].ThreadID != inboundThread.ThreadID {
		t.Fatalf("sent %+v, want it in thread %s: %v", sent, inboundThread.ThreadID, err)
	}
	header, _ := sentMessage(t, sent[0].Raw)
	inboundID := "<" + inbound + "@googlesim.example>"
	if header.Get("In-Reply-To") != inboundID || header.Get("References") != inboundID || header.Get("Subject") != "Re: Your order" {
		t.Fatalf("reply headers = %v", header)
	}

	// The shop answers to the plain address, which only the thread ties to alice.
	answer := h.deliver(mailbox, googlesim.Message{
		From: "Help Desk <help@shop.example.org>", Subject: "Re: Your order", InReplyTo: "<" + sent[0].ID + "@googlesim.example>",
	})
	h.poll()
	if routed := h.routes(answer); !slices.Equal(routed, []string{"alice"}) {
		t.Fatalf("the answer routed to %v", routed)
	}
	h.expectQuietLogs()
}

func TestABodyIsAsLongAsItsProposalCanShow(t *testing.T) {
	t.Parallel()
	h := newSendHarness(t)
	h.outside.external.Store(true)
	body := strings.Repeat("ü", 2000)
	record := h.propose("alice", map[string]any{"to": []string{"orders@shop.example.org"}, "subject": "Order 1234", "body": body})
	if !approval.SummaryFits(record.Summary) || !strings.Contains(record.Summary, body) {
		t.Fatalf("the longest body's proposal does not show it whole:\n%s", record.Summary)
	}
	if _, err := h.confirm(record); err != nil {
		t.Fatal(err)
	}

	// A long subject and many new recipients leave less room, so the same body no longer fits.
	long := map[string]any{
		"to": []string{
			"first.recipient.with.a.long.name@department.example.org", "second.recipient.with.a.long.name@department.example.org",
			"third.recipient.with.a.long.name@department.example.org",
		},
		"cc":      []string{"fourth.recipient.with.a.long.name@department.example.org", "fifth.recipient.with.a.long.name@department.example.org"},
		"subject": strings.Repeat("s", 200), "body": body,
	}
	result := h.execute("alice", "assistant_mail_send", long)
	if !result.IsError || !strings.Contains(result.Text, "too long to show") || !strings.Contains(result.Text, "at most about 2,000 characters") {
		t.Fatalf("send = %+v", result)
	}
	if count := h.proposals.count(); count != 1 {
		t.Fatalf("%d proposals, want only the one that fits", count)
	}
}
