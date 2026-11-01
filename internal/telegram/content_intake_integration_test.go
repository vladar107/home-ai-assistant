package telegram

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/intake"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

var intakeRecordPattern = regexp.MustCompile(`intake record ([0-9a-f]{16})`)

const (
	sampleInvoice = `{"source":"file","source_ref":"%s","kind":"invoice","title":"Water bill for the third quarter","sender":"Example Water",` +
		`"dates":[{"label":"payment due","date":"2026-10-15"}],"amounts":[{"label":"total","value":"42.10","currency":"EUR"}],` +
		`"references":[{"label":"invoice number","value":"W-2026-3"}],"action_required":"Pay 42.10 EUR by 15 October."}`
	noMail = `{"messages":[],"left":0,"through":""}`
)

// forwardInvoice sends alice's forwarded sample invoice as a PDF and has the model keep its facts
// and propose a reminder and filing it, as the intake guidance asks. It returns the card.
func forwardInvoice(t *testing.T, h *conversationHarness, runtime *controlledRuntime, updateID int64) sentMessage {
	t.Helper()
	content := []byte("%PDF-1.4 sample invoice")
	h.addFile("invoice-1", "documents/file_1.pdf", content)
	update := documentUpdate(updateID, 101, "", MediaFile{FileID: "invoice-1", FileName: "water-bill.pdf", MimeType: "application/pdf", FileSize: int64(len(content))})
	update.Message.ForwardOrigin = &MessageOrigin{Type: "user"}
	h.push(update)
	call := receive(t, h.ctx, runtime.calls)
	attachmentID := attachmentIDPattern.FindStringSubmatch(call.request.Message)
	if attachmentID == nil || !slices.Contains(toolNames(call.request.Tools), "intake_record") {
		t.Fatalf("turn = %q with tools %v", call.request.Message, toolNames(call.request.Tools))
	}
	if !strings.Contains(call.request.Tools.Guidance, "Content intake: when the user sends a photo, a file, or a forwarded message") {
		t.Fatal("the turn lacks the intake guidance")
	}
	kept := executeTool(t, call.ctx, call.request.Tools, "intake_record", fmt.Sprintf(sampleInvoice, attachmentID[1]))
	if kept.IsError || intakeRecordPattern.FindStringSubmatch(kept.Text) == nil {
		t.Fatalf("intake_record = %+v", kept)
	}
	for name, arguments := range map[string]string{
		"reminder_set":        `{"text":"Pay the water bill","at":"2026-10-14 09:00"}`,
		"nas_save_attachment": `{"attachment_id":"` + attachmentID[1] + `"}`,
	} {
		if result := executeTool(t, call.ctx, call.request.Tools, name, arguments); result.IsError || !strings.Contains(result.Text, approval.ProposedText) {
			t.Fatalf("%s = %+v, want it proposed", name, result)
		}
	}
	h.finishTurn(call, updateID, "This is a water bill from Example Water: 42.10 EUR, due 15 October. I kept it as a record.")
	card := h.nextMessage()
	if !strings.HasPrefix(card.text, "2 changes, confirmed or rejected together:") || !strings.Contains(card.text, "Pay the water bill") || !strings.Contains(card.text, "water-bill.pdf") {
		t.Fatalf("card = %q", card.text)
	}
	if h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatal("a follow-up ran before the card was answered")
	}
	return card
}

func newChatIntakeHarness(t *testing.T) (*conversationHarness, *controlledRuntime, string) {
	t.Helper()
	runtime := newControlledRuntime()
	roots := nasRootsFor(t, true)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{nasRoots: roots, schedules: newTestClock(t, mondayMorning)})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	h.openSettingsPage(2, 101, settingsMessageID(1), "home")
	h.press(3, 101, settingsMessageID(1), "nas:writes-on")
	return h, runtime, roots[0].Roots[1].Path
}

