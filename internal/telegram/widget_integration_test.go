package telegram

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
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
	// sendStatus refuses sendWidget, editStatus editMessageCaption and editMessageWidgetData, and
	// answerStatus answerWidgetQuery when set.
	sendStatus, editStatus, answerStatus int
	// sendAttempts counts the sendWidget calls, and textEdits the text edits of widget messages,
	// which Telegram refuses.
	sendAttempts, textEdits int
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
	if widget {
		h.widgets.textEdits++
	}
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
		h.widgets.sendAttempts++
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
	if status := h.widgets.editStatus; status != 0 {
		h.mu.Unlock()
		writeRefusal(w, status, "Bad Request: message to edit not found")
		return nil, true
	}
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

// wantOutcome expects the caption and then the data of a widget to show an outcome: a caption of one
// line, and the status apart from the text, with no actions left.
func (h *conversationHarness) wantOutcome(messageID int64, status string) (widgetEdit, widgetData) {
	h.t.Helper()
	caption := h.nextWidgetEdit("editMessageCaption", messageID)
	data := decodeWidget(h.t, h.nextWidgetEdit("editMessageWidgetData", messageID).data)
	if strings.Contains(caption.caption, "\n") || !strings.Contains(data.Status, status) || strings.Contains(data.Text, status) || len(data.Actions) != 0 {
		h.t.Fatalf("widget outcome = %q, %+v, want the status %q apart and no actions", caption.caption, data, status)
	}
	return caption, data
}

// widgetTextEdits counts the text edits of widget messages, each of which Telegram refused.
func (h *conversationHarness) widgetTextEdits() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.widgets.textEdits
}

func (h *conversationHarness) wantNoWidgetTextEdit() {
	h.t.Helper()
	if count := h.widgetTextEdits(); count != 0 {
		h.t.Fatalf("%d text edits of a widget recorded as one", count)
	}
}

// widgetFlagged reports whether the records shown in a message say it is a widget.
func (h *conversationHarness) widgetFlagged(messageID int64) bool {
	h.t.Helper()
	return h.countRows(`SELECT count(*) FROM approvals WHERE message_id = ? AND widget_kind = 'proposal'`, messageID) > 0
}

// waitUntil polls until done holds.
func (h *conversationHarness) waitUntil(what string, done func() bool) {
	h.t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !done() {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatalf("never saw %s", what)
		}
	}
}

