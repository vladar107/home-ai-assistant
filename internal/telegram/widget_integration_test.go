package telegram

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type sentWidget struct {
	chatID, messageID, threadID int64
	html, caption, data         string
}

// widgetEdit is an edit of a widget message: its caption, as the user reads it, or its data.
type widgetEdit struct {
	method            string
	chatID, messageID int64
	caption, data     string
}

type widgetAnswer struct{ id, data string }

// widgetSimulator stands for the widget messages the bot sent: their captions and data, the
// answers to taps, and the failures a test asks for.
type widgetSimulator struct {
	ids      map[int64]bool
	captions map[int64]string
	data     map[int64]string
	// sendStatus refuses sendWidget and answerStatus answerWidgetQuery when set.
	sendStatus, answerStatus int
	// answerGate holds Telegram's reply to an answer until it is closed.
	answerGate chan struct{}
	sent       chan sentWidget
	edits      chan widgetEdit
	answers    chan widgetAnswer
}

func newWidgetSimulator() widgetSimulator {
	return widgetSimulator{
		ids: make(map[int64]bool), captions: make(map[int64]string), data: make(map[int64]string),
		sent: make(chan sentWidget, 20), edits: make(chan widgetEdit, 50), answers: make(chan widgetAnswer, 50),
	}
}

func writeRefusal(w http.ResponseWriter, status int, description string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": description})
}

// refuseWidgetText answers a text edit of a widget message the way Telegram does.
func (h *conversationHarness) refuseWidgetText(w http.ResponseWriter, messageID int64) bool {
	h.mu.Lock()
	widget := h.widgets.ids[messageID]
	h.mu.Unlock()
	if !widget {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	writeRefusal(w, http.StatusBadRequest, "Bad Request: there is no text in the message to edit")
	return true
}

// shownCaption is a caption as the user reads it.
func shownCaption(caption, parseMode string) string {
	if parseMode == "HTML" {
		return captionText(caption)
	}
	return caption
}

// serveWidgetMethod answers the widget methods; a nil result means the answer was already written.
func (h *conversationHarness) serveWidgetMethod(w http.ResponseWriter, r *http.Request) (any, bool) {
	method := filepath.Base(r.URL.Path)
	switch method {
	case "sendWidget", "editMessageCaption", "editMessageWidgetData", "answerWidgetQuery":
	default:
		return nil, false
	}
	if method == "answerWidgetQuery" {
		select {
		case h.widgets.answers <- widgetAnswer{id: r.Form.Get("widget_query_id"), data: r.Form.Get("data")}:
		case <-r.Context().Done():
			return nil, true
		}
		h.mu.Lock()
		gate, status := h.widgets.answerGate, h.widgets.answerStatus
		h.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return nil, true
			}
		}
		if status != 0 {
			writeRefusal(w, status, "Bad Request: query is too old and response timeout expired or query ID is invalid")
			return nil, true
		}
		return true, true
	}
	chatID, err := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
	if err != nil {
		h.t.Error(err)
	}
	if method == "sendWidget" {
		threadID := h.threadOf(r.Form.Get("message_thread_id"))
		if h.refuseGoneThread(w, r, method, threadID) {
			return nil, true
		}
		caption := shownCaption(r.Form.Get("caption"), r.Form.Get("parse_mode"))
		if r.Form.Has("reply_markup") || r.Form.Get("allow_queries") != "true" || len(r.Form.Get("html")) > 32768 ||
			len(r.Form.Get("chat_data")) > widgetDataLimit || utf16Length(caption) > captionLimit {
			h.t.Errorf("sendWidget must carry no keyboard, allow queries, and keep to the limits: %v", r.Form)
		}
		h.mu.Lock()
		if status := h.widgets.sendStatus; status != 0 {
			h.mu.Unlock()
			writeRefusal(w, status, "Bad Request: widgets are not available in this chat")
			return nil, true
		}
		h.sentCount++
		messageID := sentMessageID(h.sentCount)
		h.widgets.ids[messageID], h.widgets.captions[messageID], h.widgets.data[messageID] = true, caption, r.Form.Get("chat_data")
		h.mu.Unlock()
		h.widgets.sent <- sentWidget{chatID: chatID, messageID: messageID, threadID: threadID, html: r.Form.Get("html"), caption: caption, data: r.Form.Get("chat_data")}
		return Message{MessageID: messageID, MessageThreadID: threadID, Chat: Chat{ID: chatID, Type: ChatTypePrivate}}, true
	}
	messageID, err := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
	if err != nil {
		h.t.Error(err)
	}
	edit := widgetEdit{method: method, chatID: chatID, messageID: messageID}
	h.mu.Lock()
	known := h.widgets.ids[messageID]
	unchanged := false
	if method == "editMessageCaption" {
		edit.caption = shownCaption(r.Form.Get("caption"), r.Form.Get("parse_mode"))
		unchanged = h.widgets.captions[messageID] == edit.caption
		h.widgets.captions[messageID] = edit.caption
	} else {
		edit.data = r.Form.Get("chat_data")
		unchanged = h.widgets.data[messageID] == edit.data
		h.widgets.data[messageID] = edit.data
	}
	h.mu.Unlock()
	if !known || utf16Length(edit.caption) > captionLimit || len(edit.data) > widgetDataLimit {
		h.t.Errorf("%s of message %d must be of a widget and keep to the limits: %v", method, messageID, r.Form)
	}
	h.widgets.edits <- edit
	if unchanged {
		writeRefusal(w, http.StatusBadRequest, "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message")
		return nil, true
	}
	return true, true
}

