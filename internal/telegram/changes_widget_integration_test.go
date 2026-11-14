package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func decodeChanges(t *testing.T, raw string) changesData {
	t.Helper()
	var data changesData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("changes widget data %q: %v", raw, err)
	}
	return data
}

func keepTap(approvalID, flags string) string { return "approval:keep:" + approvalID + ":" + flags }

func readTap(approvalID string, number int) string {
	return "approval:read:" + approvalID + ":" + strconv.Itoa(number)
}

// proposeChanges makes a chat turn's proposals for Alice, delivers them, and returns the widget
// that shows them with the ID of each record.
func (h *conversationHarness) proposeChanges(proposals ...approval.Proposal) (sentWidget, []string) {
	h.t.Helper()
	records, err := h.approvals.ProposeAll(h.ctx, "alice", proposals)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		h.t.Fatal(err)
	}
	ids := make([]string, len(records))
	for index, record := range records {
		ids[index] = record.ID
	}
	return h.nextWidget(), ids
}

func (h *conversationHarness) sendAttempts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.widgets.sendAttempts
}

func (h *conversationHarness) wantNoMessage() {
	h.t.Helper()
	if count := len(h.messages); count != 0 {
		h.t.Fatalf("%d messages were sent, want none", count)
	}
}

// Two changes whose texts fit beside their titles are one changes widget carrying the texts, which
// open with no request; the caption counts them, and no text message goes out.
func TestChangesWidgetCarriesTheTextsThatFit(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	widget, _ := h.proposeChanges(
		outcomeProposal(t, "Save the note A\nWith every line of the list.", "Saved A.", false),
		outcomeProposal(t, "Add to the list:\nmilk and eggs", "Saved B.", false),
	)
	data := decodeChanges(t, widget.data)
	if widget.caption != "2 changes" || data.Heading != "" || data.Status != "Expires in 10 minutes." || data.Preview || len(data.Changes) != 2 ||
		data.Changes[0] != (changesItem{Title: "Save the note A", State: "w", Text: "Save the note A\nWith every line of the list."}) ||
		data.Changes[1] != (changesItem{Title: "Add to the list: milk and eggs", State: "w", Text: "Add to the list:\nmilk and eggs"}) {
		t.Fatalf("changes widget = %q, %+v", widget.caption, data)
	}
	if !strings.Contains(widget.html, "approval:keep:") || !h.widgetKindIs(widget.messageID, "changes") {
		t.Fatal("the widget does not run the changes view, or its record does not say so")
	}
	h.wantNoMessage()
}

func (h *conversationHarness) widgetKindIs(messageID int64, kind string) bool {
	h.t.Helper()
	return h.countRows(`SELECT count(*) FROM approvals WHERE message_id = ? AND widget_kind = ?`, messageID, kind) > 0
}

// Two changes whose texts do not fit are one changes widget with their titles only, and no text
// message: the texts are read through a tap.
func TestChangesWidgetCarriesTitlesOnlyWhenTheTextsDoNotFit(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	long := strings.Repeat("A line of the list. ", 30)
	widget, _ := h.proposeChanges(
		outcomeProposal(t, "Save the note A\n"+long, "Saved A.", false),
		outcomeProposal(t, "Save the note B\n"+long, "Saved B.", false),
	)
	data := decodeChanges(t, widget.data)
	if widget.caption != "2 changes" || len(data.Changes) != 2 || data.Changes[0].Title != "Save the note A" || data.Changes[1].Title != "Save the note B" ||
		data.Changes[0].Text != "" || data.Changes[1].Text != "" {
		t.Fatalf("changes widget = %q, %+v", widget.caption, data)
	}
	h.wantNoMessage()
}