func (h *conversationHarness) setWidgets(principalID string, enabled bool) {
	h.t.Helper()
	if err := h.switches.SetEnabled(h.ctx, principalID, integration.WidgetsSwitch, enabled); err != nil {
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

var updateWidgets = flag.Bool("update", false, "rewrite the golden widget templates in testdata/widgets")

// Every kind's composed template is pinned in testdata/widgets, so a change to what a widget runs is
// a diff a reviewer reads, and fits Telegram's limits. Run go test ./internal/telegram -run
// TestWidgetTemplates -update after an intended change.
func TestWidgetTemplatesMatchTheirGoldenFilesAndFitTelegramsLimits(t *testing.T) {
	t.Parallel()
	for kind, template := range widgetTemplates {
		path := filepath.Join("testdata", "widgets", string(kind)+".html")
		if *updateWidgets {
			if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if golden, err := os.ReadFile(path); err != nil {
			t.Fatalf("read %s: %v; run with -update to create it", path, err)
		} else if template != string(golden) {
			t.Errorf("the %s widget's template changed; review the change and run with -update if it is intended", kind)
		}
		var compressed bytes.Buffer
		writer, err := zlib.NewWriterLevel(&compressed, 9)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(template)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if len(template) > 32768 || compressed.Len() > 4096 {
			t.Fatalf("the %s widget's template is %d bytes, %d compressed; Telegram takes 32768 and 4096", kind, len(template), compressed.Len())
		}
	}
	goldens, err := os.ReadDir(filepath.Join("testdata", "widgets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, golden := range goldens {
		if _, found := widgetTemplates[widgetKind(strings.TrimSuffix(golden.Name(), ".html"))]; !found {
			t.Errorf("testdata/widgets/%s belongs to no widget kind; remove it", golden.Name())
		}
	}
}

// The switch in /settings decides how proposals are sent, and only that: a widget already in the
// chat is still handled after it is switched off. A Confirm tap is answered before the actions run.
func TestWidgetSwitchDecidesHowProposalsAreSent(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})

	settings := h.requestSettings(1, 101)
	if !strings.Contains(settings.text, "Widgets: off") {
		t.Fatalf("settings overview = %s", settings.text)
	}
	page := h.openSettingsPage(2, 101, settings.messageID, "widgets")
	if !strings.Contains(page.text, localetest.English(WidgetOffLine)) || strings.Join(buttonData(page.keyboard), " ") != "widget:on nav:root" {
		t.Fatalf("widgets page = %+v", page)
	}
	if on := h.press(3, 101, settings.messageID, "widget:on"); !strings.Contains(on.text, localetest.English(WidgetOnLine)) || buttonData(on.keyboard)[0] != "widget:off" {
		t.Fatalf("widgets page after turning widgets on = %+v", on)
	}

	h.proposeJob(runtime, 4, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	if widget.chatID != 101 || widget.html != widgetTemplates[proposalWidget] || strings.Contains(widget.caption, "\n") || !strings.Contains(widget.caption, `"Weekly tasks"`) ||
		strings.Contains(widget.caption, "List my overdue tasks.") || strings.Contains(widget.caption, "Expires") {
		t.Fatalf("widget = %+v, want a caption of one line naming the job", widget)
	}
	if data := decodeWidget(t, widget.data); !strings.HasPrefix(data.Text, widget.caption+"\n") || !strings.Contains(data.Text, "List my overdue tasks.") ||
		strings.Contains(data.Text, "Expires") || data.Status != "Expires in 10 minutes." {
		t.Fatalf("widget data = %+v, want the whole proposal with its status apart", data)
	}
	if !h.widgetFlagged(widget.messageID) {
		t.Fatal("the widget's records do not say it is a widget")
	}
	confirm, _, _ := widgetActions(t, widget.data)

	if off := h.press(5, 101, settings.messageID, "widget:off"); !strings.Contains(off.text, localetest.English(WidgetOffLine)) {
		t.Fatalf("widgets page after turning widgets off = %+v", off)
	}

	gate := make(chan struct{})
	h.setWidgetAnswer(0, gate)
	h.push(widgetTap(6, 101, widget.messageID, confirm))
	if answer := h.wantWidgetAnswer(6); answer != localetest.English(WidgetWorkingAnswer) {
		t.Fatalf("confirm tap answer = %q", answer)
	}
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("the actions ran before the tap was answered: %d jobs", count)
	}
	close(gate)
	if caption, _ := h.wantOutcome(widget.messageID, "Scheduled. First run:"); caption.caption != widget.caption+" — done" {
		t.Fatalf("caption after the confirm tap = %q", caption.caption)
	}
	h.wantNoWidgetTextEdit()
	h.continueTurn(runtime, 6)
	if status, _ := h.approvalStatus(approvalID(confirm)); status != approval.StatusExecuted {
		t.Fatalf("approval after the confirm tap = %s", status)
	}

	h.proposeJob(runtime, 7, 0, "Daily tasks", "List today's tasks.")
	proposal := h.nextMessage()
	if data := buttonData(proposal.keyboard); len(data) != 3 || !strings.Contains(proposal.text, `"Daily tasks"`) || !strings.Contains(proposal.text, "Expires in 10 minutes.") {
		t.Fatalf("proposal with the widget off = %+v", proposal)
	}
	h.waitMessageRecorded(proposal.messageID)
	if h.widgetFlagged(proposal.messageID) {
		t.Fatal("a proposal sent with buttons is recorded as a widget")
	}
	if count := len(h.widgets.sent); count != 0 {
		t.Fatalf("%d widgets sent with the switch off", count)
	}
}

// The Widgets page replaced the Confirmations page: a press of the old page's button on a settings
// message already in the chat shows the overview, app_help answers on widgets and no longer on
// confirmations, and /privacy names widgets while they are on.
func TestWidgetsSettingsPageReplacesConfirmations(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})

	settings := h.requestSettings(1, 101)
	if old := h.press(2, 101, settings.messageID, "nav:confirmations"); old.text != settings.text || strings.Join(buttonData(old.keyboard), " ") != strings.Join(buttonData(settings.keyboard), " ") {
		t.Fatalf("old confirmations button = %+v, want the overview %+v", old, settings)
	}
	h.push(textUpdate(3, 101, "/privacy"))
	if reply := h.nextMessage(); strings.Contains(reply.text, localetest.English(WidgetPrivacyLine)) {
		t.Fatalf("privacy with widgets off = %s", reply.text)
	}
	if on := h.press(4, 101, settings.messageID, "widget:on"); !strings.HasPrefix(on.text, crumbs(context.Background(), "Widgets")) || !strings.Contains(on.text, localetest.English(WidgetOnLine)) {
		t.Fatalf("widgets page after turning widgets on = %+v", on)
	}
	h.push(textUpdate(5, 101, "/privacy"))
	if reply := h.nextMessage(); !strings.Contains(reply.text, privacyItem+localetest.English(WidgetPrivacyLine)) {
		t.Fatalf("privacy with widgets on = %s", reply.text)
	}

	h.push(textUpdate(6, 101, "how do widgets work?"))
	call := receive(t, h.ctx, runtime.calls)
	if help := h.callTool(call, "app_help", `{"topic":"widgets"}`); help.IsError || !strings.Contains(help.Text, `press "Widgets"`) || !strings.Contains(help.Text, localetest.English(WidgetOnLine)) {
		t.Fatalf("widgets help = %+v", help)
	}
	if help := h.callTool(call, "app_help", `{"topic":"confirmations"}`); !help.IsError || !strings.Contains(help.Text, "topic must be one of") {
		t.Fatalf("confirmations help = %+v", help)
	}
	call.answer <- agentruntime.RunResult{Response: "They show proposals."}
	h.wantReply(101, 6, "They show proposals.")
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
		if answer := h.wantWidgetAnswer(tap.UpdateID); answer != localetest.English(WidgetIgnoredAnswer) {
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
	if answer := h.wantWidgetAnswer(5); answer != localetest.English(WidgetWorkingAnswer) {
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

// A proposal is a widget only when its whole text fits the widget's data with its actions, counted
// in bytes, so a shorter text in a script that takes several bytes a character gets buttons too. An
// answer Telegram refuses does not stop the tap. A widget Telegram refuses is sent with buttons and
// recorded as a text message, whose edits are text edits.
func TestWidgetIsSentOnlyWhenItsTextFitsWhole(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	probe := "List my overdue tasks."
	h.proposeJob(runtime, 1, 0, "Job one", probe)
	widget := h.nextWidget()
	room := widgetDataLimit - (len(widget.data) - len(probe))
	_, reject, _ := widgetActions(t, widget.data)
	h.setWidgetAnswer(http.StatusBadRequest, nil)
	h.push(widgetTap(2, 101, widget.messageID, reject))
	h.wantWidgetAnswer(2)
	h.wantOutcome(widget.messageID, "Rejected; nothing was written.")
	h.setWidgetAnswer(0, nil)

	// No spaces, so no trimming shortens a prompt cut anywhere.
	latin := strings.Repeat("abcdefghij", 100)
	h.proposeJob(runtime, 3, 0, "Job two", latin[:room])
	widget = h.nextWidget()
	if len(widget.data) != widgetDataLimit || !strings.Contains(decodeWidget(t, widget.data).Text, latin[:room]) {
		t.Fatalf("widget filling the data = %d bytes, %q", len(widget.data), widget.data)
	}
	_, reject, _ = widgetActions(t, widget.data)
	h.push(widgetTap(4, 101, widget.messageID, reject))
	h.wantWidgetAnswer(4)
	h.wantOutcome(widget.messageID, "Rejected; nothing was written.")

	h.mu.Lock()
	attempts := h.widgets.sendAttempts
	h.mu.Unlock()
	h.proposeJob(runtime, 5, 0, "Job six", latin[:room+1])
	if proposal := h.nextMessage(); len(buttonData(proposal.keyboard)) != 3 || !strings.Contains(proposal.text, latin[:room+1]) {
		t.Fatalf("proposal a byte too long for a widget = %+v", proposal)
	}
	var cyrillic strings.Builder
	for _, r := range strings.Repeat("Список задач на неделю. ", 60) {
		if cyrillic.Len() > room {
			break
		}
		cyrillic.WriteRune(r)
	}
	if utf8.RuneCountInString(cyrillic.String()) > room*2/3 {
		t.Fatalf("the multi-byte prompt has %d characters, want far fewer than the %d bytes of room", utf8.RuneCountInString(cyrillic.String()), room)
	}
	h.proposeJob(runtime, 6, 0, "Job ten", cyrillic.String())
	if proposal := h.nextMessage(); len(buttonData(proposal.keyboard)) != 3 || !strings.Contains(proposal.text, strings.TrimSpace(cyrillic.String())) {
		t.Fatalf("multi-byte proposal too long for a widget = %+v", proposal)
	}
	h.mu.Lock()
	tried := h.widgets.sendAttempts - attempts
	h.widgets.sendStatus = http.StatusBadRequest
	h.mu.Unlock()
	if tried != 0 {
		t.Fatalf("%d widgets tried for proposals that do not fit one", tried)
	}

	h.proposeJob(runtime, 7, 0, "Job ace", "List my tasks.")
	proposal := h.nextMessage()
	if len(buttonData(proposal.keyboard)) != 3 || !strings.Contains(proposal.text, "Job ace") || !strings.Contains(proposal.text, "Expires in 10 minutes.") {
		t.Fatalf("proposal after a refused widget = %+v", proposal)
	}
	h.waitMessageRecorded(proposal.messageID)
	if h.widgetFlagged(proposal.messageID) {
		t.Fatal("a refused widget is recorded as one")
	}
	_, rejectButton := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(8, 101, proposal.messageID, rejectButton))
	h.wantAnswer(8)
	for {
		edit := receive(t, h.ctx, h.edits)
		if edit.messageID != proposal.messageID {
			continue
		}
		if !strings.Contains(edit.text, "Rejected; nothing was written.") || len(edit.keyboard) != 0 {
			t.Fatalf("outcome of a proposal sent with buttons = %+v", edit)
		}
		break
	}
	h.wantNoWidgetTextEdit()
}

// proposeReports runs one chat turn of Alice's that proposes a weekly job for each prompt, named
// Report and its number.
func (h *conversationHarness) proposeReports(runtime *controlledRuntime, updateID int64, prompts ...string) {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, "set up weekly reports"))
	chat := receive(h.t, h.ctx, runtime.calls)
	readOutside(h.t, chat)
	for index, prompt := range prompts {
		arguments, err := json.Marshal(map[string]string{"name": fmt.Sprintf("Report %d", index+1), "cron": "0 9 * * 1", "prompt": prompt})
		if err != nil {
			h.t.Fatal(err)
		}
		if proposed := toolCaller(h.t, chat)("schedule_propose", string(arguments)); proposed.IsError {
			h.t.Fatalf("schedule_propose = %+v", proposed)
		}
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, updateID, "proposed")
	h.waitState(updateID, "delivered")
}