// A forwarded PDF with a deadline is kept as a record and its follow-ups wait on one card: nothing
// runs before Confirm, after outside content, and Confirm runs both.
func TestForwardedInvoiceIsRecordedAndConfirmRunsItsFollowUps(t *testing.T) {
	t.Parallel()
	h, runtime, inbox := newChatIntakeHarness(t)
	card := forwardInvoice(t, h, runtime, 10)
	if _, err := os.Stat(filepath.Join(inbox, "water-bill.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file was filed before Confirm: %v", err)
	}
	if h.countRows(`SELECT count(*) FROM intake_records WHERE principal_id = 'alice' AND source = 'file' AND kind = 'invoice'`) != 1 {
		t.Fatal("the invoice was not kept")
	}

	confirm, _ := approvalButtons(t, card.keyboard)
	h.push(callbackUpdate(11, 101, card.messageID, confirm))
	h.wantAnswer(11)
	if edit := h.nextEdit(); strings.Count(edit.text, "Done.") != 2 || !strings.Contains(edit.text, "Reminder set for") {
		t.Fatalf("card after Confirm = %q", edit.text)
	}
	h.continueTurn(runtime, 11)
	if h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE principal_id = 'alice' AND job_type = ?`, schedule.TypeReminder) != 1 {
		t.Fatal("Confirm set no reminder")
	}
	if saved, err := os.ReadFile(filepath.Join(inbox, "water-bill.pdf")); err != nil || string(saved) != "%PDF-1.4 sample invoice" {
		t.Fatalf("filed document = %q %v", saved, err)
	}
}

// Reject runs neither follow-up, and the record stays.
func TestForwardedInvoiceRejectRunsNothing(t *testing.T) {
	t.Parallel()
	h, runtime, inbox := newChatIntakeHarness(t)
	card := forwardInvoice(t, h, runtime, 10)

	_, reject := approvalButtons(t, card.keyboard)
	h.push(callbackUpdate(11, 101, card.messageID, reject))
	h.wantAnswer(11)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Rejected; nothing was written.") {
		t.Fatalf("card after Reject = %q", edit.text)
	}
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
	if h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatal("Reject set a reminder")
	}
	if _, err := os.Stat(filepath.Join(inbox, "water-bill.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Reject filed the document: %v", err)
	}
	if h.countRows(`SELECT count(*) FROM intake_records`) != 1 {
		t.Fatal("the record went with the card")
	}
}

// inboxHarness runs the inbox check against the model the test answers, with alice's mail
// readable.
type inboxHarness struct {
	*conversationHarness
	runtime  *controlledRuntime
	clock    *testClock
	settings int64
	nextID   int64
}

func newInboxHarness(t *testing.T, options harnessOptions) *inboxHarness {
	t.Helper()
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	sim := googleSimulator(t)
	options.schedules, options.google = clock, sim
	h := newConversationHarnessWithOptions(t, runtime, options)
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	h.connectGoogle(sim, 1, 101, aliceMailbox, googlesim.Grant{Mail: true})
	h.requestSettings(10, 101)
	h.openSettingsPage(11, 101, settingsMessageID(10), "tools")
	return &inboxHarness{conversationHarness: h, runtime: runtime, clock: clock, settings: settingsMessageID(10), nextID: 12}
}

func (h *inboxHarness) update() int64 {
	h.nextID++
	return h.nextID - 1
}

// check presses the button that queues a check, answers the check's turn with reply, and returns
// the turn's message.
func (h *inboxHarness) check(action, reply string) string {
	h.t.Helper()
	return h.checkTurn(action, func(runtimeCall) string { return reply })
}

// checkTurn is check with a turn that may call its tools before it answers.
func (h *inboxHarness) checkTurn(action string, turn func(runtimeCall) string) string {
	h.t.Helper()
	id := schedule.InboxJobID("alice")
	for h.countRows(`SELECT count(*) FROM schedule_runs WHERE job_id = ? AND status IN ('queued', 'running', 'ready')`, id) != 0 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			h.t.Fatal("the previous check never finished")
		}
	}
	h.clock.Set(h.t, h.clock.Now().Add(5*time.Minute).Format(time.RFC3339))
	if page := h.press(h.update(), 101, h.settings, "inbox:"+action); !strings.Contains(page.text, InboxCheckingNote) {
		h.t.Fatalf("page after %s = %q", action, page.text)
	}
	call := receive(h.t, h.ctx, h.runtime.calls)
	if !strings.Contains(call.request.Message, "Check the user's new mail") || slices.Contains(toolNames(call.request.Tools), "intake_record") {
		h.t.Fatalf("check turn = %q with tools %v", call.request.Message, toolNames(call.request.Tools))
	}
	call.answer <- agentruntime.RunResult{Final: turn(call)}
	return call.request.Message
}

// readsAfter is the after: of a check's message, which is where it reads mail from.
func readsAfter(t *testing.T, message string) time.Time {
	t.Helper()
	match := regexp.MustCompile(`with after:(\d+)`).FindStringSubmatch(message)
	if match == nil {
		t.Fatalf("check message has no after: %q", message)
	}
	seconds, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return time.Unix(seconds, 0)
}

// nextCard collects a card's messages up to the one with its buttons.
func (h *inboxHarness) nextCard() (string, sentMessage) {
	h.t.Helper()
	var text []string
	for {
		message := h.nextMessage()
		text = append(text, message.text)
		if len(message.keyboard) > 0 {
			return strings.Join(text, "\n\n"), message
		}
	}
}

func (h *inboxHarness) pressCard(card sentMessage, confirm bool) string {
	h.t.Helper()
	confirmData, rejectData := approvalButtons(h.t, card.keyboard)
	data := rejectData
	if confirm {
		data = confirmData
	}
	id := h.update()
	h.push(callbackUpdate(id, 101, card.messageID, data))
	h.wantAnswer(id)
	return h.nextEdit().text
}

// unoffered counts alice's pending follow-ups that wait for a card.
func (h *inboxHarness) unoffered() int {
	return h.countRows(`SELECT count(*) FROM intake_records, json_each(intake_records.pending_actions)
		WHERE principal_id = 'alice' AND json_extract(json_each.value, '$.approval_id') IS NULL`)
}

func mail(id, kind, title string, actions ...string) string {
	return fmt.Sprintf(`{"account":"alice@example.com","message_id":%q,"kind":%q,"title":%q,"sender":"Example Sender",`+
		`"dates":[{"label":"due","date":"2026-10-15"}],"amounts":[],"references":[{"label":"reference","value":%q}],"action_required":"Act by 15 October.","actions":[%s]}`,
		id, kind, title, "R-"+id, strings.Join(actions, ","))
}

func reminderAction(text string) string {
	return fmt.Sprintf(`{"type":"reminder","text":%q,"at":"2026-10-14 09:00"}`, text)
}

func scanReplyOf(messages ...string) string {
	return `{"messages":[` + strings.Join(messages, ",") + `],"left":0,"through":""}`
}

// The check stages a message's follow-ups on one card in the Inbox tab, with the summary after it.
// While that card waits, a later check sends its summary but no card and does not record the same
// mail again; once the card is confirmed, the next check offers what waited.
func TestInboxCheckOffersOneCardAndWaitsBehindAnUnansweredOne(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{topics: true, watches: &stubEvaluator{}})
	invoice := mail("m1", "invoice", "Water bill", reminderAction("Pay the water bill"),
		`{"type":"watch","name":"Water bill paid","instruction":"Search Gmail for a payment receipt from Example Water.","condition":"a receipt for the water bill arrived","until":"2026-10-20"}`)
	message := h.check("on", scanReplyOf(invoice))
	if !strings.Contains(message, "with after:") {
		t.Fatalf("check message = %q", message)
	}
	tab := h.wantFeedTab("Inbox", thread.FeedInbox)
	text, card := h.nextCard()
	for _, want := range []string{intake.InboxHeading, "2 changes, confirmed or rejected together:", "I read this from mail, which is outside content",
		"About: Water bill, from Example Sender", "Set a reminder for", "Pay the water bill", "Watch Water bill paid"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card lacks %q:\n%s", want, text)
		}
	}
	if card.threadID != tab {
		t.Fatalf("card went to tab %d, want the Inbox tab %d", card.threadID, tab)
	}
	report := h.nextMessage()
	if report.threadID != tab || !strings.Contains(report.text, "New mail that needs something from you:") || !strings.Contains(report.text, "Water bill (invoice), from Example Sender") ||
		!strings.Contains(report.text, "- reference: R-m1") || !strings.Contains(report.text, "on the card above") {
		t.Fatalf("report = %+v", report)
	}

	parcel := mail("m2", "parcel notice", "Parcel on its way", reminderAction("Be home for the parcel"))
	h.check("check", scanReplyOf(invoice, parcel))
	report = h.nextMessage()
	if len(report.keyboard) != 0 || !strings.Contains(report.text, "Parcel on its way") || strings.Contains(report.text, "Water bill") ||
		!strings.Contains(report.text, "An earlier inbox card is still waiting, so 1 follow-up waits until you answer it or it expires.") {
		t.Fatalf("report while the card waits = %+v", report)
	}
	if h.countRows(`SELECT count(*) FROM intake_records`) != 2 || h.countRows(`SELECT count(*) FROM approvals`) != 2 || h.unoffered() != 1 {
		t.Fatalf("records %d, approvals %d, waiting %d", h.countRows(`SELECT count(*) FROM intake_records`), h.countRows(`SELECT count(*) FROM approvals`), h.unoffered())
	}

	if edit := h.pressCard(card, true); strings.Count(edit, "Done.") != 2 {
		t.Fatalf("card after Confirm = %q", edit)
	}
	if h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE job_type = ?`, schedule.TypeReminder) != 1 || h.countRows(`SELECT count(*) FROM watches WHERE kind = 'agent'`) != 1 {
		t.Fatal("Confirm did not set the reminder and the watch")
	}

	h.check("check", noMail)
	text, card = h.nextCard()
	if !strings.HasPrefix(text, intake.InboxHeading) || !strings.Contains(text, "Be home for the parcel") || strings.Contains(text, "Pay the water bill") {
		t.Fatalf("second card = %q", text)
	}
	if report = h.nextMessage(); !strings.Contains(report.text, "Follow-ups for mail I told you about earlier are on the card above") {
		t.Fatalf("report with the second card = %+v", report)
	}
	if h.unoffered() != 0 || h.countRows(`SELECT count(*) FROM intake_records WHERE json_array_length(pending_actions) > 0`) != 1 {
		t.Fatal("the offered follow-up is not tracked, or the confirmed ones were kept")
	}
}

