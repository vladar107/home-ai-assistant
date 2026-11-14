package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
)

// A user who chose Russian gets a proposal widget with Russian buttons and a Russian status line
// once it is rejected, confirmed or expired by the sweep; the templates carry both languages' labels.
func TestRussianUserGetsRussianProposalWidgets(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{languages: true, approvalSweep: 100 * time.Millisecond})
	h.approvals.SetLanguages(h.store)
	if err := h.store.SaveLanguage(h.ctx, "alice", locale.Russian); err != nil {
		t.Fatal(err)
	}

	_, single := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note A", "Saved.", false))
	data := decodeWidget(t, single.data)
	if data.Lang != "ru" || len(data.Actions) != 2 || data.Actions[0].Label != "Подтвердить" || data.Actions[1].Label != "Отклонить" || data.Status != "Истекает через 10 минут." {
		t.Fatalf("russian widget = %+v", data)
	}
	h.push(widgetTap(1, 101, single.messageID, data.Actions[1].Data))
	h.wantWidgetAnswer(1)
	caption, outcome := h.wantOutcome(single.messageID, "Отклонено, ничего не записано.")
	if outcome.Lang != "ru" || !strings.HasSuffix(caption.caption, "— отклонено") {
		t.Fatalf("rejected = %q %+v", caption.caption, outcome)
	}

	_, second := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note B", "Saved.", false))
	confirm := decodeWidget(t, second.data).Actions[0].Data
	h.push(widgetTap(2, 101, second.messageID, confirm))
	h.wantWidgetAnswer(2)
	h.wantOutcome(second.messageID, "Готово. Saved.")

	_, third := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note C", "Saved.", false))
	h.expirePendingApprovals(third.messageID)
	h.nextWidgetEdit("editMessageCaption", third.messageID)
	expired := decodeWidget(t, h.nextWidgetEdit("editMessageWidgetData", third.messageID).data)
	if expired.Status != "Истекло, ничего не записано." || len(localetest.Latin(expired.Status)) != 0 {
		t.Fatalf("expired by the sweep = %+v", expired)
	}

	_, changes := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note D", "Saved.", false), outcomeProposal(t, "Save the note E", "Saved.", false))
	list := decodeChanges(t, changes.data)
	if list.Lang != "ru" || len(changes.data) > widgetDataLimit || !strings.Contains(changes.caption, "2 изменения") {
		t.Fatalf("russian changes widget = %q %+v", changes.caption, list)
	}
	for _, kind := range []widgetKind{proposalWidget, changesWidget} {
		if template := widgetTemplates[kind]; !strings.Contains(template, `"en":{`) || !strings.Contains(template, `"ru":{`) {
			t.Fatalf("the %s template lacks a language's labels", kind)
		}
	}
}

// A user with no language keeps the data of today: it names no language, so the widget draws from
// the template's English labels.
func TestEnglishWidgetDataNamesNoLanguage(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{languages: true})
	h.approvals.SetLanguages(h.store)
	_, single := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note A", "Saved.", false))
	if strings.Contains(single.data, "lang") || decodeWidget(t, single.data).Actions[0].Label != "Confirm" {
		t.Fatalf("english widget data = %s", single.data)
	}
	_, changes := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note B", "Saved.", false), outcomeProposal(t, "Save the note C", "Saved.", false))
	if strings.Contains(changes.data, `"g":`) || strings.Contains(changes.data, "labels") || changes.caption != "Weekly review: 2 changes" {
		t.Fatalf("english changes data = %s", changes.data)
	}
}