// A single change is the proposal widget, and with widgets off a list is text with buttons.
func TestChangesWidgetIsNotUsedForOneChangeOrWithWidgetsOff(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	widget, _ := h.proposeChanges(outcomeProposal(t, "Save the note A", "Saved A.", false))
	if !h.widgetKindIs(widget.messageID, "proposal") || widget.caption != "Save the note A" {
		t.Fatalf("a single change = %+v", widget)
	}

	h.setWidgets("alice", false)
	if _, err := h.approvals.ProposeAll(h.ctx, "alice", []approval.Proposal{outcomeProposal(t, "Save the note B", "Saved.", false), outcomeProposal(t, "Save the note C", "Saved.", false)}); err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		t.Fatal(err)
	}
	message := h.nextMessage()
	if len(buttonData(message.keyboard)) != 2 || !strings.Contains(message.text, "1. Save the note B") || !strings.Contains(message.text, "2. Save the note C") {
		t.Fatalf("a list with widgets off = %+v", message)
	}
	if attempts := h.sendAttempts(); attempts != 1 {
		t.Fatalf("%d widgets sent, want the single change's only", attempts)
	}
}

// A list whose data cannot fit even with its titles at their floor is not a widget, and one
// Telegram refuses is not either; both go out as text with buttons, and no proposal widget is tried.
func TestChangesWidgetThatCannotBeSentGoesOutAsText(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	var crowd []approval.Proposal
	for index := range 10 {
		title := strings.Repeat("界", 29) + string(rune('一'+index))
		crowd = append(crowd, outcomeProposal(t, approval.IntentWarning+"\n\n"+title+"\nWith more text of the change.", "Saved.", false))
	}
	if _, err := h.approvals.Offer(h.ctx, "alice", 101, 0, strings.Repeat("Weekly review of the week ", 3)[:64], 10*time.Minute, crowd); err != nil {
		t.Fatal(err)
	}
	message := h.nextMessage()
	if len(buttonData(message.keyboard)) != 2 || !strings.Contains(message.text, "10. ") {
		t.Fatalf("a list that cannot fit = %+v", message)
	}
	if attempts := h.sendAttempts(); attempts != 0 {
		t.Fatalf("%d widgets tried for a list that cannot fit", attempts)
	}

	h.mu.Lock()
	h.widgets.sendStatus = http.StatusBadRequest
	h.mu.Unlock()
	if _, err := h.approvals.ProposeAll(h.ctx, "alice", []approval.Proposal{outcomeProposal(t, "Save the note C", "Saved.", false), outcomeProposal(t, "Save the note D", "Saved.", false)}); err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		t.Fatal(err)
	}
	message = h.nextMessage()
	if len(buttonData(message.keyboard)) != 2 || !strings.Contains(message.text, "2. Save the note D") {
		t.Fatalf("a list Telegram refuses = %+v", message)
	}
	h.waitMessageRecorded(message.messageID)
	if attempts := h.sendAttempts(); attempts != 1 || h.widgetFlagged(message.messageID) || h.widgetKindIs(message.messageID, "changes") {
		t.Fatalf("%d widgets tried for the refused list, or its text is recorded as a widget", attempts)
	}
}

// Ten changes with titles in a script of two-byte letters fit the data before and after they are
// resolved, and no tap the view builds is over 64 bytes, which the fake Telegram checks.
func TestChangesWidgetOfTenChangesFitsInAnyScript(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	var proposals []approval.Proposal
	for index := range 10 {
		title := strings.Repeat("Ж", 30) + string(rune('a'+index))
		proposals = append(proposals, outcomeProposal(t, title+"\nWith more text of the change.", "Сохранено.", false))
	}
	widget, _ := h.proposeChanges(proposals...)
	data := decodeChanges(t, widget.data)
	if len(widget.data) > widgetDataLimit || len(data.Changes) != 10 {
		t.Fatalf("ten changes = %d bytes, %d changes", len(widget.data), len(data.Changes))
	}
	for _, item := range data.Changes {
		if utf8.RuneCountInString(item.Title) < changesTitleFloor {
			t.Fatalf("title %q is below the floor", item.Title)
		}
	}
	flags := strings.Repeat("1", 10)
	if tap := keepTap(data.ID, flags); len(tap) > 64 {
		t.Fatalf("the longest keep tap is %d bytes", len(tap))
	}
	h.push(widgetTap(1, 101, widget.messageID, keepTap(data.ID, flags)))
	h.wantWidgetAnswer(1)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	resolved := decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if len(resolved.Changes) != 10 || resolved.Changes[9].State != "d" {
		t.Fatalf("resolved changes = %+v", resolved)
	}
	for _, item := range resolved.Changes {
		if utf8.RuneCountInString(item.Title) < changesTitleFloor {
			t.Fatalf("resolved title %q is below the floor", item.Title)
		}
	}
}