// A card carries at most ten follow-ups; the eleventh waits for the next card.
func TestInboxCardHoldsTenFollowUps(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{})
	actions := func(prefix string, count int) []string {
		var list []string
		for index := range count {
			list = append(list, reminderAction(fmt.Sprintf("%s %d", prefix, index+1)))
		}
		return list
	}
	h.check("on", scanReplyOf(mail("a", "bill", "Bill A", actions("Pay A", 3)...), mail("b", "bill", "Bill B", actions("Pay B", 3)...),
		mail("c", "bill", "Bill C", actions("Pay C", 3)...), mail("d", "bill", "Bill D", actions("Pay D", 2)...)))
	text, _ := h.nextCard()
	if !strings.Contains(text, "10 changes, confirmed or rejected together:") || !strings.Contains(text, "Pay D 1") || strings.Contains(text, "Pay D 2") {
		t.Fatalf("card = %q", text)
	}
	if report := h.nextMessage(); !strings.Contains(report.text, "A card holds at most 10, so 1 follow-up waits for the next card.") {
		t.Fatalf("report = %q", report.text)
	}
	if h.unoffered() != 1 {
		t.Fatalf("waiting follow-ups = %d", h.unoffered())
	}
}

// A chat proposal in the Inbox tab replaces its card, and the replaced follow-ups wait again; a
// check never replaces the chat's card, and offers them again once it is answered. A turn that read
// a record is outside content, so its reminder is only proposed.
func TestChatProposalReturnsReplacedInboxFollowUpsToWaiting(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{})
	h.check("on", scanReplyOf(mail("m1", "invoice", "Water bill", reminderAction("Pay the water bill"))))
	_, card := h.nextCard()
	h.nextMessage()
	offered := approvalID(buttonData(card.keyboard)[0])

	chatUpdate := h.update()
	h.push(textUpdate(chatUpdate, 101, "when is the water bill due?"))
	call := receive(t, h.ctx, h.runtime.calls)
	if listed := executeTool(t, call.ctx, call.request.Tools, "intake_list", `{}`); listed.IsError || !strings.Contains(listed.Text, "Water bill") {
		t.Fatalf("intake_list = %+v", listed)
	}
	if result := executeTool(t, call.ctx, call.request.Tools, "reminder_set", `{"text":"Water bill","at":"2026-10-13 18:00"}`); !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("reminder_set after reading a record = %+v, want it proposed", result)
	}
	h.finishTurn(call, chatUpdate, "It is due on 15 October.")
	chatCard := h.nextMessage()
	if status, reason := h.approvalStatus(offered); status != approval.StatusExpired || reason != approval.ReasonReplaced {
		t.Fatalf("inbox card = %s %s, want it replaced", status, reason)
	}
	if edit := h.nextEdit(); edit.messageID != card.messageID || !strings.Contains(edit.text, "Replaced by a newer proposal") {
		t.Fatalf("inbox card edit = %+v", edit)
	}

	h.check("check", noMail)
	if report := h.nextMessage(); len(report.keyboard) != 0 || !strings.HasSuffix(report.text, schedule.QuietManualText) {
		t.Fatalf("check while the chat card waits = %+v", report)
	}
	if h.unoffered() != 1 {
		t.Fatalf("waiting follow-ups = %d", h.unoffered())
	}
	if edit := h.pressCard(chatCard, false); !strings.Contains(edit, "Rejected") {
		t.Fatalf("chat card after Reject = %q", edit)
	}

	h.check("check", noMail)
	if text, _ := h.nextCard(); !strings.HasPrefix(text, intake.InboxHeading) || !strings.Contains(text, "Pay the water bill") {
		t.Fatalf("card offered again = %q", text)
	}
}