// The model withdraws part of a changes widget's list: the widget keeps its controls for the rest,
// and loses them once nothing is left. A long change carries no text in the data, only its title,
// and no reply is sent.
func TestChangesWidgetShowsWithdrawals(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	h.proposeReports(runtime, 1, "Summarise report one.", "Summarise report two.")
	widget := h.nextWidget()
	list := decodeChanges(t, widget.data)
	if widget.caption != "2 changes" || len(list.Changes) != 2 || !list.Preview {
		t.Fatalf("batch widget = %q, %+v", widget.caption, list)
	}
	h.push(widgetTap(2, 101, widget.messageID, "approval:reject:"+list.ID))
	h.wantWidgetAnswer(2)
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	list = decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — rejected" || list.Status != "Rejected." && list.Status != "2 rejected." || list.Changes[0].State != "r" || list.Changes[1].State != "r" {
		t.Fatalf("rejected batch = %q, %+v", caption.caption, list)
	}

	long := "Summarise report two" + strings.Repeat(" and its appendix", 80)
	h.proposeReports(runtime, 3, "Summarise report one.", long)
	widget = h.nextWidget()
	list = decodeChanges(t, widget.data)
	if list.Changes[1].Text != "" || list.Changes[1].Title != `Schedule "Report 2"` {
		t.Fatalf("widget of a long change = %+v", list)
	}

	h.push(textUpdate(4, 101, "drop the second one"))
	chat := receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"items":[2]}`); withdrawn.IsError {
		t.Fatalf("proposal_withdraw = %+v", withdrawn)
	}
	caption = h.nextWidgetEdit("editMessageCaption", widget.messageID)
	list = decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — 1 waiting, 1 withdrawn" || list.Status != "1 waiting, 1 withdrawn." || list.Changes[0].State != "w" || list.Changes[1].State != "h" || !list.Preview {
		t.Fatalf("widget after a withdrawal = %q, %+v", caption.caption, list)
	}
	chat.answer <- agentruntime.RunResult{Response: "withdrawn"}
	h.wantReply(101, 4, "withdrawn")
	h.waitState(4, "delivered")

	h.push(textUpdate(5, 101, "forget all of it"))
	chat = receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"all":true}`); withdrawn.IsError {
		t.Fatalf("withdraw all = %+v", withdrawn)
	}
	caption = h.nextWidgetEdit("editMessageCaption", widget.messageID)
	list = decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — withdrawn" || list.Changes[0].State != "h" || list.Changes[1].State != "h" {
		t.Fatalf("widget once nothing is left = %q, %+v", caption.caption, list)
	}
	chat.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 5, "done")
	h.waitState(5, "delivered")
	if count := len(h.messages); count != 0 {
		t.Fatalf("withdrawals sent %d messages", count)
	}
	h.wantNoWidgetTextEdit()
}