// A tap that reads a change answers its summary as plain text, even once it is resolved. A summary
// longer than an answer holds is answered by its beginning and sent whole once as a reply, and a
// second tap sends no second reply.
func TestChangesWidgetReadsAChange(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	long := "Save the note A\n" + strings.Repeat("A line of the list. ", 80)
	widget, ids := h.proposeChanges(
		outcomeProposal(t, long, "Saved A.", false),
		outcomeProposal(t, "Save the note B\nWith **every** line.", "Saved B.", false),
	)
	id := decodeChanges(t, widget.data).ID

	h.push(widgetTap(1, 101, widget.messageID, readTap(id, 2)))
	if answer := h.wantWidgetAnswer(1); answer != "Save the note B\nWith every line." {
		t.Fatalf("answer = %q", answer)
	}
	h.push(widgetTap(2, 101, widget.messageID, readTap(id, 1)))
	answer := h.wantWidgetAnswer(2)
	if len(answer) > widgetDataLimit || !strings.HasPrefix(answer, "Save the note A\nA line of the list.") || len(answer) < widgetDataLimit-4 {
		t.Fatalf("answer to a long summary = %d bytes %q", len(answer), answer)
	}
	reply := h.nextMessage()
	if reply.replyTo != widget.messageID || !strings.HasPrefix(reply.text, "Save the note A") || !strings.HasSuffix(strings.TrimSpace(reply.text), "A line of the list.") || utf8.RuneCountInString(reply.text) < len(long)-2 {
		t.Fatalf("whole summary = %+v", reply)
	}
	h.push(widgetTap(3, 101, widget.messageID, readTap(id, 1)))
	if answer := h.wantWidgetAnswer(3); !strings.HasPrefix(answer, "Save the note A") {
		t.Fatalf("second answer = %q", answer)
	}
	h.waitOffset(4)
	h.wantNoMessage()

	h.push(widgetTap(4, 101, widget.messageID, keepTap(id, "01")))
	h.wantWidgetAnswer(4)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	h.wantStatuses(ids, "rejected/left_out", "executed")
	h.push(widgetTap(5, 101, widget.messageID, readTap(id, 1)))
	if answer := h.wantWidgetAnswer(5); !strings.HasPrefix(answer, "Save the note A") {
		t.Fatalf("answer for a change left out = %q", answer)
	}
}

// A read tap for another principal's proposal, from another message, or for a change the proposal
// does not have, is answered as doing nothing.
func TestChangesWidgetReadTapsThatNameNothingDoNothing(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	first, _ := h.proposeChanges(outcomeProposal(t, "Save the note A", "Saved.", false), outcomeProposal(t, "Save the note B", "Saved.", false))
	id := decodeChanges(t, first.data).ID

	h.push(widgetTap(1, 202, first.messageID, readTap(id, 1)))
	if answer := h.wantWidgetAnswer(1); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("another principal's tap answered %q", answer)
	}
	h.push(widgetTap(2, 101, first.messageID+100, readTap(id, 1)))
	if answer := h.wantWidgetAnswer(2); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("a tap from another message answered %q", answer)
	}
	h.push(widgetTap(3, 101, first.messageID, readTap(id, 3)))
	if answer := h.wantWidgetAnswer(3); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("a tap for a change the proposal does not have answered %q", answer)
	}
	h.push(widgetTap(4, 101, first.messageID, "approval:read:"+id+":0"))
	if answer := h.wantWidgetAnswer(4); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("a malformed read tap answered %q", answer)
	}
}