// A reply that is not the JSON asked for fails the run and records nothing; a second one in a row
// on the same window skips it, once, rather than failing every few hours. The first check reads a
// day back, a check with nothing actionable sends nothing, and each check reads from where the last
// one read to, a few minutes earlier for mail indexed late.
func TestInboxCheckFailsOnMalformedJSONSkipsAStuckWindowAndStaysQuietWithoutMail(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{})
	if after := readsAfter(t, h.check("on", "I found an invoice from the water company.")); !after.Equal(h.clock.Now().Add(-24*time.Hour - 5*time.Minute)) {
		t.Fatalf("the first check reads after %s, want a day back", after)
	}
	if report := h.nextMessage(); !strings.HasSuffix(report.text, schedule.FailedText) {
		t.Fatalf("report = %q", report.text)
	}
	for _, table := range []string{"intake_records", "approvals"} {
		if count := h.countRows(`SELECT count(*) FROM ` + table); count != 0 {
			t.Fatalf("%s has %d rows after a malformed reply", table, count)
		}
	}
	if h.countRows(`SELECT count(*) FROM intake_scans WHERE stalls = 1`) != 1 {
		t.Fatal("the failed check was not counted")
	}

	h.check("check", "Sorry, I cannot help with that.")
	skippedAt := h.clock.Now()
	if report := h.nextMessage(); !strings.Contains(report.text, "Two inbox checks in a row could not read your mail from") || !strings.Contains(report.text, "so I skipped it") {
		t.Fatalf("report after the second failure = %q", report.text)
	}
	if h.countRows(`SELECT count(*) FROM intake_scans WHERE stalls = 0`) != 1 {
		t.Fatal("the skipped window still counts")
	}

	if after := readsAfter(t, h.check("check", noMail)); !after.Equal(skippedAt.Add(-5 * time.Minute)) {
		t.Fatalf("the check after the skip reads after %s, want %s", after, skippedAt.Add(-5*time.Minute))
	}
	if report := h.nextMessage(); !strings.HasSuffix(report.text, schedule.QuietManualText) {
		t.Fatalf("manual report = %q", report.text)
	}
	readUntil := h.clock.Now()
	h.clock.Set(t, "2026-09-28T11:00:30+02:00")
	call := receive(t, h.ctx, h.runtime.calls)
	if after := readsAfter(t, call.request.Message); !after.Equal(readUntil.Add(-5 * time.Minute)) {
		t.Fatalf("scheduled check reads after %s, want %s", after, readUntil.Add(-5*time.Minute))
	}
	call.answer <- agentruntime.RunResult{Final: noMail}
	h.waitRun(schedule.InboxJobID("alice"), schedule.StatusSkipped)
	h.expectNoMessage(50 * time.Millisecond)
}