// A record of the proposal kind with several changes, as a message sent before the changes widget
// is, is still edited in the proposal view's shape: the widget keeps its actions for the rest, and
// loses them once nothing is left. A withdrawal that makes the text too long for the data shortens
// it there, keeps the status whole, and sends the whole text as a reply. No edit tries the widget's
// text first.
func TestProposalKindRecordOfSeveralChangesKeepsItsShape(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	// A probe pair, as a proposal widget shows it, gives the size of the data around the second
	// text, which is then grown until the data just fits.
	h.proposeReports(runtime, 1, "Summarise report one.", "Summarise report two.")
	probe := h.nextWidget()
	h.exec(`UPDATE approvals SET widget_kind = 'proposal' WHERE message_id = ?`, probe.messageID)
	probeID := decodeChanges(t, probe.data).ID
	pressed, err := h.approvals.PressKept(h.ctx, "alice", probeID, nil)
	if err != nil || !pressed.Changed || pressed.Shown.Widget != approval.ProposalWidget {
		t.Fatalf("probe = %+v %v", pressed, err)
	}
	frame := len(marshalWidgetData(widgetData{Text: plainWidgetText(pressed.Shown.Body), Status: plainWidgetText(pressed.Shown.Status), Actions: proposalActions(pressed.Buttons)})) - len("Summarise report two.")
	h.push(widgetTap(2, 101, probe.messageID, "approval:reject:"+probeID))
	h.wantWidgetAnswer(2)
	h.wantOutcome(probe.messageID, "Rejected; nothing was written.")

	long := "Summarise report two" + strings.Repeat(" and its appendix", 50)
	long = strings.TrimSpace(long[:widgetDataLimit-frame-12]) + "."
	h.proposeReports(runtime, 3, "Summarise report one.", long)
	widget := h.nextWidget()
	h.exec(`UPDATE approvals SET widget_kind = 'proposal' WHERE message_id = ?`, widget.messageID)
	pressed, err = h.approvals.PressKept(h.ctx, "alice", decodeChanges(t, widget.data).ID, nil)
	if err != nil || !pressed.Changed {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	if data, fits := encodeWidgetData(locale.English, pressed.Shown.Body, pressed.Shown.Status, proposalActions(pressed.Buttons)); !fits || len(data) < widgetDataLimit-20 {
		t.Fatalf("batch data in the proposal shape = %d bytes, want just under the limit", len(data))
	}

	h.push(textUpdate(4, 101, "drop the second one"))
	chat := receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"items":[2]}`); withdrawn.IsError {
		t.Fatalf("proposal_withdraw = %+v", withdrawn)
	}
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	data := decodeWidget(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "2 changes — 1 waiting, 1 withdrawn" || data.Status != "1 waiting, 1 withdrawn." || !strings.Contains(data.Text, "…") ||
		!strings.HasPrefix(data.Text, "2 changes, confirmed or rejected together:") || !strings.HasSuffix(data.Text, "Withdrawn by the assistant; nothing was written.") {
		t.Fatalf("widget after a withdrawal that outgrows its data = %q, %+v", caption.caption, data)
	}
	if len(data.Actions) != 3 {
		t.Fatalf("widget actions after a partial withdrawal = %+v", data.Actions)
	}
	full := h.nextMessage()
	if full.replyTo != widget.messageID || !strings.Contains(full.text, long) || !strings.Contains(full.text, "Withdrawn by the assistant") || !strings.Contains(full.text, "Expires in 10 minutes.") {
		t.Fatalf("whole text after the withdrawal = %+v", full)
	}
	chat.answer <- agentruntime.RunResult{Response: "withdrawn"}
	h.wantReply(101, 4, "withdrawn")
	h.waitState(4, "delivered")

	h.push(textUpdate(5, 101, "forget all of it"))
	chat = receive(t, h.ctx, runtime.calls)
	if withdrawn := toolCaller(t, chat)("proposal_withdraw", `{"all":true}`); withdrawn.IsError {
		t.Fatalf("withdraw all = %+v", withdrawn)
	}
	if caption, data := h.wantOutcome(widget.messageID, "Withdrawn by the assistant; nothing was written."); caption.caption != "2 changes — withdrawn" || strings.Contains(data.Text, "…") {
		t.Fatalf("widget once nothing is left = %q, %+v", caption.caption, data)
	}
	chat.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 5, "done")
	h.waitState(5, "delivered")
	h.wantNoWidgetTextEdit()
}

func TestWidgetShowsExpiry(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{approvalSweep: 100 * time.Millisecond})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	confirm, _, _ := widgetActions(t, widget.data)
	h.expirePendingApprovals(widget.messageID)
	if caption, _ := h.wantOutcome(widget.messageID, "Expired; nothing was written."); caption.caption != widget.caption+" — expired" {
		t.Fatalf("caption after expiry = %q", caption.caption)
	}
	h.wantNoWidgetTextEdit()

	h.push(widgetTap(2, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(2)
	h.wantOutcome(widget.messageID, "Expired; nothing was written.")
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("an expired widget proposal created %d jobs", count)
	}
	h.wantNoWidgetTextEdit()
}

// A widget's data is decided on its encoded bytes: a text in any script is shortened in the middle
// to fit with its status whole, and a status too long alone is shortened instead with no text.
func TestWidgetDataKeepsTextInAnyScript(t *testing.T) {
	t.Parallel()
	actions := []widgetAction{{Label: "Confirm", Data: "approval:confirm:0000000000000000"}}
	for name, character := range map[string]string{"two bytes": "ж", "three bytes": "界", "four bytes": "🙂", "escaped": `"`} {
		if _, fits := encodeWidgetData(locale.English, strings.Repeat(character, 900), "Done.", actions); fits {
			t.Fatalf("%s: 900 characters fit the data whole", name)
		}
		raw, shortened, fits := shortenWidgetData(locale.English, strings.Repeat(character, 900), "Done.", actions)
		data := decodeWidget(t, raw)
		if !fits || !shortened || len(raw) > widgetDataLimit || data.Status != "Done." || !strings.Contains(data.Text, "…") {
			t.Fatalf("%s: fits = %t, shortened = %t, %d bytes, %+v", name, fits, shortened, len(raw), data)
		}
		kept := utf8.RuneCountInString(data.Text)
		encoded := len(marshalWidgetData(widgetData{Text: character})) - len(marshalWidgetData(widgetData{}))
		if most := (widgetDataLimit - 100) / encoded; kept < most*9/10 {
			t.Fatalf("%s: kept %d characters, want about %d", name, kept, most)
		}
		caption, plain := widgetCaption(strings.Repeat(character, 2000))
		if units := utf16Length(caption); !plain || units > captionLimit || units < captionLimit*9/10 {
			t.Fatalf("%s: caption of %d units, plain = %t", name, units, plain)
		}
	}
	raw, shortened, fits := shortenWidgetData(locale.English, "Save the note", "Done. "+strings.Repeat("Saved. ", 200), actions)
	if data := decodeWidget(t, raw); !fits || !shortened || len(raw) > widgetDataLimit || data.Text != "" || !strings.HasPrefix(data.Status, "Done. Saved.") || !strings.Contains(data.Status, "…") {
		t.Fatalf("status too long for the data = %t %t %+v", fits, shortened, data)
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
	if answer := h.wantWidgetAnswer(2); answer != localetest.English(WidgetIgnoredAnswer) {
		t.Fatalf("answer to a tap from another message = %q", answer)
	}
	h.waitOffset(3)
	if count := h.countRows(`SELECT count(*) FROM scheduled_jobs`); count != 0 {
		t.Fatalf("a tap from another message scheduled %d jobs", count)
	}

	tap := widgetTap(3, 101, widget.messageID, confirm)
	tap.WidgetQuery.Chat, tap.WidgetQuery.ChatType = nil, "sender"
	h.push(tap)
	if answer := h.wantWidgetAnswer(3); answer != localetest.English(WidgetWorkingAnswer) {
		t.Fatalf("answer to a tap without a chat = %q", answer)
	}
	h.wantOutcome(widget.messageID, "Scheduled. First run:")
	h.continueTurn(runtime, 3)
}

