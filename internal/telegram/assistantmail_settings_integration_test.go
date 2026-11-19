package telegram

import (
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

// A principal with a tag sees their own address on the mailbox, can switch it and its routine
// replies off and on, and is told when the mailbox needs the operator; a principal without a tag
// sees nothing of it.
func TestAssistantMailboxSettings(t *testing.T) {
	t.Parallel()
	h, _, sim := newAssistantMailHarness(t)

	overview := h.requestSettings(1, 101)
	if !strings.Contains(overview.text, "Accounts: Assistant mailbox on") {
		t.Fatalf("overview = %q", overview.text)
	}
	page := h.openSettingsPage(2, 101, settingsMessageID(1), "accounts")
	for _, want := range []string{localetest.English(AssistantMailOnLine), "<code>assistant+a1@example.com</code>", "reaches only you"} {
		if !strings.Contains(page.text, want) {
			t.Fatalf("page lacks %q: %s", want, page.text)
		}
	}
	if data := buttonData(page.keyboard); !slices.Contains(data, "mailbox:off") || !slices.Contains(data, "mailbox:replies_on") ||
		!strings.Contains(page.text, localetest.English(AssistantMailRepliesOffLine)) {
		t.Fatalf("page = %+v", page)
	}

	replies := h.press(3, 101, settingsMessageID(1), "mailbox:replies_on")
	if !strings.Contains(replies.text, localetest.English(AssistantMailRepliesOnLine)) || !slices.Contains(buttonData(replies.keyboard), "mailbox:replies_off") {
		t.Fatalf("after routine replies on = %+v", replies)
	}
	if enabled, err := h.assembly.Switches.Enabled(h.ctx, "alice", assistantmail.RoutineSwitch); err != nil || !enabled {
		t.Fatalf("routine replies = %v %v", enabled, err)
	}
	if enabled, err := h.assembly.Switches.Enabled(h.ctx, "alice", assistantmail.Switch); err != nil || !enabled {
		t.Fatalf("the mailbox = %v %v", enabled, err)
	}

	off := h.press(4, 101, settingsMessageID(1), "mailbox:off")
	if !strings.Contains(off.text, localetest.English(AssistantMailOffLine)) || !slices.Contains(buttonData(off.keyboard), "mailbox:on") ||
		strings.Contains(off.text, "Routine replies") || slices.Contains(buttonData(off.keyboard), "mailbox:replies_off") {
		t.Fatalf("after off = %+v", off)
	}
	on := h.press(5, 101, settingsMessageID(1), "mailbox:on")
	if !strings.Contains(on.text, localetest.English(AssistantMailOnLine)) || !strings.Contains(on.text, localetest.English(AssistantMailRepliesOnLine)) {
		t.Fatalf("after on = %+v", on)
	}

	h.push(textUpdate(6, 101, "/privacy"))
	if privacy := h.nextMessage().text; !strings.Contains(privacy, "each new message at your address goes to your model, which decides whether it needs you; the sender and subject of mail I tell you about go to your model with your next message, and nothing is sent without your confirmation") ||
		!strings.Contains(privacy, localetest.English(AssistantMailRepliesPrivacyLine)) {
		t.Fatalf("/privacy = %q", privacy)
	}
	repliesOff := h.press(7, 101, settingsMessageID(1), "mailbox:replies_off")
	if !strings.Contains(repliesOff.text, localetest.English(AssistantMailRepliesOffLine)) {
		t.Fatalf("after routine replies off = %+v", repliesOff)
	}

	sim.RevokeGrant(assistantMailbox)
	if err := h.assembly.AssistantMail.Poll(h.ctx); err == nil {
		t.Fatal("poll with a revoked grant succeeded")
	}
	unavailable := h.press(8, 101, settingsMessageID(1), "mailbox:on")
	if !strings.Contains(unavailable.text, "authorizes it again") || !slices.Contains(buttonData(unavailable.keyboard), "mailbox:off") {
		t.Fatalf("unavailable page = %+v", unavailable)
	}

	bob := h.requestSettings(9, 202)
	if strings.Contains(bob.text, "Assistant mailbox") || slices.Contains(buttonData(bob.keyboard), "nav:accounts") {
		t.Fatalf("bob's settings = %+v", bob)
	}
	h.push(textUpdate(10, 202, "/privacy"))
	if privacy := h.nextMessage().text; strings.Contains(privacy, "Assistant mailbox") {
		t.Fatalf("bob's /privacy = %q", privacy)
	}
}

// A Russian user reads the page and the privacy line in Russian.
func TestAssistantMailboxSettingsReadInRussian(t *testing.T) {
	t.Parallel()
	sim := googlesim.New(t)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{assistantMail: sim, assistantMailToken: authorizeAssistantMail(t, sim), languages: true})
	h.provision("alice", "acct-alice-1111")
	h.start()
	h.pollAssistantMail()
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.push(textUpdate(1, 101, "/settings"))
	h.nextMessage()
	h.push(callbackUpdate(2, 101, settingsMessageID(1), settingsNavPrefix+":accounts"))
	h.wantAnswer(2)
	page := h.nextEdit()
	if !strings.Contains(page.text, "Почта ассистента: включена") || !strings.Contains(page.text, "<code>assistant+a1@example.com</code>") {
		t.Fatalf("page = %q", page.text)
	}
	h.push(textUpdate(3, 101, "/privacy"))
	if privacy := h.nextMessage().text; !strings.Contains(privacy, "каждое новое письмо на твой адрес уходит твоей модели, которая решает, требует ли оно твоего внимания") {
		t.Fatalf("/privacy = %q", privacy)
	}
}