// The switch is offered only once mail can be read, and creates and deletes the principal's own
// job; with it off there is no job to run.
func TestInboxSwitchNeedsMailAndManagesItsJob(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	sim := googleSimulator(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: newTestClock(t, mondayMorning), google: sim})
	h.provision("alice", "account-alice")
	h.start()
	h.requestSettings(1, 101)
	page := h.openSettingsPage(2, 101, settingsMessageID(1), "tools")
	if !strings.Contains(page.text, InboxNeedsMailLine) || slices.Contains(buttonData(page.keyboard), "inbox:on") {
		t.Fatalf("page without mail = %+v", page)
	}
	if page = h.press(3, 101, settingsMessageID(1), "inbox:on"); h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatalf("the switch created a job without mail: %+v", page)
	}

	h.connectGoogle(sim, 10, 101, aliceMailbox, googlesim.Grant{Calendar: true})
	h.requestSettings(20, 101)
	if page = h.openSettingsPage(21, 101, settingsMessageID(20), "tools"); !strings.Contains(page.text, InboxNeedsMailLine) {
		t.Fatalf("page with calendar only = %+v", page)
	}
	h.connectGoogle(sim, 30, 101, "alice@work.example", googlesim.Grant{Mail: true})
	h.requestSettings(40, 101)
	page = h.openSettingsPage(41, 101, settingsMessageID(40), "tools")
	if !strings.Contains(page.text, InboxOffLine) || !slices.Contains(buttonData(page.keyboard), "inbox:on") {
		t.Fatalf("page with mail = %+v", page)
	}
	if page = h.press(42, 101, settingsMessageID(40), "inbox:on"); !strings.Contains(page.text, InboxOnLine) || !strings.Contains(page.text, InboxCheckingNote) {
		t.Fatalf("page after on = %+v", page)
	}
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Final: scanReplyOf(mail("m1", "bill", "Bill A", reminderAction("Pay A 1"), reminderAction("Pay A 2")))}
	if card := h.nextMessage(); !strings.HasPrefix(card.text, intake.InboxHeading) {
		t.Fatalf("first check card = %q", card.text)
	}
	if report := h.nextMessage(); !strings.Contains(report.text, "Bill A") {
		t.Fatalf("first check report = %q", report.text)
	}
	jobs, err := h.scheduler.Jobs(h.ctx, "alice")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if job := jobs[0]; job.ID != schedule.InboxJobID("alice") || job.Type != schedule.InboxScanType || job.Cron != schedule.InboxCron || job.Name != schedule.InboxName || !job.Enabled {
		t.Fatalf("job = %+v", job)
	}
	if bobs, err := h.scheduler.Jobs(h.ctx, "bob"); err != nil || len(bobs) != 0 {
		t.Fatalf("bob's jobs = %+v %v", bobs, err)
	}

	if page = h.press(43, 101, settingsMessageID(40), "inbox:off"); !strings.Contains(page.text, InboxOffLine) || h.countRows(`SELECT count(*) FROM scheduled_jobs`) != 0 {
		t.Fatalf("page after off = %+v", page)
	}
	// Turning it off forgets where it read to and what waited, so turning it on again starts from
	// the last day; the record itself stays.
	if h.countRows(`SELECT count(*) FROM intake_scans`) != 0 || h.countRows(`SELECT count(*) FROM intake_records WHERE pending_actions != '[]'`) != 0 ||
		h.countRows(`SELECT count(*) FROM intake_records`) != 1 {
		t.Fatal("turning the check off kept its read position or its waiting follow-ups")
	}
	h.press(44, 101, settingsMessageID(40), "inbox:check")
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
}