// A tap is offered to each kind's handler in turn: one whose data no handler takes is answered as
// doing nothing and logged as ignored, and a proposal tap that is not for the message it came from
// keeps its own reason.
func TestWidgetTapNoHandlerTakesIsAnsweredAsDoingNothing(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	confirm, _, _ := widgetActions(t, widget.data)

	for _, tap := range []struct {
		update            Update
		principal, reason string
	}{
		{widgetTap(2, 101, widget.messageID, "picker:choose:1"), "alice", "unknown_action"},
		{widgetTap(3, 101, widget.messageID+50, confirm), "alice", "misplaced_widget_tap"},
		{widgetTap(4, 202, widget.messageID, confirm), "bob", "foreign_approval"},
	} {
		h.push(tap.update)
		if answer := h.wantWidgetAnswer(tap.update.UpdateID); answer != localetest.English(WidgetIgnoredAnswer) {
			t.Fatalf("answer to a tap ignored as %s = %q", tap.reason, answer)
		}
		logged := regexp.MustCompile(`msg=telegram\.update\.ignored reason=` + tap.reason + ` principal_id=` + tap.principal + `\b`)
		h.waitUntil("the tap logged as ignored with "+tap.reason, func() bool { return logged.MatchString(h.logs.String()) })
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
}

const outcomeTool = "outcome_write"

// outcomeBook is an executor whose actions end as their arguments say: with the outcome given, and
// failed when asked to.
type outcomeBook struct{}

func (outcomeBook) ActionTools() []string { return []string{outcomeTool} }

func (outcomeBook) ExecuteApproved(_ context.Context, record approval.Record) (approval.Outcome, error) {
	var arguments struct {
		Outcome string `json:"outcome"`
		Fail    bool   `json:"fail"`
	}
	if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
		return approval.Outcome{}, err
	}
	if arguments.Fail {
		return approval.Outcome{}, &approval.ActionError{Reason: "refused", Text: arguments.Outcome}
	}
	return approval.Outcome{Text: arguments.Outcome}, nil
}