// Keeping the first of two runs it and ends the other as left out; the edit shows both, the caption
// counts both, the turn that continues the request is queued with the notice, and a replayed tap
// acts once.
func TestChangesWidgetConfirmKeepsTheFirstAndLeavesOutTheSecond(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)
	h.proposeReports(runtime, 1, "Summarise report one.", "Summarise report two.")
	widget := h.nextWidget()
	data := decodeChanges(t, widget.data)

	h.push(widgetTap(2, 101, widget.messageID, keepTap(data.ID, "10")))
	if answer := h.wantWidgetAnswer(2); answer != localetest.English(WidgetWorkingAnswer) {
		t.Fatalf("answer = %q", answer)
	}
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	after := decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — 1 done, 1 left out" || after.Status != "1 done, 1 left out." || after.Changes[0].State != "d" || after.Changes[1].State != "l" {
		t.Fatalf("after the choice = %q, %+v", caption.caption, after)
	}
	continued := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(continued.request.Message, "The user left out your ") || !strings.HasSuffix(continued.request.Message, prompt.Continue) {
		t.Fatalf("continuation message = %q", continued.request.Message)
	}
	continued.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	h.waitState(2, "delivered")
	if jobs := h.countRows(`SELECT count(*) FROM scheduled_jobs`); jobs != 1 {
		t.Fatalf("jobs = %d, want 1", jobs)
	}

	h.push(widgetTap(3, 101, widget.messageID, keepTap(data.ID, "10")))
	h.wantWidgetAnswer(3)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	h.waitOffset(4)
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if jobs := h.countRows(`SELECT count(*) FROM scheduled_jobs`); jobs != 1 {
		t.Fatalf("jobs after a replayed tap = %d, want 1", jobs)
	}
}

// A Confirm that names the wrong number of changes, flags other than 0 and 1, or keeps none that
// waits is answered as changed or ignored, resolves nothing, and still edits the message to its
// stored state with its controls.
func TestChangesWidgetConfirmThatDoesNotMatchResolvesNothing(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	widget, ids := h.proposeChanges(
		outcomeProposal(t, "Save the note A", "Saved A.", false),
		outcomeProposal(t, "Save the note B", "Saved B.", false),
		outcomeProposal(t, "Save the note C", "Saved C.", false),
	)
	id := decodeChanges(t, widget.data).ID
	h.withdraw(2)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)

	for update, flags := range []string{"11", "1111"} {
		h.push(widgetTap(int64(update+1), 101, widget.messageID, keepTap(id, flags)))
		if answer := h.wantWidgetAnswer(int64(update + 1)); answer != approval.ProposalChangedText {
			t.Fatalf("a Confirm naming %d changes answered %q", len(flags), answer)
		}
		h.nextWidgetEdit("editMessageCaption", widget.messageID)
		state := decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
		if state.Changes[0].State != "w" || state.Changes[1].State != "h" || state.Changes[2].State != "w" {
			t.Fatalf("stored state = %+v", state)
		}
	}
	h.push(widgetTap(3, 101, widget.messageID, keepTap(id, "010")))
	if answer := h.wantWidgetAnswer(3); answer != approval.ProposalChangedText {
		t.Fatalf("a Confirm keeping only a withdrawn change answered %q", answer)
	}
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	h.push(widgetTap(4, 101, widget.messageID, keepTap(id, "1x1")))
	if answer := h.wantWidgetAnswer(4); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("a Confirm with other flags answered %q", answer)
	}
	h.wantStatuses(ids, "pending", "rejected/withdrawn", "pending")

	h.push(widgetTap(5, 101, widget.messageID, keepTap(id, "101")))
	h.wantWidgetAnswer(5)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	h.wantStatuses(ids, "executed", "rejected/withdrawn", "executed")
}