// OfferIfIdle and a chat proposal in the same tab, at once: the chat proposal is never replaced.
// Either the offer comes first and the chat proposal replaces it, or the offer finds it waiting.
func TestOfferIfIdleNeverReplacesAChatProposal(t *testing.T) {
	t.Parallel()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{schedules: newTestClock(t, mondayMorning)})
	h.provision("alice", "account-alice")
	h.start()
	for round := range 20 {
		chat, err := h.scheduler.ReminderProposal(h.ctx, "alice", fmt.Sprintf("Chat reminder %d", round), "2026-10-14 09:00")
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := h.scheduler.ReminderProposal(h.ctx, "alice", fmt.Sprintf("Inbox reminder %d", round), "2026-10-14 10:00")
		if err != nil {
			t.Fatal(err)
		}
		var group sync.WaitGroup
		var proposed approval.Record
		var proposeErr, offerErr error
		var offered []approval.Record
		group.Add(2)
		go func() {
			defer group.Done()
			proposed, proposeErr = h.approvals.Propose(h.ctx, "alice", chat)
		}()
		go func() {
			defer group.Done()
			offered, offerErr = h.approvals.OfferIfIdle(h.ctx, "alice", 101, 0, intake.InboxHeading, intake.InboxLifetime, []approval.Proposal{inbox})
		}()
		group.Wait()
		if proposeErr != nil {
			t.Fatal(proposeErr)
		}
		if status, reason := h.approvalStatus(proposed.ID); status != approval.StatusPending {
			t.Fatalf("round %d: the chat proposal is %s %s", round, status, reason)
		}
		switch {
		case errors.Is(offerErr, approval.ErrBundleWaiting):
		case offerErr != nil:
			t.Fatal(offerErr)
		default:
			h.nextMessage()
			if status, reason := h.approvalStatus(offered[0].ID); status != approval.StatusExpired || reason != approval.ReasonReplaced {
				t.Fatalf("round %d: the offer that came first is %s %s", round, status, reason)
			}
		}
		if _, err := h.db.ExecContext(h.ctx, `UPDATE approvals SET status = 'rejected', finished_at = created_at WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
	}
}

// A chat turn that reads a record has read outside content, so a reminder it sets is only a card.
func TestReadingAnIntakeRecordMakesAReminderACard(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{})
	h.check("on", scanReplyOf(mail("m1", "invoice", "Water bill")))
	report := h.nextMessage()
	id := h.recordID("alice@example.com/m1")
	if !strings.Contains(report.text, "Water bill") {
		t.Fatalf("report = %q", report.text)
	}

	chatUpdate := h.update()
	h.push(textUpdate(chatUpdate, 101, "remind me to pay that water bill"))
	call := receive(t, h.ctx, h.runtime.calls)
	if shown := executeTool(t, call.ctx, call.request.Tools, "intake_get", `{"id":"`+id+`"}`); shown.IsError || !strings.Contains(shown.Text, "Water bill") {
		t.Fatalf("intake_get = %+v", shown)
	}
	if result := executeTool(t, call.ctx, call.request.Tools, "reminder_set", `{"text":"Pay the water bill","at":"2026-10-14 09:00"}`); !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("reminder_set after intake_get = %+v, want it proposed", result)
	}
	h.finishTurn(call, chatUpdate, "I propose a reminder.")
	if card := h.nextMessage(); !strings.Contains(card.text, "Pay the water bill") || len(card.keyboard) == 0 {
		t.Fatalf("card = %+v", card)
	}
	if h.countRows(`SELECT count(*) FROM scheduled_jobs WHERE job_type = ?`, schedule.TypeReminder) != 0 {
		t.Fatal("the reminder was set without the card")
	}
}

func (h *inboxHarness) recordID(sourceRef string) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM intake_records WHERE source_ref = ?`, sourceRef).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// With messages left, the next check reads from Gmail's own received time of the newest message
// this one opened, not from a time the model reports. A check a week or more behind reads a week
// back and says so.
func TestInboxCheckReadsOnFromTheNewestMessageOpened(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{})
	h.clock.Set(t, "2026-09-28T10:00:00+02:00")
	h.checkTurn("on", func(call runtimeCall) string {
		if read := executeTool(t, call.ctx, call.request.Tools, "gmail_read_message", `{"account":"alice@example.com","id":"m2"}`); read.IsError {
			t.Errorf("gmail_read_message = %+v", read)
		}
		return `{"messages":[],"left":2,"through":"2026-09-28T07:59:00Z"}`
	})
	if report := h.nextMessage(); !strings.Contains(report.text, "2 more messages are left for the next check.") {
		t.Fatalf("report = %q", report.text)
	}
	opened := time.Date(2026, 9, 28, 6, 15, 0, 0, time.UTC)
	if after := readsAfter(t, h.check("check", noMail)); !after.Equal(opened.Add(-5 * time.Minute)) {
		t.Fatalf("the next check reads after %s, want the opened message's receipt %s less the overlap", after, opened)
	}
	h.nextMessage()

	tenDaysAgo := h.clock.Now().Add(-10 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := h.db.ExecContext(h.ctx, `UPDATE intake_scans SET read_until = ?`, tenDaysAgo); err != nil {
		t.Fatal(err)
	}
	message := h.check("check", noMail)
	if after := readsAfter(t, message); !after.Equal(h.clock.Now().Add(-7*24*time.Hour - 5*time.Minute)) {
		t.Fatalf("a check ten days behind reads after %s, want a week back", after)
	}
	if report := h.nextMessage(); !strings.Contains(report.text, "A check reads at most a week back, so mail from before") {
		t.Fatalf("report of a capped check = %q", report.text)
	}
}

// A message with a fact it cannot read keeps the rest: a label too long is cut, a date with seconds
// is read, and a bad date, a bad currency, and the watches it may not set are left out and counted.
// A message without a title is left out whole.
func TestInboxCheckKeepsWhatItCanReadOfAMessage(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{watches: &stubEvaluator{}})
	letter := `{"account":"alice@example.com","message_id":"m1","kind":"letter","title":"Annual statement","sender":"Example Bank",` +
		`"dates":[{"label":"` + strings.Repeat("l", 70) + `","date":"2026-10-15 09:30:00"},{"label":"meeting","date":"next Tuesday"}],` +
		`"amounts":[{"label":"fee","value":"12.00","currency":"euros"},{"label":"balance","value":"100.00","currency":"EUR"}],` +
		`"references":[{"label":"account","value":"ST-1"}],"action_required":"Check the statement.","actions":[` +
		`{"type":"watch","name":"Statement follow-up","instruction":"Open https://bank.example/login and check.","condition":"a reply arrived","until":"2026-10-10"},` +
		`{"type":"watch","name":"Reply from the bank","instruction":"Search Gmail for a reply from Example Bank.","condition":"a reply arrived"},` +
		`{"type":"watch","name":"Late reply","instruction":"Search Gmail for a reply from Example Bank.","condition":"a reply arrived","until":"2027-03-01"},` +
		`{"type":"watch","name":"Bank reply","instruction":"Search Gmail for a reply from Example Bank.","condition":"a reply arrived","until":"2026-10-20"}]}`
	untitled := `{"account":"alice@example.com","message_id":"m2","kind":"bill","title":"","sender":"","dates":[],"amounts":[],"references":[],"action_required":"","actions":[]}`
	h.check("on", scanReplyOf(letter, untitled))
	text, _ := h.nextCard()
	if !strings.Contains(text, "Watch Bank reply") || strings.Contains(text, "Statement follow-up") || strings.Contains(text, "Late reply") || strings.Contains(text, "Reply from the bank") {
		t.Fatalf("card = %q", text)
	}
	report := h.nextMessage()
	for _, want := range []string{
		"Annual statement (letter), from Example Bank", "- " + strings.Repeat("l", 59) + "…: 2026-10-15 09:30", "- balance: 100.00 EUR", "- account: ST-1",
		"I could not read 1 message and left it out.", "I left out or shortened 6 details I could not read.",
	} {
		if !strings.Contains(report.text, want) {
			t.Fatalf("report lacks %q:\n%s", want, report.text)
		}
	}
	if strings.Contains(report.text, "next Tuesday") || strings.Contains(report.text, "fee") {
		t.Fatalf("a fact that could not be read was kept:\n%s", report.text)
	}
}