func widgetQueryID(updateID int64) string { return fmt.Sprintf("wq-%d", updateID) }

// widgetTap is a tap in a widget, which names no tab and carries no message.
func widgetTap(updateID, userID, messageID int64, data string) Update {
	return Update{UpdateID: updateID, WidgetQuery: &WidgetQuery{
		ID: widgetQueryID(updateID), From: &User{ID: userID, FirstName: "Test"}, Chat: &Chat{ID: userID, Type: ChatTypePrivate},
		MessageID: messageID, ChatType: ChatTypePrivate, Data: data,
	}}
}

// nextWidget takes the next widget the bot sent, once the approval service recorded it, so a tap
// never comes before the service knows its message.
func (h *conversationHarness) nextWidget() sentWidget {
	h.t.Helper()
	widget := receive(h.t, h.ctx, h.widgets.sent)
	h.waitMessageRecorded(widget.messageID)
	return widget
}

func (h *conversationHarness) wantWidgetAnswer(updateID int64) string {
	h.t.Helper()
	answer := receive(h.t, h.ctx, h.widgets.answers)
	if answer.id != widgetQueryID(updateID) {
		h.t.Fatalf("answered widget query %+v, want %s", answer, widgetQueryID(updateID))
	}
	return answer.data
}

func (h *conversationHarness) nextWidgetEdit(method string, messageID int64) widgetEdit {
	h.t.Helper()
	edit := receive(h.t, h.ctx, h.widgets.edits)
	if edit.method != method || edit.chatID != 101 || edit.messageID != messageID {
		h.t.Fatalf("widget edit = %+v, want %s of message %d", edit, method, messageID)
	}
	return edit
}

// wantOutcome expects the caption and then the data of a widget to show an outcome, with no actions
// left.
func (h *conversationHarness) wantOutcome(messageID int64, want string) (widgetEdit, widgetData) {
	h.t.Helper()
	caption := h.nextWidgetEdit("editMessageCaption", messageID)
	data := decodeWidget(h.t, h.nextWidgetEdit("editMessageWidgetData", messageID).data)
	if !strings.Contains(caption.caption, want) || !strings.Contains(data.Text, want) || len(data.Actions) != 0 {
		h.t.Fatalf("widget outcome = %q, %+v, want %q and no actions", caption.caption, data, want)
	}
	return caption, data
}

func (h *conversationHarness) setWidgets(principalID string, enabled bool) {
	h.t.Helper()
	if err := h.switches.SetEnabled(h.ctx, principalID, approval.WidgetSwitch, enabled); err != nil {
		h.t.Fatal(err)
	}
}

func (h *conversationHarness) setWidgetAnswer(status int, gate chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.widgets.answerStatus, h.widgets.answerGate = status, gate
}

func decodeWidget(t *testing.T, raw string) widgetData {
	t.Helper()
	var data widgetData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("widget data %q: %v", raw, err)
	}
	return data
}

