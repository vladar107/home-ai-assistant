package telegram

import (
	"crypto/sha256"
	"encoding/base64"
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

// A notice reaches only its principal's chat as plain text, and the next turn hears of it as
// outside content.
func TestAssistantMailNoticesReachTheChatAsOutsideContent(t *testing.T) {
	t.Parallel()
	h, runtime, sim := newAssistantMailHarness(t)
	sim.DeliverTo("assistant+a1@example.com", googlesim.Message{
		From: `"Shop *Deals*" <orders@shop.example.org>`, Subject: "Your <b>order</b>", Received: time.Now().Add(time.Minute),
	})
	h.pollAssistantMail()
	notice := h.nextMessage()
	if notice.chatID != 101 || notice.parseMode != "" || notice.replyTo != 0 ||
		!strings.HasPrefix(notice.text, "📬 1 new email at your assistant address:\n• Shop Deals (orders@shop.example.org) · Your order · ") {
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