// A card that expired before the sweep reached it gives its follow-ups back to the next check, up
// to three cards; then the follow-up is dropped, and the summary says so once.
func TestInboxFollowUpExpiresOnThreeCardsAndIsDropped(t *testing.T) {
	t.Parallel()
	h := newInboxHarness(t, harnessOptions{})
	expire := func() {
		t.Helper()
		if _, err := h.db.ExecContext(h.ctx, `UPDATE approvals SET created_at = '2026-08-31T00:00:00.000Z', expires_at = '2026-09-01T00:00:00.000Z' WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
	}
	h.check("on", scanReplyOf(mail("m1", "invoice", "Water bill", reminderAction("Pay the water bill"))))
	h.nextCard()
	h.nextMessage()
	for round := 1; round < intake.MaximumExpiries; round++ {
		expire()
		h.check("check", noMail)
		if text, _ := h.nextCard(); !strings.Contains(text, "Pay the water bill") {
			t.Fatalf("round %d card = %q", round, text)
		}
		if report := h.nextMessage(); !strings.Contains(report.text, "Follow-ups for mail I told you about earlier are on the card above") {
			t.Fatalf("round %d report = %q", round, report.text)
		}
		if edit := h.nextEdit(); !strings.Contains(edit.text, "Replaced by a newer proposal") && !strings.Contains(edit.text, "Expired") {
			t.Fatalf("round %d edit of the expired card = %q", round, edit.text)
		}
	}
	expire()
	h.check("check", noMail)
	if report := h.nextMessage(); len(report.keyboard) != 0 || !strings.Contains(report.text, "1 follow-up expired unanswered on 3 cards, so I dropped it.") {
		t.Fatalf("report when the follow-up is dropped = %+v", report)
	}
	if h.countRows(`SELECT count(*) FROM intake_records WHERE pending_actions != '[]'`) != 0 {
		t.Fatal("the dropped follow-up is still kept")
	}
	h.nextEdit()
	h.check("check", noMail)
	if report := h.nextMessage(); !strings.HasSuffix(report.text, schedule.QuietManualText) {
		t.Fatalf("the drop was told again: %q", report.text)
	}
}

// After outside content, recording an item already kept with other facts, or deleting a record, is
// only proposed; the record changes once the user confirms.
func TestChangingOrDeletingARecordAfterOutsideContentNeedsACard(t *testing.T) {
	t.Parallel()
	h, runtime, _ := newChatIntakeHarness(t)
	first := forwardInvoice(t, h, runtime, 10)
	var attachmentID, recordID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT source_ref, id FROM intake_records`).Scan(&attachmentID, &recordID); err != nil {
		t.Fatal(err)
	}

	h.push(textUpdate(11, 101, "the total is actually 45.00"))
	call := receive(t, h.ctx, runtime.calls)
	changed := strings.Replace(fmt.Sprintf(sampleInvoice, attachmentID), `"value":"42.10"`, `"value":"45.00"`, 1)
	if result := executeTool(t, call.ctx, call.request.Tools, "intake_record", changed); !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("changed record = %+v, want it proposed", result)
	}
	h.finishTurn(call, 11, "I propose the change.")
	if edit := h.nextEdit(); edit.messageID != first.messageID {
		t.Fatalf("first card edit = %+v", edit)
	}
	card := h.nextMessage()
	if !strings.Contains(card.text, "Change the intake record Water bill for the third quarter (invoice) to:") || !strings.Contains(card.text, "- total: 45.00 EUR") {
		t.Fatalf("card = %q", card.text)
	}
	if h.countRows(`SELECT count(*) FROM intake_records WHERE facts LIKE '%42.10%'`) != 1 {
		t.Fatal("the record changed before the card was answered")
	}
	confirm, _ := approvalButtons(t, card.keyboard)
	h.push(callbackUpdate(12, 101, card.messageID, confirm))
	h.wantAnswer(12)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Updated the intake record") {
		t.Fatalf("card after Confirm = %q", edit.text)
	}
	h.continueTurn(runtime, 12)
	if h.countRows(`SELECT count(*) FROM intake_records WHERE facts LIKE '%45.00%'`) != 1 {
		t.Fatal("Confirm did not change the record")
	}

	h.push(textUpdate(13, 101, "forget that bill"))
	call = receive(t, h.ctx, runtime.calls)
	if result := executeTool(t, call.ctx, call.request.Tools, "intake_delete", `{"id":"`+recordID+`"}`); !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("intake_delete = %+v, want it proposed", result)
	}
	h.finishTurn(call, 13, "I propose deleting it.")
	if card = h.nextMessage(); !strings.Contains(card.text, "Delete the intake record Water bill for the third quarter") {
		t.Fatalf("delete card = %q", card.text)
	}
	if h.countRows(`SELECT count(*) FROM intake_records`) != 1 {
		t.Fatal("the record was deleted without the card")
	}
}