// Reject all rejects every change, and Preview gives the controls back.
func TestChangesWidgetRejectsAllAndPreviews(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	widget, ids := h.proposeChanges(outcomeProposal(t, "Save the note A", "Saved A.", false), outcomeProposal(t, "Save the note B", "Saved B.", false))
	id := decodeChanges(t, widget.data).ID

	h.push(widgetTap(1, 101, widget.messageID, "approval:preview:"+id))
	if answer := h.wantWidgetAnswer(1); answer != "Nothing in this batch can be previewed." {
		t.Fatalf("preview answered %q", answer)
	}
	h.push(widgetTap(2, 101, widget.messageID, "approval:reject:"+id))
	h.wantWidgetAnswer(2)
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	state := decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — rejected" || state.Changes[0].State != "r" || state.Changes[1].State != "r" {
		t.Fatalf("after Reject all = %q, %+v", caption.caption, state)
	}
	h.wantStatuses(ids, "rejected", "rejected")
}

// A button press whose data starts with approval: and is not a button's is answered and ignored.
func TestApprovalKeepButtonPressIsIgnored(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	h.setWidgets("alice", false)
	if _, err := h.approvals.ProposeAll(h.ctx, "alice", []approval.Proposal{outcomeProposal(t, "Save the note A", "Saved.", false), outcomeProposal(t, "Save the note B", "Saved.", false)}); err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		t.Fatal(err)
	}
	message := h.nextMessage()
	confirm, _ := approvalButtons(t, message.keyboard)
	id := approvalID(confirm)

	h.push(callbackUpdate(1, 101, message.messageID, keepTap(id, "11")))
	h.wantAnswer(1)
	h.push(callbackUpdate(2, 101, message.messageID, "approval:unknown:"+id))
	h.wantAnswer(2)
	h.waitOffset(3)
	select {
	case edit := <-h.edits:
		t.Fatalf("an ignored press edited the message: %+v", edit)
	default:
	}
	if status, _ := h.approvalStatus(id); status != approval.StatusPending {
		t.Fatalf("approval after ignored presses = %s", status)
	}
	h.wantNoMessage()
}

// An edit Telegram refuses for good sends the outcome as a reply, split into messages that each fit.
func TestChangesWidgetEditRefusedForGoodRepliesTheText(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	widget, _ := h.proposeChanges(outcomeProposal(t, "Save the note A", "Saved A.", false), outcomeProposal(t, "Save the note B", "Saved B.", false))
	id := decodeChanges(t, widget.data).ID

	h.mu.Lock()
	h.widgets.editStatus = http.StatusBadRequest
	h.mu.Unlock()
	h.push(widgetTap(1, 101, widget.messageID, keepTap(id, "11")))
	h.wantWidgetAnswer(1)
	reply := h.nextMessage()
	if !strings.Contains(reply.text, "1. Save the note A\nDone. Saved A.") || !strings.Contains(reply.text, "2. Save the note B\nDone. Saved B.") {
		t.Fatalf("outcome reply = %+v", reply)
	}
}

// An expired list is edited in its own shape, and a Confirm after the sweep has expired it changes
// nothing and leaves none out.
func TestChangesWidgetShowsExpiry(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{approvalSweep: 100 * time.Millisecond})
	widget, ids := h.proposeChanges(outcomeProposal(t, "Save the note A", "Saved A.", false), outcomeProposal(t, "Save the note B", "Saved B.", false))
	id := decodeChanges(t, widget.data).ID
	h.expirePendingApprovals(widget.messageID)

	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	state := decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — expired" || state.Status != "Expired." && state.Status != "2 expired." || state.Changes[0].State != "e" || state.Changes[1].State != "e" {
		t.Fatalf("after expiry = %q, %+v", caption.caption, state)
	}
	h.push(widgetTap(1, 101, widget.messageID, keepTap(id, "10")))
	h.wantWidgetAnswer(1)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	h.wantStatuses(ids, "expired/timeout", "expired/timeout")
	h.wantNoWidgetTextEdit()
}

