package gmail

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
)

func TestSearchMailListsTheNewestMatchesOldestFirstAcrossMailboxes(t *testing.T) {
	h := newHarness(t)
	h.connect(work, googlesim.Grant{Mail: true})
	h.connect(home, googlesim.Grant{Mail: true})
	h.sim.AddMailbox(work, invoices("w", 3)...)
	h.sim.AddMailbox(home, invoices("h", 2)...)

	refs, err := h.service.SearchMail(t.Context(), "principal-1", "invoice", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 4 || refs[0].Received.After(refs[3].Received) || refs[0].Account == "" || refs[0].ID == "" || refs[3].ID != "wc" {
		t.Fatalf("refs = %+v", refs)
	}
	if _, err := h.service.SearchMail(t.Context(), "principal-1", "  ", 4); err == nil {
		t.Fatal("an empty search was accepted")
	}

	h.sim.FailMessage("ha", http.StatusForbidden)
	if _, err := h.service.SearchMail(t.Context(), "principal-1", "invoice", 4); err == nil {
		t.Fatal("a mailbox that could not be read was skipped silently")
	}
}

func TestReadMailReturnsTheSenderSubjectDateAndTextWithoutLinks(t *testing.T) {
	h := newHarness(t)
	h.connect(work, googlesim.Grant{Mail: true})
	received := time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)
	h.sim.AddMailbox(work, googlesim.Message{
		ID: "m1", ThreadID: "m1", From: "Example Post <Ship@Example-Shop.test>", Subject: "Shipped", Received: received,
		HTML: `<p>Your parcel is on its way. <a href="https://tracking.example.test/c/1">Track it</a></p><p>Visit https://plain.example.test/x for more.</p>`,
	})

	mail, err := h.service.ReadMail(t.Context(), "principal-1", MailRef{Account: work, ID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if mail.From != "ship@example-shop.test" || mail.Subject != "Shipped" || !mail.Received.Equal(received) || mail.Account != work ||
		!strings.Contains(mail.Text, "Your parcel is on its way.") || strings.Contains(mail.Text, "example.test") {
		t.Fatalf("mail = %+v", mail)
	}
	if _, err := h.service.ReadMail(t.Context(), "principal-1", MailRef{Account: work, ID: "gone"}); !errors.Is(err, ErrMessageGone) {
		t.Fatalf("a deleted message gave %v", err)
	}
	if _, err := h.service.ReadMail(t.Context(), "principal-1", MailRef{Account: "other@example.test", ID: "m1"}); !errors.Is(err, google.ErrNotConnected) {
		t.Fatalf("another mailbox gave %v", err)
	}
}