// widgetActions returns the data of a widget's Confirm, Reject, and Preview actions.
func widgetActions(t *testing.T, raw string) (confirm, reject, preview string) {
	t.Helper()
	data := decodeWidget(t, raw)
	if len(data.Actions) != 3 || data.Actions[0].Label != "Confirm" || data.Actions[1].Label != "Reject" || data.Actions[2].Label != "Preview" ||
		!strings.HasPrefix(data.Actions[0].Data, "approval:confirm:") || !strings.HasPrefix(data.Actions[1].Data, "approval:reject:") ||
		!strings.HasPrefix(data.Actions[2].Data, "approval:preview:") {
		t.Fatalf("widget actions = %+v", data.Actions)
	}
	return data.Actions[0].Data, data.Actions[1].Data, data.Actions[2].Data
}

// proposeJob runs one chat turn of Alice's, in a tab when threadID is set, that proposes a weekly
// job after reading outside content.
func (h *conversationHarness) proposeJob(runtime *controlledRuntime, updateID, threadID int64, name, prompt string) {
	h.t.Helper()
	update := textUpdate(updateID, 101, "set up a weekly job")
	if threadID > 0 {
		update = tabMessage(updateID, threadID, "set up a weekly job")
	}
	h.push(update)
	chat := receive(h.t, h.ctx, runtime.calls)
	readOutside(h.t, chat)
	arguments, err := json.Marshal(map[string]string{"name": name, "cron": "0 9 * * 1", "prompt": prompt})
	if err != nil {
		h.t.Fatal(err)
	}
	if proposed := toolCaller(h.t, chat)("schedule_propose", string(arguments)); proposed.IsError {
		h.t.Fatalf("schedule_propose = %+v", proposed)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	if reply := h.nextMessage(); reply.text != "proposed" || reply.threadID != threadID {
		h.t.Fatalf("reply = %+v, want it in tab %d", reply, threadID)
	}
	h.waitState(updateID, "delivered")
}

func newWidgetHarness(t *testing.T, runtime *controlledRuntime, options harnessOptions) *conversationHarness {
	t.Helper()
	options.schedules = newTestClock(t, mondayMorning)
	options.providers = []tools.Provider{outsideProvider{}}
	h := newConversationHarnessWithOptions(t, runtime, options)
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()
	return h
}

func TestWidgetTemplateFitsTelegramsLimits(t *testing.T) {
	t.Parallel()
	var compressed bytes.Buffer
	writer, err := zlib.NewWriterLevel(&compressed, 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(widgetTemplate)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if len(widgetTemplate) > 32768 || compressed.Len() > 4096 {
		t.Fatalf("widget template is %d bytes, %d compressed; Telegram takes 32768 and 4096", len(widgetTemplate), compressed.Len())
	}
}

// The switch in /settings decides how proposals are sent, and only that: a widget already in the
// chat is still handled after it is switched off. A Confirm tap is answered before the actions run.
func TestWidgetSwitchDecidesHowProposalsAreSent(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})

	settings := h.requestSettings(1, 101)
	if !strings.Contains(settings.text, "Confirmations: buttons") {
		t.Fatalf("settings overview = %s", settings.text)
	}
	page := h.openSettingsPage(2, 101, settings.messageID, "confirmations")
	if !strings.Contains(page.text, WidgetOffLine) || strings.Join(buttonData(page.keyboard), " ") != "widget:on nav:root" {
		t.Fatalf("confirmations page = %+v", page)
	}
	if on := h.press(3, 101, settings.messageID, "widget:on"); !strings.Contains(on.text, WidgetOnLine) || buttonData(on.keyboard)[0] != "widget:off" {
		t.Fatalf("confirmations page after turning the widget on = %+v", on)
	}

	h.proposeJob(runtime, 4, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	if widget.chatID != 101 || widget.html != widgetTemplate || !strings.Contains(widget.caption, `"Weekly tasks"`) || !strings.Contains(widget.caption, "Expires in 10 minutes.") {
		t.Fatalf("widget = %+v", widget)
	}
	if data := decodeWidget(t, widget.data); data.Text != widget.caption {
		t.Fatalf("widget text = %q, want the caption %q", data.Text, widget.caption)
	}
	confirm, _, _ := widgetActions(t, widget.data)

	if off := h.press(5, 101, settings.messageID, "widget:off"); !strings.Contains(off.text, WidgetOffLine) {
		t.Fatalf("confirmations page after turning the widget off = %+v", off)
	}

	gate := make(chan struct{})
	h.setWidgetAnswer(0, gate)
	h.push(widgetTap(6, 101, widget.messageID, confirm))
	if answer := h.wantWidgetAnswer(6); answer != WidgetWorkingAnswer {
		t.Fatalf("confirm tap answer = %q", answer)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("the actions ran before the tap was answered: %d jobs", count)
	}
	close(gate)
	h.wantOutcome(widget.messageID, "Scheduled. First run:")
	h.continueTurn(runtime, 6)
	if status, _ := h.approvalStatus(approvalID(confirm)); status != approval.StatusExecuted {
		t.Fatalf("approval after the confirm tap = %s", status)
	}

	h.proposeJob(runtime, 7, 0, "Daily tasks", "List today's tasks.")
	proposal := h.nextMessage()
	if data := buttonData(proposal.keyboard); len(data) != 3 || !strings.Contains(proposal.text, `"Daily tasks"`) {
		t.Fatalf("proposal with the widget off = %+v", proposal)
	}
	if count := len(h.widgets.sent); count != 0 {
		t.Fatalf("%d widgets sent with the switch off", count)
	}
}

// Preview and Reject work from the widget as their buttons do, and a tap on someone else's proposal
// is answered without touching it.
func TestWidgetPreviewsAndRejects(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue Notion tasks.")
	widget := h.nextWidget()
	confirm, reject, preview := widgetActions(t, widget.data)

	h.push(widgetTap(2, 101, widget.messageID, preview))
	if answer := h.wantWidgetAnswer(2); answer != "Running a preview; its report will follow here. Nothing is saved." {
		t.Fatalf("preview tap answer = %q", answer)
	}
	run := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(run.request.Message, `scheduled job "Weekly tasks"`) {
		t.Fatalf("preview message = %q", run.request.Message)
	}
	run.answer <- agentruntime.RunResult{Response: "Two tasks are overdue.", Final: "Two tasks are overdue."}
	if report := h.nextMessage(); report.replyTo != widget.messageID || !strings.Contains(report.text, "Two tasks are overdue.") {
		t.Fatalf("preview report = %+v", report)
	}

	for index, tap := range []Update{
		widgetTap(3, 202, widget.messageID, confirm),
		widgetTap(4, 101, widget.messageID, "approval:confirm:not-an-id"),
	} {
		h.push(tap)
		if answer := h.wantWidgetAnswer(tap.UpdateID); answer != WidgetIgnoredAnswer {
			t.Fatalf("tap %d answer = %q", index, answer)
		}
	}
	h.waitOffset(5)
	select {
	case edit := <-h.widgets.edits:
		t.Fatalf("an ignored tap edited the widget: %+v", edit)
	default:
	}
	if status, _ := h.approvalStatus(approvalID(confirm)); status != approval.StatusPending {
		t.Fatalf("approval after ignored taps = %s", status)
	}

	h.push(widgetTap(5, 101, widget.messageID, reject))
	if answer := h.wantWidgetAnswer(5); answer != WidgetWorkingAnswer {
		t.Fatalf("reject tap answer = %q", answer)
	}
	h.wantOutcome(widget.messageID, "Rejected; nothing was written.")
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("jobs after reject = %d", count)
	}
}