// A failed change keeps its whole line, which is its advice when it has no outcome of its own, and a
// done change keeps what follows its leading word.
func TestChangesDataKeepsWhatAFailedChangeSays(t *testing.T) {
	t.Parallel()
	interrupted := "The assistant restarted while executing this; check whether it took effect before trying again."
	data, shortened, fits := encodeChangesData(approval.Shown{
		ID: "aaaaaaaaaaaaaaaa", Status: "1 done, 1 failed.",
		Changes: []approval.Change{
			{Title: "Save the note A", State: approval.ChangeDone, Outcome: "Done. Saved A."},
			{Title: "Save the note B", State: approval.ChangeFailed, Outcome: interrupted},
			{Title: "Save the note C", State: approval.ChangeDone, Outcome: "Done."},
		},
	}, false)
	list := decodeChanges(t, data)
	if !fits || shortened || list.Changes[0].Outcome != "Saved A." || list.Changes[1].Outcome != interrupted || list.Changes[2].Outcome != "" {
		t.Fatalf("data = %v %v %+v", fits, shortened, list)
	}
}

// A list that fits while its changes wait may not fit once they are resolved, since the tally is
// longer than the expiry line. The data is still updated, with the status shortened as a last
// resort, and reported as shortened so that the text is sent.
func TestChangesDataIsUpdatedWhenTheTallyDoesNotFit(t *testing.T) {
	t.Parallel()
	changes := func(state approval.ChangeState, warnedFrom int) []approval.Change {
		var list []approval.Change
		for index := range 10 {
			list = append(list, approval.Change{Title: strings.Repeat("界", 23) + string(rune('一'+index)), State: state, Warned: index < warnedFrom})
		}
		return list
	}
	// The most warned changes the waiting list still carries whole, which puts it at the limit.
	var waiting approval.Shown
	var data string
	var warnedCount int
	for warned := 10; warned >= 0; warned-- {
		waiting = approval.Shown{ID: "aaaaaaaaaaaaaaaa", Status: "Expires in 10 minutes.", Changes: changes(approval.ChangeWaiting, warned)}
		encoded, shortened, fits := encodeChangesData(waiting, true)
		if fits && !shortened {
			data, warnedCount = encoded, warned
			break
		}
	}
	if len(data) < widgetDataLimit-20 {
		t.Fatalf("waiting data = %d bytes; the case needs it just under the limit", len(data))
	}

	resolved := approval.Shown{ID: waiting.ID, Status: "3 done, 2 failed, 3 left out, 2 rejected.", Changes: changes(approval.ChangeLeftOut, warnedCount)}
	data, shortened, fits := encodeChangesData(resolved, false)
	list := decodeChanges(t, data)
	if !fits || !shortened || len(data) > widgetDataLimit || len(list.Changes) != 10 || list.Changes[9].State != "l" || list.Preview ||
		list.Status == resolved.Status || !strings.HasPrefix(resolved.Status, strings.TrimSuffix(list.Status, "…")) {
		t.Fatalf("resolved data = %d bytes, shortened %v, fits %v, %+v", len(data), shortened, fits, list)
	}
}

// Outcomes too long for the data are shortened there with the titles at their floor, and the text of
// the message, the titles with what became of each, is sent once as a reply; data already showing
// it sends no second reply.
func TestChangesWidgetOutcomesTooLongForTheDataAreRepliedOnce(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	outcome := strings.Repeat("Saved a line. ", 12)
	var proposals []approval.Proposal
	for index := range 8 {
		proposals = append(proposals, outcomeProposal(t, "Save the note "+string(rune('A'+index)), outcome, false))
	}
	widget, _ := h.proposeChanges(proposals...)
	id := decodeChanges(t, widget.data).ID

	h.push(widgetTap(1, 101, widget.messageID, keepTap(id, strings.Repeat("1", 8))))
	h.wantWidgetAnswer(1)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	list := decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if len(list.Changes) != 8 || list.Changes[7].State != "d" || !strings.Contains(widget.data, "Save the note H") {
		t.Fatalf("resolved list = %+v", list)
	}
	reply := h.nextMessage()
	if reply.replyTo != widget.messageID || !strings.Contains(reply.text, "1. Save the note A\nDone. "+strings.TrimSpace(outcome)[:60]) || !strings.Contains(reply.text, "8. Save the note H") {
		t.Fatalf("outcome reply = %+v", reply)
	}

	h.push(widgetTap(2, 101, widget.messageID, keepTap(id, strings.Repeat("1", 8))))
	h.wantWidgetAnswer(2)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	h.waitOffset(3)
	h.wantNoMessage()
}

