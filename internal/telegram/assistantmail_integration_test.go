package telegram

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const assistantMailbox = "assistant@example.com"

// newAssistantMailHarness runs the chat with the assistant's own mailbox, after its first poll.
func newAssistantMailHarness(t *testing.T) (*conversationHarness, *controlledRuntime, *googlesim.Simulator) {
	t.Helper()
	sim := googlesim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{assistantMail: sim, assistantMailToken: authorizeAssistantMail(t, sim)})
	h.provision("alice", "acct-alice-1111")
	h.provision("bob", "acct-bob-2222")
	h.start()
	h.pollAssistantMail()
	// The first poll's triage must end before a test polls again, or that poll skips the principal
	// whose triage is still running.
	if err := h.assembly.AssistantMail.WaitTriage(h.ctx); err != nil {
		t.Fatal(err)
	}
	return h, runtime, sim
}

// authorizeAssistantMail plays the operator's one-time authorization of the mailbox's own client
// and returns its refresh token.
func authorizeAssistantMail(t *testing.T, sim *googlesim.Simulator) string {
	t.Helper()
	const verifier = "test-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
	const redirect = "http://127.0.0.1:49152/callback"
	tokens, err := google.NewClient(google.ClientConfig{
		ClientID: googlesim.AssistantClientID, ClientSecret: googlesim.AssistantClientSecret, RedirectURL: redirect,
		TokenURL: sim.TokenURL(), HTTPClient: sim.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge := sha256.Sum256([]byte(verifier))
	link := sim.AuthURL() + "?" + url.Values{
		"client_id": {googlesim.AssistantClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {google.MailScope + " " + google.SendScope}, "access_type": {"offline"}, "state": {"state"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	back, err := url.Parse(sim.Consent(link, assistantMailbox, googlesim.Grant{Mail: true, Send: true}))
	if err != nil {
		t.Fatal(err)
	}
	granted, err := tokens.Exchange(t.Context(), back.Query().Get("code"), verifier)
	if err != nil || granted.Refresh == "" {
		t.Fatalf("exchange = %+v %v", granted, err)
	}
	return granted.Refresh
}

func (h *conversationHarness) pollAssistantMail() {
	h.t.Helper()
	if err := h.assembly.AssistantMail.Poll(h.ctx); err != nil {
		h.t.Fatalf("poll the assistant mailbox: %v", err)
	}
}

// triageAssistantMail answers the triage run a poll started for the message's thread, listing the
// message as needing the user, and returns the run.
func (h *conversationHarness) triageAssistantMail(runtime *controlledRuntime, id, why string) runtimeCall {
	h.t.Helper()
	call := receive(h.t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, "This is a check of new mail in one thread") || !strings.Contains(call.request.Message, id) {
		h.t.Fatalf("not the message's triage: %q", call.request.Message)
	}
	if read := executeTool(h.t, call.ctx, call.request.Tools, "assistant_mail_read", `{"id":"`+id+`"}`); read.IsError {
		h.t.Fatalf("triage read = %+v", read)
	}
	call.answer <- agentruntime.RunResult{Final: `{"needs_you":[{"id":"` + id + `","why":"` + why + `"}]}`}
	return call
}

// A triage run reads only its thread's mail; its notice reaches only its principal's chat as plain
// text, and the next turn hears of it as outside content.
func TestAssistantMailNoticesReachTheChatAsOutsideContent(t *testing.T) {
	t.Parallel()
	h, runtime, sim := newAssistantMailHarness(t)
	id := sim.DeliverTo("assistant+a1@example.com", googlesim.Message{
		From: `"Shop *Deals*" <orders@shop.example.org>`, Subject: "Your <b>order</b>", Received: time.Now().Add(time.Minute),
	})
	h.pollAssistantMail()
	triage := h.triageAssistantMail(runtime, id, "Confirm the <b>order</b>.")
	if names := toolNames(triage.request.Tools); !slices.Equal(names, []string{"assistant_mail_list", "assistant_mail_read"}) ||
		triage.request.WebSearch || triage.request.Memory != "" {
		t.Fatalf("triage run = tools %v, web search %v, memory %q", names, triage.request.WebSearch, triage.request.Memory)
	}
	notice := h.nextMessage()
	if notice.chatID != 101 || notice.parseMode != "" || notice.replyTo != 0 ||
		notice.text != "📬 1 email at your assistant address needs you:\n• Shop Deals (orders@shop.example.org) · Your order\n  Confirm the order.\n\nAsk me to read any of them." {
		t.Fatalf("notice = %+v", notice)
	}

	h.push(textUpdate(1, 101, "what came in?"))
	call := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, "You sent the user this notice of new mail") || !strings.Contains(call.request.Message, "orders@shop.example.org") {
		t.Fatalf("turn message = %q", call.request.Message)
	}
	if run := tools.RunFrom(call.ctx); run == nil || !run.External() {
		t.Fatal("the turn told of a notice is not marked as having read outside content")
	}
	h.finishTurn(call, 1, "A shop order.")
	if h.externalSessions() != 1 {
		t.Fatal("the conversation told of a notice is not marked as having read outside content")
	}
}

// The tools are offered to a principal with a tag and the switch on, and a read marks the
// conversation as having read outside content.
func TestAssistantMailToolsFollowTheTagAndTheSwitch(t *testing.T) {
	t.Parallel()
	h, runtime, sim := newAssistantMailHarness(t)
	id := sim.DeliverTo("assistant+a1@example.com", googlesim.Message{Subject: "Your order", PlainText: "Ships tomorrow.", Received: time.Now().Add(time.Minute)})
	h.pollAssistantMail()
	h.triageAssistantMail(runtime, id, "Check the order.")
	h.nextMessage()

	// The first turn hears of the notice; a new conversation starts clean.
	h.push(textUpdate(1, 101, "anything new?"))
	h.finishTurn(receive(t, h.ctx, runtime.calls), 1, "One order.")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, localetest.English(NewSessionReply))

	h.push(textUpdate(3, 101, "read my assistant mail"))
	call := receive(t, h.ctx, runtime.calls)
	names := toolNames(call.request.Tools)
	if !slices.Contains(names, "assistant_mail_list") || !slices.Contains(names, "assistant_mail_read") ||
		!strings.Contains(call.request.Tools.Guidance, "assistant+a1@example.com") {
		t.Fatalf("tools = %v, guidance %q", names, call.request.Tools.Guidance)
	}
	if run := tools.RunFrom(call.ctx); run == nil || run.External() {
		t.Fatal("the new conversation starts as having read outside content")
	}
	result := executeTool(t, call.ctx, call.request.Tools, "assistant_mail_read", `{"id":"`+id+`"}`)
	if result.IsError || !strings.Contains(result.Text, "Ships tomorrow.") {
		t.Fatalf("read = %+v", result)
	}
	h.finishTurn(call, 3, "It ships tomorrow.")
	if h.externalSessions() != 2 {
		t.Fatal("the conversation that read the mail is not marked as having read outside content")
	}

	// bob has no tag, and alice's switch turned off takes the tools away.
	h.push(textUpdate(4, 202, "any mail?"))
	call = receive(t, h.ctx, runtime.calls)
	if names := toolNames(call.request.Tools); slices.ContainsFunc(names, isAssistantMailTool) {
		t.Fatalf("bob's tools = %v", names)
	}
	call.answer <- agentruntime.RunResult{Response: "No."}
	h.wantReply(202, 4, "No.")

	if err := h.switches.SetEnabled(h.ctx, "alice", assistantmail.Switch, false); err != nil {
		t.Fatal(err)
	}
	h.push(textUpdate(5, 101, "and now?"))
	call = receive(t, h.ctx, runtime.calls)
	if names := toolNames(call.request.Tools); slices.ContainsFunc(names, isAssistantMailTool) {
		t.Fatalf("tools with the switch off = %v", names)
	}
	h.finishTurn(call, 5, "Off.")
}

func isAssistantMailTool(name string) bool { return strings.HasPrefix(name, "assistant_mail_") }

// A reply drafted after reading a stranger's mail is shown whole with its marks, sent exactly as
// shown once confirmed, and a rejected one sends nothing.
func TestAssistantMailSendIsSentOnlyAsConfirmed(t *testing.T) {
	t.Parallel()
	h, runtime, sim := newAssistantMailHarness(t)
	inbound := sim.DeliverTo("assistant+a1@example.com", googlesim.Message{
		From: "Shop <orders@shop.example.org>", Subject: "Your order", PlainText: "Is this an automated message?", Received: time.Now().Add(time.Minute),
	})
	h.pollAssistantMail()
	h.triageAssistantMail(runtime, inbound, "Answer the shop's question.")
	h.nextMessage()

	h.push(textUpdate(1, 101, "answer the shop"))
	call := receive(t, h.ctx, runtime.calls)
	if !slices.Contains(toolNames(call.request.Tools), "assistant_mail_send") || !strings.Contains(call.request.Tools.Guidance, "say plainly that you are an AI assistant") {
		t.Fatalf("tools = %v, guidance %q", toolNames(call.request.Tools), call.request.Tools.Guidance)
	}
	if read := executeTool(t, call.ctx, call.request.Tools, "assistant_mail_read", `{"id":"`+inbound+`"}`); read.IsError {
		t.Fatalf("read = %+v", read)
	}
	body := "Hello,\n\nYes: I am an AI assistant writing for the user, who confirms every email. **Not bold** & <b>not markup</b>.\n\nAssistant, Assistant to the user"
	arguments, err := json.Marshal(map[string]any{"to": []string{"orders@shop.example.org"}, "subject": "Re: Your order", "body": body, "reply_to_id": inbound})
	if err != nil {
		t.Fatal(err)
	}
	if result := executeTool(t, call.ctx, call.request.Tools, "assistant_mail_send", string(arguments)); result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("send = %+v", result)
	}
	if sent := sim.Sent(); len(sent) != 0 {
		t.Fatalf("a proposal sent %d messages", len(sent))
	}
	h.finishTurn(call, 1, "I drafted a reply.")
	proposal := h.nextMessage()
	escaped := "From: Assistant &lt;assistant@example.com&gt;\nReply-To: assistant+a1@example.com\nTo: orders@shop.example.org\nSubject: Re: Your order\n\n" +
		"Hello,\n\nYes: I am an AI assistant writing for the user, who confirms every email. **Not bold** &amp; &lt;b&gt;not markup&lt;/b&gt;.\n\nAssistant, Assistant to the user"
	for _, want := range []string{"Assistant mailbox · reply", "⚠️ This conversation has read outside content", "<pre><code>" + escaped + "</code></pre>"} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal = %q\nwant %q", proposal.text, want)
		}
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Sent from the assistant mailbox") {
		t.Fatalf("outcome = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	sent := sim.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	message, err := netmail.ReadMessage(bytes.NewReader(sent[0].Raw))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(message.Body))
	if err != nil || strings.ReplaceAll(string(decoded), "\r\n", "\n") != body || message.Header.Get("Subject") != "Re: Your order" ||
		message.Header.Get("To") != "orders@shop.example.org" || message.Header.Get("Reply-To") != "assistant+a1@example.com" ||
		message.Header.Get("In-Reply-To") != "<"+inbound+"@googlesim.example>" {
		t.Fatalf("sent %v\n%q %v", message.Header, decoded, err)
	}

	// A rejected proposal sends nothing.
	h.push(textUpdate(3, 101, "write to the shop again"))
	call = receive(t, h.ctx, runtime.calls)
	if result := executeTool(t, call.ctx, call.request.Tools, "assistant_mail_send", `{"to":["orders@shop.example.org"],"subject":"Another","body":"Hello again."}`); result.IsError {
		t.Fatalf("send = %+v", result)
	}
	h.finishTurn(call, 3, "Drafted.")
	proposal = h.nextMessage()
	_, reject := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(4, 101, proposal.messageID, reject))
	h.wantAnswer(4)
	h.nextEdit()
	if sent := sim.Sent(); len(sent) != 1 {
		t.Fatalf("a rejected proposal sent: %d messages", len(sent))
	}
	if logs := h.logs.String(); strings.Contains(logs, "shop.example.org") || strings.Contains(logs, "Your order") || strings.Contains(logs, "AI assistant writing") {
		t.Fatalf("logs carry mail content: %s", logs)
	}
}
