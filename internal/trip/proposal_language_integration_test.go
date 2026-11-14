package trip_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

var proposalLanguageAllowed = []string{"Example", "City", "Flight", "to", "Hotel", "Rental", "Train", "trip", "ABC", "Europe", "Lisbon"}

func (f *toolsFixture) proposalLanguageCall(language locale.Language, name, arguments string) tools.Result {
	f.t.Helper()
	result, err := f.tools.Execute(locale.With(f.chat, language), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *toolsFixture) proposalLanguagePending(tool string) approval.Record {
	f.t.Helper()
	pending, err := f.store.PendingApprovals(f.t.Context(), "alice", 0)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, record := range pending {
		if record.Tool == tool {
			return record
		}
	}
	f.t.Fatalf("no pending %s in %+v", tool, pending)
	return approval.Record{}
}

// A Russian user reads the trip proposal, what it did once confirmed, and the trip card's summary in
// Russian with Russian dates, while the model's result and the English text stay as they were.
func TestRussianUserReadsTripProposalOutcomeAndSummaryInRussian(t *testing.T) {
	t.Parallel()
	f := newToolsFixture(t)
	f.exposure.external.Store(true)
	russian := locale.With(t.Context(), locale.Russian)

	if result := f.proposalLanguageCall(locale.Russian, "trip_add", addCall); result.IsError || result.Text != approval.ProposedText {
		t.Fatalf("the model's result = %+v, want the English proposal text", result)
	}
	proposal := f.proposalLanguagePending("trip_add")
	for _, want := range []string{"Добавить поездку:", "с пн 2 ноя 2026 09:00 по пт 6 ноя 2026 (Europe/Lisbon)", "- перелёт: Flight to Example City (ABC123), начало пн 2 ноя 2026 09:00, регистрация вс 1 ноя 2026 09:00 (указано в бронировании)", "- отель: Example Hotel"} {
		if !strings.Contains(proposal.Summary, want) {
			t.Errorf("proposal lacks %q:\n%s", want, proposal.Summary)
		}
	}
	if latin := localetest.Latin(proposal.Summary, proposalLanguageAllowed...); len(latin) != 0 {
		t.Errorf("proposal has Latin words %v:\n%s", latin, proposal.Summary)
	}

	outcome, err := f.tools.ExecuteApproved(russian, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(outcome.Text, "Saved the trip [") || !strings.Contains(outcome.Text, "Reminders set: Flight to Example City at 2026-11-01 09:00 (stated in the booking)") {
		t.Fatalf("the model's outcome = %q, want English", outcome.Text)
	}
	for _, want := range []string{"Поездка Example trip, Example City сохранена: с пн 2 ноя 2026 09:00 по пт 6 ноя 2026 (Europe/Lisbon).",
		"Напоминания: Flight to Example City, вс 1 ноя 2026 09:00 (указано в бронировании)", "Без напоминания: Example Rental (регистрация уже открыта).",
		"Время регистрации неизвестно, напоминания нет: Example Train."} {
		if !strings.Contains(outcome.UserText, want) {
			t.Errorf("user outcome lacks %q:\n%s", want, outcome.UserText)
		}
	}
	if latin := localetest.Latin(outcome.UserText, proposalLanguageAllowed...); len(latin) != 0 {
		t.Errorf("user outcome has Latin words %v:\n%s", latin, outcome.UserText)
	}

	trips, err := f.trips.Trips(t.Context(), "alice")
	if err != nil || len(trips) != 1 {
		t.Fatalf("trips = %+v, %v", trips, err)
	}
	card := trip.SummaryIn(locale.Russian, trips[0], f.clock.Now())
	for _, want := range []string{"Example trip (Example City)", "с пн 2 ноя 2026 по пт 6 ноя 2026, время: Europe/Lisbon",
		"- Flight to Example City, пн 2 ноя 09:00 (ABC123). Регистрация откроется вс 1 ноя 09:00.", "- Example Train, ср 4 ноя 10:00. Время регистрации неизвестно."} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}
	if latin := localetest.Latin(card, proposalLanguageAllowed...); len(latin) != 0 {
		t.Errorf("card has Latin words %v:\n%s", latin, card)
	}
	if english := trip.SummaryIn(locale.English, trips[0], f.clock.Now()); english != trip.Summary(trips[0], f.clock.Now()) ||
		!strings.Contains(english, "Mon 2 Nov 2026 to Fri 6 Nov 2026, times in Europe/Lisbon") || !strings.Contains(english, "Check-in opens Sun 1 Nov 09:00.") {
		t.Errorf("english card changed:\n%s", english)
	}

	if result := f.proposalLanguageCall(locale.Russian, "trip_update", `{"id":"`+trips[0].ID+`","cancel":true}`); result.IsError || result.Text != approval.ProposedText {
		t.Fatalf("cancel = %+v", result)
	}
	cancel := f.proposalLanguagePending("trip_update")
	if want := `Отменить поездку "Example trip" и убрать напоминания о регистрации.`; cancel.Summary != want {
		t.Fatalf("cancel proposal = %q, want %q", cancel.Summary, want)
	}
	done, err := f.tools.ExecuteApproved(russian, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if done.Text != `Cancelled the trip "Example trip" and removed its check-in reminders.` || done.UserText != `Поездка "Example trip" отменена, напоминания о регистрации убраны.` {
		t.Fatalf("cancel outcome = %+v", done)
	}
	cancelled, _, _ := f.trips.Trip(t.Context(), "alice", trips[0].ID)
	if card := trip.SummaryIn(locale.Russian, cancelled, f.clock.Now()); !strings.HasPrefix(card, "Отменена.\n") {
		t.Errorf("cancelled card = %q", card)
	}
}

// An English user's proposal and outcome carry the same English as before, and no separate user text.
func TestEnglishUserTripProposalAndOutcomeStayEnglish(t *testing.T) {
	t.Parallel()
	f := newToolsFixture(t)
	f.exposure.external.Store(true)
	f.proposalLanguageCall(locale.English, "trip_add", addCall)
	proposal := f.proposalLanguagePending("trip_add")
	if !strings.HasPrefix(proposal.Summary, "Add a trip:\nExample trip, Example City: 2026-11-02 09:00 to 2026-11-06 (Europe/Lisbon)\n- flight: Flight to Example City (ABC123), starts 2026-11-02 09:00, check-in 2026-11-01 09:00 (stated in the booking)") {
		t.Fatalf("english proposal:\n%s", proposal.Summary)
	}
	outcome, err := f.tools.ExecuteApproved(t.Context(), proposal)
	if err != nil || outcome.UserText != "" {
		t.Fatalf("english outcome = %+v, %v", outcome, err)
	}
}