// An outcome too long for one message is replied in as many messages as it needs when Telegram
// refuses the edit for good.
func TestChangesWidgetLongOutcomeRepliesAreSplit(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	first := strings.Repeat("a", 2300)
	second := strings.Repeat("b", 2300)
	widget, _ := h.proposeChanges(outcomeProposal(t, first, "Saved A.", false), outcomeProposal(t, second, "Saved B.", false))
	id := decodeChanges(t, widget.data).ID

	h.mu.Lock()
	h.widgets.editStatus = http.StatusBadRequest
	h.mu.Unlock()
	h.push(widgetTap(1, 101, widget.messageID, keepTap(id, "11")))
	h.wantWidgetAnswer(1)
	var text strings.Builder
	for text.Len() < 4600 {
		message := h.nextMessage()
		if utf8.RuneCountInString(message.text) > 4096 {
			t.Fatalf("a reply of %d characters does not fit a message", utf8.RuneCountInString(message.text))
		}
		text.WriteString(message.text)
	}
	if !strings.Contains(text.String(), first) || !strings.Contains(text.String(), second) {
		t.Fatalf("the replies lost a title: %d characters", text.Len())
	}
}

// A keep tap continues the request in the tab of the proposal, and the list still works after
// widgets are switched off.
func TestChangesWidgetKeepTapContinuesInItsTabAndWorksWithWidgetsOff(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{topics: true})
	h.setWidgets("alice", true)

	h.push(tabMessage(1, 50, "set up weekly reports"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	for index, prompt := range []string{"Summarise report one.", "Summarise report two."} {
		arguments, err := json.Marshal(map[string]string{"name": "Report " + strconv.Itoa(index+1), "cron": "0 9 * * 1", "prompt": prompt})
		if err != nil {
			t.Fatal(err)
		}
		if proposed := toolCaller(t, chat)("schedule_propose", string(arguments)); proposed.IsError {
			t.Fatalf("schedule_propose = %+v", proposed)
		}
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	if reply := h.nextMessage(); reply.text != "proposed" || reply.threadID != 50 {
		t.Fatalf("reply = %+v", reply)
	}
	h.waitState(1, "delivered")
	widget := h.nextWidget()
	if widget.threadID != 50 {
		t.Fatalf("widget = %+v, want it in tab 50", widget)
	}
	h.setWidgets("alice", false)

	h.push(widgetTap(2, 101, widget.messageID, keepTap(decodeChanges(t, widget.data).ID, "01")))
	h.wantWidgetAnswer(2)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	continued := receive(t, h.ctx, runtime.calls)
	continued.answer <- agentruntime.RunResult{Response: "It is set up."}
	if reply := h.nextMessage(); reply.threadID != 50 || reply.replyTo != widget.messageID || reply.text != "It is set up." {
		t.Fatalf("continuation reply = %+v, want it in tab 50", reply)
	}
	h.waitState(2, "delivered")
}

// A list whose delivery failed is recorded with the kind of what is finally sent, and a proposal
// already partly delivered as text stays text.
func TestChangesWidgetKindFollowsDelivery(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{approvalSweep: 50 * time.Millisecond})
	h.mu.Lock()
	h.widgets.sendStatus = http.StatusInternalServerError
	h.mu.Unlock()
	records, err := h.approvals.Offer(h.ctx, "alice", 101, 0, "Weekly review", 10*time.Minute, []approval.Proposal{
		outcomeProposal(t, "Save the note A", "Saved.", false), outcomeProposal(t, "Save the note B", "Saved.", false),
	})
	if err == nil {
		t.Fatal("an offer whose widget failed was delivered")
	}
	if h.countRows(`SELECT count(*) FROM approvals WHERE bundle_id = ? AND message_id IS NULL AND widget_kind = ''`, records[0].BundleID) != 2 {
		t.Fatal("a failed delivery left the list shown or flagged")
	}
	h.mu.Lock()
	h.widgets.sendStatus = 0
	h.mu.Unlock()
	if widget := h.nextWidget(); !h.widgetKindIs(widget.messageID, "changes") || h.countRows(`SELECT count(*) FROM approvals WHERE widget_kind = 'changes'`) != 2 {
		t.Fatal("the resent list is not recorded as the changes kind")
	}

	h.setWidgets("alice", true)
	partly, err := h.approvals.ProposeAll(h.ctx, "alice", []approval.Proposal{outcomeProposal(t, "Save the note C", "Saved.", false), outcomeProposal(t, "Save the note D", "Saved.", false)})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE approvals SET chat_id = 101, message_id = 9001 WHERE id = ?`, partly[0].ID)
	attempts := h.sendAttempts()
	if err := h.approvals.AfterDelivery(h.ctx, "alice", 101); err != nil {
		t.Fatal(err)
	}
	message := h.nextMessage()
	if len(buttonData(message.keyboard)) != 2 || h.sendAttempts() != attempts {
		t.Fatalf("a proposal partly delivered as text = %+v, %d widgets tried", message, h.sendAttempts()-attempts)
	}
	h.waitMessageRecorded(message.messageID)
	if h.widgetKindIs(message.messageID, "changes") || h.widgetKindIs(message.messageID, "proposal") {
		t.Fatal("the rest of a proposal delivered as text is recorded as a widget")
	}
}

// previewBook is an executor whose proposals can be previewed.
type previewBook struct{ outcomeBook }

func (previewBook) ActionTools() []string { return []string{"preview_write"} }

func (previewBook) Previewable(context.Context, approval.Record) bool { return true }

func (previewBook) Preview(_ context.Context, record approval.Record) (approval.Preview, error) {
	return approval.Preview{Name: record.Summary, Text: "Would save it."}, nil
}

// A Preview tap on a previewable list previews each change under the widget and answers; the
// widget is not edited, and its controls, which the view gives back itself, stay on the list.
func TestChangesWidgetPreviewsAPreviewableList(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})
	if err := h.approvals.RegisterExecutor(previewBook{}); err != nil {
		t.Fatal(err)
	}
	preview := func(summary string) approval.Proposal {
		proposal := outcomeProposal(t, summary, "Saved.", false)
		proposal.Tool = "preview_write"
		return proposal
	}
	widget, ids := h.proposeChanges(preview("Save the note A"), preview("Save the note B"))
	list := decodeChanges(t, widget.data)
	if !list.Preview {
		t.Fatalf("a previewable list offers no Preview: %+v", list)
	}

	h.push(widgetTap(1, 101, widget.messageID, "approval:preview:"+list.ID))
	if answer := h.wantWidgetAnswer(1); !strings.HasPrefix(answer, "Running previews of 2 items") {
		t.Fatalf("preview answered %q", answer)
	}
	for range 2 {
		reply := h.nextMessage()
		if reply.replyTo != widget.messageID || !strings.Contains(reply.text, "Would save it.") {
			t.Fatalf("preview report = %+v", reply)
		}
	}
	h.wantStatuses(ids, "pending", "pending")
	select {
	case edit := <-h.widgets.edits:
		t.Fatalf("a preview edited the widget: %+v", edit)
	default:
	}
}
