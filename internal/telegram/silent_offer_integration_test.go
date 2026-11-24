package telegram

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// offerSilently offers proposals to Alice under a heading without a notification, as the parcel mail
// job does, and returns the records and the error of the first delivery.
func (h *conversationHarness) offerSilently(proposals ...approval.Proposal) ([]approval.Record, error) {
	h.t.Helper()
	return h.approvals.OfferSilentlyIfIdle(h.ctx, "alice", 101, 0, "Parcels from your mail", 10*time.Minute, proposals)
}

// longProposal is a proposal whose summary fills more than half of a bundle's message, so two of
// them go out as two messages when they are text.
func longProposal(t *testing.T, name string) approval.Proposal {
	t.Helper()
	return outcomeProposal(t, "Track the parcel "+name+"\n"+strings.Repeat("A line of the mail. ", 70), "Tracked.", false)
}

// A bundle offered silently is sent without a notification, whatever it is drawn as: one change as
// the proposal widget or as text, several as the changes widget or as text over two messages.
func TestSilentOfferIsSentWithoutANotification(t *testing.T) {
	t.Parallel()
	t.Run("one change as a widget", func(t *testing.T) {
		t.Parallel()
		h := newOfferHarness(t, harnessOptions{})
		if _, err := h.offerSilently(outcomeProposal(t, "Track the parcel A", "Tracked.", false)); err != nil {
			t.Fatal(err)
		}
		if widget := h.nextWidget(); !widget.silent || !h.widgetKindIs(widget.messageID, "proposal") {
			t.Fatalf("proposal widget = %+v, want it silent", widget)
		}
	})
	t.Run("one change as text", func(t *testing.T) {
		t.Parallel()
		h := newOfferHarness(t, harnessOptions{})
		h.setWidgets("alice", false)
		if _, err := h.offerSilently(outcomeProposal(t, "Track the parcel A", "Tracked.", false)); err != nil {
			t.Fatal(err)
		}
		if message := h.nextMessage(); !message.silent || len(buttonData(message.keyboard)) != 2 {
			t.Fatalf("proposal message = %+v, want it silent with its buttons", message)
		}
	})
	t.Run("several changes as a widget", func(t *testing.T) {
		t.Parallel()
		h := newOfferHarness(t, harnessOptions{})
		if _, err := h.offerSilently(outcomeProposal(t, "Track the parcel A", "Tracked.", false), outcomeProposal(t, "Track the parcel B", "Tracked.", false)); err != nil {
			t.Fatal(err)
		}
		if widget := h.nextWidget(); !widget.silent || !h.widgetKindIs(widget.messageID, "changes") {
			t.Fatalf("changes widget = %+v, want it silent", widget)
		}
	})
	t.Run("several changes as text", func(t *testing.T) {
		t.Parallel()
		h := newOfferHarness(t, harnessOptions{})
		h.setWidgets("alice", false)
		if _, err := h.offerSilently(longProposal(t, "A"), longProposal(t, "B")); err != nil {
			t.Fatal(err)
		}
		first, last := h.nextMessage(), h.nextMessage()
		if !first.silent || len(first.keyboard) != 0 || !strings.Contains(first.text, "Track the parcel A") {
			t.Fatalf("first message = %+v, want it silent without buttons", first)
		}
		if !last.silent || len(buttonData(last.keyboard)) != 2 || !strings.Contains(last.text, "Track the parcel B") {
			t.Fatalf("last message = %+v, want it silent with the buttons", last)
		}
	})
}

// The sweep's resend of a silent bundle whose delivery failed is silent too, as a widget and as text
// over two messages.
func TestSilentOfferResentBySweepIsSilent(t *testing.T) {
	t.Parallel()
	t.Run("widget", func(t *testing.T) {
		t.Parallel()
		h := newOfferHarness(t, harnessOptions{approvalSweep: 50 * time.Millisecond})
		h.mu.Lock()
		h.widgets.sendStatus = http.StatusInternalServerError
		h.mu.Unlock()
		records, err := h.offerSilently(outcomeProposal(t, "Track the parcel A", "Tracked.", false))
		if err == nil {
			t.Fatal("an offer whose widget failed was delivered")
		}
		if h.countRows(`SELECT count(*) FROM approvals WHERE id = ? AND message_id IS NULL AND silent = 1`, records[0].ID) != 1 {
			t.Fatal("the failed offer is not stored as silent and undelivered")
		}
		h.mu.Lock()
		h.widgets.sendStatus = 0
		h.mu.Unlock()
		if widget := h.nextWidget(); !widget.silent {
			t.Fatalf("resent widget = %+v, want it silent", widget)
		}
	})
	t.Run("text", func(t *testing.T) {
		t.Parallel()
		h := newOfferHarness(t, harnessOptions{approvalSweep: 50 * time.Millisecond})
		h.setWidgets("alice", false)
		h.mu.Lock()
		h.sendStatuses = []int{http.StatusInternalServerError}
		h.mu.Unlock()
		if _, err := h.offerSilently(longProposal(t, "A"), longProposal(t, "B")); err == nil {
			t.Fatal("an offer whose first message failed was delivered")
		}
		if failed := h.nextMessage(); failed.status != http.StatusInternalServerError || !failed.silent {
			t.Fatalf("first attempt = %+v, want a failed silent send", failed)
		}
		first, last := h.nextMessage(), h.nextMessage()
		if first.status != http.StatusOK || !first.silent || len(first.keyboard) != 0 || !strings.Contains(first.text, "Track the parcel A") {
			t.Fatalf("resent first message = %+v, want it silent", first)
		}
		if last.status != http.StatusOK || !last.silent || len(buttonData(last.keyboard)) != 2 {
			t.Fatalf("resent last message = %+v, want it silent with the buttons", last)
		}
	})
}

// A chat turn's proposal and a bundle offered without the flag still notify, as a widget and as
// text over two messages.
func TestProposalsNotOfferedSilentlyNotify(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	if widget, _ := h.proposeChanges(outcomeProposal(t, "Save the note A", "Saved.", false)); widget.silent {
		t.Fatalf("a chat turn's proposal widget = %+v, want it to notify", widget)
	}
	if _, widget := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note B", "Saved.", false)); widget.silent {
		t.Fatalf("an offered proposal widget = %+v, want it to notify", widget)
	}

	h.setWidgets("alice", false)
	if _, err := h.approvals.ProposeAll(h.ctx, "alice", []approval.Proposal{longProposal(t, "C"), longProposal(t, "D")}); err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		t.Fatal(err)
	}
	first, last := h.nextMessage(), h.nextMessage()
	if first.silent || last.silent || len(buttonData(last.keyboard)) != 2 {
		t.Fatalf("a chat turn's proposal as text = %+v, %+v, want both to notify", first, last)
	}
}