// A tap names no tab, so it runs in the tab the proposal was made in, and the conversation goes on
// there.
func TestWidgetTapContinuesInTheProposalsTab(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{topics: true})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 50, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	if widget.threadID != 50 {
		t.Fatalf("widget = %+v, want it in tab 50", widget)
	}
	confirm, _, _ := widgetActions(t, widget.data)

	h.push(widgetTap(2, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(2)
	h.wantOutcome(widget.messageID, "Scheduled. First run:")
	continued := receive(t, h.ctx, runtime.calls)
	continued.answer <- agentruntime.RunResult{Response: "It is set up."}
	if reply := h.nextMessage(); reply.threadID != 50 || reply.replyTo != widget.messageID || reply.text != "It is set up." {
		t.Fatalf("continuation reply = %+v, want it in tab 50", reply)
	}
	h.waitState(2, "delivered")
}

// A button press and a later tap on the same proposal act once: the tap only shows the outcome
// again, which changes nothing and sends nothing.
func TestButtonPressThenWidgetTapActOnce(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	confirm, _, _ := widgetActions(t, widget.data)

	h.push(callbackUpdate(2, 101, widget.messageID, confirm))
	h.wantAnswer(2)
	h.wantOutcome(widget.messageID, "Scheduled. First run:")
	h.continueTurn(runtime, 2)

	h.push(widgetTap(3, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(3)
	h.wantOutcome(widget.messageID, "Scheduled. First run:")
	h.waitOffset(4)
	expectNoRuntimeCall(t, runtime, 100*time.Millisecond)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 1 {
		t.Fatalf("jobs = %d, want 1", count)
	}
	if count := len(h.messages); count != 0 {
		t.Fatalf("a repeated outcome sent %d messages", count)
	}
}

// A proposal is a widget only while its text fits a caption. An outcome that no longer fits is
// clipped in the caption and sent in full as a reply. An answer Telegram refuses does not stop the
// tap, and a widget Telegram refuses is sent with buttons.
func TestWidgetCaptionLimits(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	probe := "List my overdue tasks."
	h.proposeJob(runtime, 1, 0, "Job one", probe)
	widget := h.nextWidget()
	frame := utf16Length(widget.caption) - len(probe)
	_, reject, _ := widgetActions(t, widget.data)
	h.setWidgetAnswer(http.StatusBadRequest, nil)
	h.push(widgetTap(2, 101, widget.messageID, reject))
	h.wantWidgetAnswer(2)
	h.wantOutcome(widget.messageID, "Rejected; nothing was written.")
	h.setWidgetAnswer(0, nil)

	prompt := strings.TrimSpace(strings.Repeat("List the tasks. ", 80))[:captionLimit-frame-2]
	h.proposeJob(runtime, 3, 0, "Job two", prompt)
	widget = h.nextWidget()
	if utf16Length(widget.caption) != captionLimit-2 {
		t.Fatalf("caption is %d long, want %d", utf16Length(widget.caption), captionLimit-2)
	}
	confirm, _, _ := widgetActions(t, widget.data)
	h.push(widgetTap(4, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(4)
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	if utf16Length(caption.caption) > captionLimit || !strings.Contains(caption.caption, "…") || !strings.Contains(caption.caption, "Scheduled. First run:") {
		t.Fatalf("clipped caption = %q", caption.caption)
	}
	full := h.nextMessage()
	if full.replyTo != widget.messageID || !strings.Contains(full.text, prompt) || !strings.Contains(full.text, "Scheduled. First run:") {
		t.Fatalf("full outcome = %+v", full)
	}
	if data := decodeWidget(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data); len(data.Actions) != 0 || !strings.Contains(data.Text, "Scheduled. First run:") {
		t.Fatalf("widget data after a long outcome = %+v", data)
	}
	h.continueTurn(runtime, 4)

	h.proposeJob(runtime, 5, 0, "Job three", strings.Repeat("x", captionLimit))
	if proposal := h.nextMessage(); len(buttonData(proposal.keyboard)) != 3 || !strings.Contains(proposal.text, "Job three") {
		t.Fatalf("proposal too long for a caption = %+v", proposal)
	}

	h.mu.Lock()
	h.widgets.sendStatus = http.StatusBadRequest
	h.mu.Unlock()
	h.proposeJob(runtime, 6, 0, "Job four", "List my tasks.")
	if proposal := h.nextMessage(); len(buttonData(proposal.keyboard)) != 3 || !strings.Contains(proposal.text, "Job four") {
		t.Fatalf("proposal after a refused widget = %+v", proposal)
	}
	if count := len(h.widgets.sent); count != 0 {
		t.Fatalf("%d widgets sent past the caption limit or a refusal", count)
	}
}

// The model withdraws part of a widget's batch: the widget keeps its actions for the rest, and
// loses them once nothing is left.
func TestWidgetShowsWithdrawals(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	h.push(textUpdate(1, 101, "set up two weekly reports"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	for _, name := range []string{"Report A", "Report B"} {
		if proposed := toolCaller(t, chat)("schedule_propose", `{"name":"`+name+`","cron":"0 9 * * 1","prompt":"Summarise `+name+`."}`); proposed.IsError {
			t.Fatalf("schedule_propose = %+v", proposed)
		}
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	widget := h.nextWidget()
	widgetActions(t, widget.data)

	h.push(textUpdate(2, 101, "drop the second one"))
	chat = receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"items":[2]}`); withdrawn.IsError {
		t.Fatalf("proposal_withdraw = %+v", withdrawn)
	}
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	data := h.nextWidgetEdit("editMessageWidgetData", widget.messageID)
	if !strings.Contains(caption.caption, "Withdrawn by the assistant") || !strings.Contains(decodeWidget(t, data.data).Text, "Withdrawn by the assistant") {
		t.Fatalf("widget after a withdrawal = %q, %q", caption.caption, data.data)
	}
	widgetActions(t, data.data)
	chat.answer <- agentruntime.RunResult{Response: "withdrawn"}
	h.wantReply(101, 2, "withdrawn")
	h.waitState(2, "delivered")

	h.push(textUpdate(3, 101, "forget all of it"))
	chat = receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"all":true}`); withdrawn.IsError {
		t.Fatalf("withdraw all = %+v", withdrawn)
	}
	h.wantOutcome(widget.messageID, "Withdrawn by the assistant; nothing was written.")
	chat.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 3, "done")
	h.waitState(3, "delivered")
}

// Serial: waits out a 2 s approval lifetime with a 100 ms sweep.
func TestWidgetShowsExpiry(t *testing.T) {
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{approvalLifetime: 2 * time.Second, approvalSweep: 100 * time.Millisecond})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	confirm, _, _ := widgetActions(t, widget.data)
	h.wantOutcome(widget.messageID, "Expired; nothing was written.")

	h.push(widgetTap(2, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(2)
	h.wantOutcome(widget.messageID, "Expired; nothing was written.")
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("an expired widget proposal created %d jobs", count)
	}
}

// A widget keeps as much of a text in another script as fits, however many bytes a character takes.
func TestWidgetDataKeepsTextInAnyScript(t *testing.T) {
	t.Parallel()
	actions := []widgetAction{{Label: "Confirm", Data: "approval:confirm:0000000000000000"}}
	for name, character := range map[string]string{"two bytes": "ж", "three bytes": "界", "four bytes": "🙂"} {
		raw, fits := encodeWidgetData(strings.Repeat(character, 900), actions)
		if !fits || len(raw) > widgetDataLimit {
			t.Fatalf("%s: fits = %t, %d bytes", name, fits, len(raw))
		}
		kept := utf8.RuneCountInString(decodeWidget(t, raw).Text)
		if most := (widgetDataLimit - 80) / len(character); kept < most*9/10 {
			t.Fatalf("%s: kept %d characters, want about %d", name, kept, most)
		}
		caption, clipped := widgetCaption(strings.Repeat(character, 2000))
		if units := utf16Length(caption); !clipped || units > captionLimit || units < captionLimit*9/10 {
			t.Fatalf("%s: caption of %d units, clipped = %t", name, units, clipped)
		}
	}
}

// A tap acts only on the message its proposal is shown in, and is taken from a chat Telegram names
// by its type alone.
func TestWidgetTapMustComeFromTheProposalsMessage(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	confirm, _, _ := widgetActions(t, widget.data)

	h.push(widgetTap(2, 101, widget.messageID+50, confirm))
	if answer := h.wantWidgetAnswer(2); answer != WidgetIgnoredAnswer {
		t.Fatalf("answer to a tap from another message = %q", answer)
	}
	h.waitOffset(3)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("a tap from another message scheduled %d jobs", count)
	}

	tap := widgetTap(3, 101, widget.messageID, confirm)
	tap.WidgetQuery.Chat, tap.WidgetQuery.ChatType = nil, "sender"
	h.push(tap)
	if answer := h.wantWidgetAnswer(3); answer != WidgetWorkingAnswer {
		t.Fatalf("answer to a tap without a chat = %q", answer)
	}
	h.wantOutcome(widget.messageID, "Scheduled. First run:")
	h.continueTurn(runtime, 3)
}