func outcomeProposal(t *testing.T, summary, outcome string, fail bool) approval.Proposal {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"summary": summary, "outcome": outcome, "fail": fail})
	if err != nil {
		t.Fatal(err)
	}
	return approval.Proposal{Tool: outcomeTool, Arguments: arguments, Summary: summary}
}

// newOfferHarness runs the application with Alice's proposals as widgets and outcomeBook as an
// executor, for bundles offered outside a chat turn.
func newOfferHarness(t *testing.T, options harnessOptions) *conversationHarness {
	t.Helper()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), options)
	h.provision("alice", "account-alice")
	if err := h.approvals.RegisterExecutor(outcomeBook{}); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.setWidgets("alice", true)
	return h
}

// offerWidget offers proposals to Alice under the heading, as a scheduled review does, and returns
// their records and the widget that shows them.
func (h *conversationHarness) offerWidget(lifetime time.Duration, proposals ...approval.Proposal) ([]approval.Record, sentWidget) {
	h.t.Helper()
	records, err := h.approvals.Offer(h.ctx, "alice", 101, 0, "Weekly review", lifetime, proposals)
	if err != nil {
		h.t.Fatal(err)
	}
	return records, h.nextWidget()
}

// The caption is one line the approval service builds from the records: under an offered bundle's
// heading, a single proposal's first line or the number of changes, and once resolved what became of
// them, as one word or as counts when they ended differently. The data holds the whole text with the
// status apart.
func TestWidgetCaptionNamesTheProposalInOneLine(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	_, single := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note A\nWith every line of the list.", "Saved.", false))
	data := decodeWidget(t, single.data)
	if single.caption != "Weekly review: Save the note A" || data.Text != "Weekly review\n\nSave the note A\nWith every line of the list." ||
		data.Status != "Expires in 10 minutes." || len(data.Actions) != 2 {
		t.Fatalf("offered widget = %q, %+v", single.caption, data)
	}
	h.push(widgetTap(1, 101, single.messageID, data.Actions[1].Data))
	h.wantWidgetAnswer(1)
	if caption, _ := h.wantOutcome(single.messageID, "Rejected; nothing was written."); caption.caption != "Weekly review: Save the note A — rejected" {
		t.Fatalf("caption of a rejected proposal = %q", caption.caption)
	}

	_, batch := h.offerWidget(10*time.Minute,
		outcomeProposal(t, "Save the note B", "Saved.", false),
		outcomeProposal(t, "Save the note C", "The note is locked.", true),
		outcomeProposal(t, "Save the note D", "Saved.", false),
	)
	list := decodeChanges(t, batch.data)
	if batch.caption != "Weekly review: 3 changes" || list.Heading != "Weekly review" || list.Status != "Expires in 10 minutes." || len(list.Changes) != 3 ||
		list.Changes[1].Title != "Save the note C" || list.Changes[1].Text != "Save the note C" || list.Changes[1].State != "w" {
		t.Fatalf("offered batch = %q, %+v", batch.caption, list)
	}
	h.push(widgetTap(2, 101, batch.messageID, keepTap(list.ID, "111")))
	h.wantWidgetAnswer(2)
	caption := h.nextWidgetEdit("editMessageCaption", batch.messageID)
	list = decodeChanges(t, h.nextWidgetEdit("editMessageWidgetData", batch.messageID).data)
	if caption.caption != "Weekly review: 3 changes — 2 done, 1 failed" || list.Status != "2 done, 1 failed." ||
		list.Changes[0].State != "d" || list.Changes[0].Outcome != "Saved." || list.Changes[1].State != "f" || list.Changes[1].Outcome != "Failed. The note is locked." || list.Changes[2].State != "d" {
		t.Fatalf("batch whose items ended differently = %q, %+v", caption.caption, list)
	}
	h.waitOffset(3)
	if count := len(h.messages); count != 0 {
		t.Fatalf("an outcome that fits the data sent %d messages", count)
	}
	h.wantNoWidgetTextEdit()
}

