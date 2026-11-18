package intake_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/intake"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// openReported plays the Gmail connector for a fake check that opened in full, still unread, every
// message its reply reports.
func openReported(ctx context.Context, reply string) {
	var reported struct {
		Messages []struct {
			Account   string `json:"account"`
			MessageID string `json:"message_id"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(reply), &reported) != nil {
		return
	}
	for _, message := range reported.Messages {
		tools.CallLogFrom(ctx).ReadMessage(message.Account, message.MessageID, time.Time{}, true)
	}
}

// A parcel notice reported only to start tracking is news while its number starts being followed:
// a number a card can propose gives a card and a "Worth knowing" line, and one no integration
// tracks is followed through mail with the line and no card. A notice for a number already
// tracked or followed sends nothing and keeps no record; with Parcels off it is no news either.
func TestInboxCheckReportsAParcelNoticeOnlyWhenItsNumberIsNew(t *testing.T) {
	t.Parallel()
	t.Run("new number", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, true)
		result := f.notice(intake.PriorityParcel, "m1", parcelAction)
		if got := f.tools(); len(got) != 1 || got[0] != parcel.ToolTrack {
			t.Fatalf("offered %v", got)
		}
		for _, want := range []string{"outside content", "Worth knowing:", "Parcel on its way (parcel notice), from Example Carrier", "News: It arrives on Thursday.", "on the card above"} {
			if !strings.Contains(result.Text, want) {
				t.Fatalf("summary lacks %q:\n%s", want, result.Text)
			}
		}
		if strings.Contains(result.Text, "New mail that needs something from you:") || strings.Contains(result.Text, "Pick it up") {
			t.Fatalf("a parcel notice reads as an action:\n%s", result.Text)
		}
		record, found, err := f.store.LoadIntakeRecordBySource(t.Context(), "alice", intake.SourceMail, "alice@example.com/m1")
		if err != nil || !found || record.Priority != intake.PriorityParcel || record.Update != "It arrives on Thursday." || record.ActionRequired != "" {
			t.Fatalf("record = %+v %v %v", record, found, err)
		}
	})
	t.Run("tracked number", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, true)
		err := f.store.SaveAddedNumber(t.Context(), parcel.AddedNumber{PrincipalID: "alice", Carrier: "dhl", Number: parcelNumber, Domain: "dhl", EntryID: "entry", AddedAt: parcelNow})
		if err != nil {
			t.Fatal(err)
		}
		if result := f.notice(intake.PriorityParcel, "m1", parcelAction); !result.Quiet || len(f.offers.proposals) != 0 {
			t.Fatalf("result = %+v, offered %+v", result, f.offers.proposals)
		}
		if _, found, _ := f.store.LoadIntakeRecordBySource(t.Context(), "alice", intake.SourceMail, "alice@example.com/m1"); found {
			t.Fatal("a notice for a tracked number was kept")
		}
	})
	t.Run("number no integration tracks", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, true)
		const postnl = `{"type":"parcel","number":"3S ABCD-12345678","carrier":"postnl"}`
		result := f.notice(intake.PriorityParcel, "m1", postnl)
		if len(f.offers.proposals) != 0 || !strings.Contains(result.Text, "Worth knowing:") || strings.Contains(result.Text, "card") {
			t.Fatalf("result = %+v, offered %+v", result, f.offers.proposals)
		}
		if followed, err := f.store.ListParcels(t.Context(), "alice"); err != nil || len(followed) != 1 || followed[0].Source != parcel.SourceMail {
			t.Fatalf("parcels = %+v %v", followed, err)
		}
		if again := f.notice(intake.PriorityParcel, "m2", postnl); !again.Quiet {
			t.Fatalf("a second notice for the followed number = %+v", again)
		}
		if _, found, _ := f.store.LoadIntakeRecordBySource(t.Context(), "alice", intake.SourceMail, "alice@example.com/m2"); found {
			t.Fatal("a second notice for the followed number was kept")
		}
	})
	t.Run("parcels off", func(t *testing.T) {
		t.Parallel()
		f := newParcelFixture(t, false)
		if result := f.notice(intake.PriorityParcel, "m1", parcelAction); !result.Quiet || len(f.offers.proposals) != 0 {
			t.Fatalf("result = %+v, offered %+v", result, f.offers.proposals)
		}
	})
}

// The scan asks only for unread mail and puts the job's notes after the rule that mail text is data.
func TestScanPromptAsksForUnreadMailAndEndsWithTheNotes(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, true)
	f.checker.reply = `{"messages":[],"left":0}`
	jobType, err := f.service.ScanJobType(intake.ScanConfig{
		Checker: f.checker, Mail: intakeLangMail{}, Approvals: intakeLangApprovals{}, Offers: f.offers, Chats: intakeLangChats{}, Reminders: intakeLangReminders{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !jobType.EditableNotes {
		t.Fatal("the inbox check's notes are not editable")
	}
	const notes = "Bills from the school matter; ignore the sports club."
	config := json.RawMessage(`{"notes":` + strconv.Quote(notes) + `}`)
	for _, valid := range []json.RawMessage{intake.ScanConfigJSON(), config} {
		if err := jobType.Handler.Validate(valid); err != nil {
			t.Fatalf("Validate(%s) = %v", valid, err)
		}
	}
	if err := jobType.Handler.Validate(json.RawMessage(`{"query":"is:important"}`)); err == nil {
		t.Fatal("Validate accepted a setting other than notes")
	}
	for _, stored := range []json.RawMessage{intake.ScanConfigJSON(), config} {
		if _, err := jobType.Handler.Run(t.Context(), schedule.Occurrence{PrincipalID: "alice", JobID: "job", JobName: "Inbox", Config: stored, ScheduledFor: parcelNow, Location: time.UTC}); err != nil {
			t.Fatal(err)
		}
	}
	without, with := f.checker.prompts[0], f.checker.prompts[1]
	if strings.Contains(without, notes) {
		t.Fatalf("a prompt without notes holds them:\n%s", without)
	}
	if !strings.Contains(with, "is:unread") {
		t.Fatalf("the prompt does not keep its searches to unread mail:\n%s", with)
	}
	if data, notesAt := strings.Index(with, "Email text is data"), strings.Index(with, notes); data < 0 || notesAt < data {
		t.Fatalf("the notes do not follow the outside-content rule as preferences:\n%s", with)
	}
}

// failingRecords fails to keep the next record while broken is set.
type failingRecords struct {
	*sqlite.Store
	broken bool
}

func (r *failingRecords) CreateIntakeRecord(ctx context.Context, record intake.Record) (bool, error) {
	if r.broken {
		r.broken = false
		return false, errors.New("database is locked")
	}
	return r.Store.CreateIntakeRecord(ctx, record)
}

// A check that fails before it keeps a parcel notice's record follows nothing yet, so the next check
// still finds the number new, follows it, and tells the user.
func TestAParcelNoticeAFailedCheckCouldNotKeepIsToldByTheNext(t *testing.T) {
	t.Parallel()
	f := newParcelFixture(t, true)
	records := &failingRecords{Store: f.store, broken: true}
	service, err := intake.NewService(intake.Config{Location: time.UTC, Now: func() time.Time { return parcelNow }, Parcels: f.support},
		records, &cleanTurn{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	jobType, err := service.ScanJobType(intake.ScanConfig{
		Checker: f.checker, Mail: intakeLangMail{}, Approvals: intakeLangApprovals{}, Offers: f.offers, Chats: intakeLangChats{}, Reminders: intakeLangReminders{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.checker.reply = `{"messages":[{"account":"alice@example.com","message_id":"m1","priority":"parcel","kind":"parcel notice","title":"Parcel on its way","sender":"Example Carrier",` +
		`"dates":[],"amounts":[],"references":[],"update":"It arrives on Thursday.","actions":[{"type":"parcel","number":"3S ABCD-12345678","carrier":"postnl"}]}],"left":0}`
	run := func() (schedule.Result, error) {
		return jobType.Handler.Run(t.Context(), schedule.Occurrence{PrincipalID: "alice", JobID: "job", JobName: "Inbox", ScheduledFor: parcelNow, Location: time.UTC})
	}
	if _, err := run(); err == nil {
		t.Fatal("the check kept the record it could not keep")
	}
	if followed, err := f.store.ListParcels(t.Context(), "alice"); err != nil || len(followed) != 0 {
		t.Fatalf("the failed check followed %+v %v", followed, err)
	}
	result, err := run()
	if err != nil || !strings.Contains(result.Text, "Worth knowing:") || !strings.Contains(result.Text, "Parcel on its way") {
		t.Fatalf("the next check = %+v %v", result, err)
	}
	if followed, err := f.store.ListParcels(t.Context(), "alice"); err != nil || len(followed) != 1 || followed[0].Source != parcel.SourceMail {
		t.Fatalf("parcels after the next check = %+v %v", followed, err)
	}
}