// A proposal the intent check warns about is marked in its caption, which still names the proposal;
// the warning is the widget's title.
func TestWidgetCaptionMarksAWarnedProposal(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newWidgetHarness(t, runtime, harnessOptions{})
	h.setWidgets("alice", true)
	h.approvals.SetIntentChecker(flagEverything{})

	h.proposeJob(runtime, 1, 0, "Weekly tasks", "List my overdue tasks.")
	widget := h.nextWidget()
	data := decodeWidget(t, widget.data)
	if !strings.HasPrefix(widget.caption, "⚠️ ") || !strings.Contains(widget.caption, `"Weekly tasks"`) || strings.Contains(widget.caption, approval.IntentWarning) ||
		!strings.HasPrefix(data.Text, approval.IntentWarning+"\n\n") || !strings.Contains(data.Text, "List my overdue tasks.") {
		t.Fatalf("warned widget = %q, %+v", widget.caption, data)
	}
}

// An outcome too long for the data shortens the text there and keeps the status whole, and the whole
// text is sent as a reply once: an edit that leaves the data as it was sends nothing again.
func TestWidgetOutcomeTooLongForTheData(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	outcome := strings.TrimSpace(strings.Repeat("Saved a line. ", 70))
	_, widget := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note A\nWith every line of the list.", outcome, false))
	confirm := decodeWidget(t, widget.data).Actions[0].Data
	h.push(widgetTap(1, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(1)
	caption := h.nextWidgetEdit("editMessageCaption", widget.messageID)
	data := decodeWidget(t, h.nextWidgetEdit("editMessageWidgetData", widget.messageID).data)
	if caption.caption != "Weekly review: Save the note A — done" || data.Status != "Done. "+outcome || !strings.HasPrefix(data.Text, "Weekly") ||
		!strings.Contains(data.Text, "…") || len(data.Actions) != 0 {
		t.Fatalf("widget after a long outcome = %q, %+v", caption.caption, data)
	}
	full := h.nextMessage()
	if full.replyTo != widget.messageID || !strings.Contains(full.text, "With every line of the list.") || !strings.Contains(full.text, "Done. "+outcome) {
		t.Fatalf("whole outcome = %+v", full)
	}

	h.push(widgetTap(2, 101, widget.messageID, confirm))
	h.wantWidgetAnswer(2)
	h.nextWidgetEdit("editMessageCaption", widget.messageID)
	if again := h.nextWidgetEdit("editMessageWidgetData", widget.messageID); again.data != h.widgetData(widget.messageID) {
		t.Fatalf("repeated outcome data = %q", again.data)
	}
	h.waitOffset(3)
	if count := len(h.messages); count != 0 {
		t.Fatalf("a repeated outcome sent %d more messages", count)
	}
	h.wantNoWidgetTextEdit()
}

func (h *conversationHarness) widgetData(messageID int64) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.widgets.data[messageID]
}

// A widget Telegram no longer lets the bot edit still gets its outcome, as a reply.
func TestWidgetEditFailureOnATapRepliesTheOutcome(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{})

	_, widget := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note A", "Saved.", false))
	h.mu.Lock()
	h.widgets.editStatus = http.StatusBadRequest
	h.mu.Unlock()
	h.push(widgetTap(1, 101, widget.messageID, decodeWidget(t, widget.data).Actions[0].Data))
	h.wantWidgetAnswer(1)
	if reply := h.nextMessage(); !strings.Contains(reply.text, "Save the note A") || !strings.Contains(reply.text, "Done. Saved.") {
		t.Fatalf("outcome of a widget that cannot be edited = %+v", reply)
	}
	h.wantNoWidgetTextEdit()
}

// A delivery that fails clears the widget flag with the message, and the resend that sends a widget
// sets it.
func TestWidgetFlagFollowsDelivery(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{approvalSweep: 50 * time.Millisecond})
	h.mu.Lock()
	h.widgets.sendStatus = http.StatusInternalServerError
	h.mu.Unlock()

	records, err := h.approvals.Offer(h.ctx, "alice", 101, 0, "Weekly review", 10*time.Minute, []approval.Proposal{outcomeProposal(t, "Save the note A", "Saved.", false)})
	if err == nil {
		t.Fatal("an offer whose widget failed was delivered")
	}
	unflagged := func() bool {
		return h.countRows(`SELECT count(*) FROM approvals WHERE id = ? AND message_id IS NULL AND widget_kind = ''`, records[0].ID) == 1
	}
	if !unflagged() {
		t.Fatal("a failed delivery left the proposal flagged or shown")
	}
	// A stale flag, as a message recorded earlier would leave, must be cleared by the next failure.
	if _, err := h.db.ExecContext(h.ctx, `UPDATE approvals SET widget_kind = 'proposal' WHERE id = ?`, records[0].ID); err != nil {
		t.Fatal(err)
	}
	h.waitUntil("a failed resend clear the widget flag", unflagged)

	h.mu.Lock()
	h.widgets.sendStatus = 0
	h.mu.Unlock()
	if widget := h.nextWidget(); !h.widgetFlagged(widget.messageID) {
		t.Fatal("the resent widget is not recorded as one")
	}
}

// The "Done" message of a direct action has buttons of its own and is never recorded as a widget.
func TestDirectActionMessageIsNotAWidget(t *testing.T) {
	t.Parallel()
	h, runtime, _ := newActHarness(t, readingTools{})
	h.setWidgets("alice", true)

	h.push(textUpdate(1, 101, "save a note"))
	call := receive(t, h.ctx, runtime.calls)
	text, err := writeNote(call.ctx, h, "A", false)
	wantDone(t, text, err)
	h.finishTurn(call, 1, "saved")
	done := h.nextMessage()
	h.waitUntil("the Done message recorded", func() bool {
		return h.countRows(`SELECT count(*) FROM approvals WHERE mode = 'direct' AND message_id = ?`, done.messageID) == 1
	})
	if h.widgetFlagged(done.messageID) || len(h.widgets.sent) != 0 {
		t.Fatal("a direct action's message is recorded as a widget")
	}
}

// A flagged widget expires without a text edit; one whose records predate the flag is found by
// Telegram's refusal of the text edit and still shows its outcome, in its text, which is all its
// older template draws.
func TestUnflaggedWidgetIsEditedThroughTheRefusedTextEdit(t *testing.T) {
	t.Parallel()
	h := newOfferHarness(t, harnessOptions{approvalSweep: 100 * time.Millisecond})

	_, flagged := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note A", "Saved.", false))
	h.expirePendingApprovals(flagged.messageID)
	h.wantOutcome(flagged.messageID, "Expired; nothing was written.")
	h.wantNoWidgetTextEdit()

	records, legacy := h.offerWidget(10*time.Minute, outcomeProposal(t, "Save the note B", "Saved.", false))
	if _, err := h.db.ExecContext(h.ctx, `UPDATE approvals SET widget_kind = '' WHERE bundle_id = ?`, records[0].BundleID); err != nil {
		t.Fatal(err)
	}
	h.expirePendingApprovals(legacy.messageID)
	caption := h.nextWidgetEdit("editMessageCaption", legacy.messageID)
	data := decodeWidget(t, h.nextWidgetEdit("editMessageWidgetData", legacy.messageID).data)
	if caption.caption != "Weekly review: Save the note B — expired" {
		t.Fatalf("caption of an unflagged widget after expiry = %q", caption.caption)
	}
	if !strings.HasSuffix(data.Text, "Expired; nothing was written.") || data.Status != "" || len(data.Actions) != 0 {
		t.Fatalf("data of an unflagged widget after expiry = %+v, want the status in its text", data)
	}
	if count := h.widgetTextEdits(); count != 1 {
		t.Fatalf("%d text edits of the unflagged widget, want the one Telegram refused", count)
	}
}
