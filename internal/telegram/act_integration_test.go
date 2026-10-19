package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const noteTool = "note_write"

// noteBook is an executor whose notes are written at once in a clean turn and can be undone until
// someone else changes them.
type noteBook struct {
	mu         sync.Mutex
	notes      map[string]string
	executions int
	changed    bool
}

func (b *noteBook) ActionTools() []string { return []string{noteTool} }

func (b *noteBook) ExecuteApproved(_ context.Context, record approval.Record) (approval.Outcome, error) {
	var arguments struct{ Name, Text string }
	if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
		return approval.Outcome{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.executions++
	b.notes[arguments.Name] = arguments.Text
	undo, err := json.Marshal(map[string]string{"name": arguments.Name})
	return approval.Outcome{Text: "Saved the note " + arguments.Name + ".", Undo: undo}, err
}

func (b *noteBook) Undo(_ context.Context, _ approval.Record, undo json.RawMessage) (approval.Outcome, error) {
	var payload struct{ Name string }
	if err := json.Unmarshal(undo, &payload); err != nil {
		return approval.Outcome{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the note changed since."}
	}
	delete(b.notes, payload.Name)
	return approval.Outcome{Text: "Removed the note " + payload.Name + "."}, nil
}

func (b *noteBook) note(name string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text, found := b.notes[name]
	return text, found
}

// readingTools reads outside text, which can be held until released, and household text.
type readingTools struct {
	started chan struct{}
	release chan struct{}
}

func (p readingTools) Contribute(context.Context, string) (tools.Contribution, error) {
	outside := tools.Definition{Name: "outside_read", Description: "Read a page.", InputSchema: json.RawMessage(`{"type":"object"}`), Risk: tools.RiskRead, ThirdParty: true}
	household := tools.Definition{Name: "home_read", Description: "Read a sensor.", InputSchema: json.RawMessage(`{"type":"object"}`), Risk: tools.RiskRead, ThirdParty: true, Household: true}
	return tools.Contribution{Definitions: []tools.Definition{outside, household}}, nil
}

func (p readingTools) Execute(_ context.Context, _ string, call tools.Call) (tools.Result, error) {
	if call.Name == "outside_read" && p.started != nil {
		p.started <- struct{}{}
		<-p.release
	}
	return tools.Result{Text: "some text"}, nil
}

func newActHarness(t *testing.T, reading readingTools) (*conversationHarness, *controlledRuntime, *noteBook) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{providers: []tools.Provider{reading}})
	h.provision("alice", "acct-alice-1111")
	h.provision("bob", "acct-bob-2222")
	book := &noteBook{notes: make(map[string]string)}
	if err := h.approvals.RegisterExecutor(book); err != nil {
		t.Fatal(err)
	}
	h.start()
	return h, runtime, book
}

func writeNote(ctx context.Context, h *conversationHarness, name string, strict bool) (string, error) {
	return h.approvals.Act(ctx, "alice", approval.Proposal{
		Tool:      noteTool,
		Arguments: json.RawMessage(fmt.Sprintf(`{"name":%q,"text":"buy bread"}`, name)),
		Summary:   "Save the note " + name,
		Intent:    approval.Intent{Verb: "save", TargetKind: "note", TargetName: name},
		Strict:    strict,
	})
}

func wantDone(t *testing.T, text string, err error) {
	t.Helper()
	if err != nil || !strings.HasPrefix(text, "Done:") || !strings.Contains(text, "Undo button") {
		t.Fatalf("direct result = %q %v, want done with an Undo", text, err)
	}
}

func wantProposed(t *testing.T, text string, err error) {
	t.Helper()
	if err != nil || text != approval.ProposedText {
		t.Fatalf("result = %q %v, want a proposal", text, err)
	}
}

func (h *conversationHarness) directRecord(name string) (id, status, reason, requestID string) {
	h.t.Helper()
	if err := h.db.QueryRowContext(h.ctx,
		`SELECT id, status, COALESCE(reason, ''), request_id FROM approvals WHERE mode = 'direct' AND summary LIKE ?`, "%Save the note "+name,
	).Scan(&id, &status, &reason, &requestID); err != nil {
		h.t.Fatalf("direct record %s: %v", name, err)
	}
	return id, status, reason, requestID
}

// finishTurn answers the turn and waits until its reply is out.
func (h *conversationHarness) finishTurn(call runtimeCall, updateID int64, reply string) {
	h.t.Helper()
	call.answer <- agentruntime.RunResult{Response: reply}
	h.wantReply(101, updateID, reply)
	h.waitState(updateID, "delivered")
}

// A clean turn acts at once and acts once for a repeated call; after outside text the same tool
// proposes. The "Done" message follows the reply, before the bundle.
func TestActActsInACleanTurnAndTheDoneMessagePrecedesTheBundle(t *testing.T) {
	t.Parallel()
	h, runtime, book := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "save my grocery note"))
	call := receive(t, h.ctx, runtime.calls)

	text, err := writeNote(call.ctx, h, "groceries", false)
	wantDone(t, text, err)
	if !strings.Contains(text, "Saved the note groceries.") {
		t.Fatalf("direct result = %q, want the outcome", text)
	}
	if note, found := book.note("groceries"); !found || note != "buy bread" {
		t.Fatalf("note = %q %v, want it written at once", note, found)
	}
	again, err := writeNote(call.ctx, h, "groceries", false)
	if err != nil || !strings.Contains(again, "already made in this turn") || book.executions != 1 {
		t.Fatalf("repeated result = %q %v after %d executions, want one action", again, err, book.executions)
	}
	withdrawn, done, err := h.approvals.Withdraw(call.ctx, "alice", nil, true)
	if err != nil || done || !strings.Contains(withdrawn, "Already done without asking") || !strings.Contains(withdrawn, "Save the note groceries") {
		t.Fatalf("withdraw = %q %v %v, want the direct action named as done", withdrawn, done, err)
	}

	if result := executeTool(t, h.ctx, call.request.Tools, "outside_read", `{}`); result.IsError {
		t.Fatalf("outside_read = %+v", result)
	}
	proposed, err := writeNote(call.ctx, h, "errands", false)
	wantProposed(t, proposed, err)
	h.finishTurn(call, 1, "saved")

	id, status, _, requestID := h.directRecord("groceries")
	if status != approval.StatusDone || requestID != h.requestID(1) {
		t.Fatalf("direct record = %s for request %s, want done for %s", status, requestID, h.requestID(1))
	}
	done1 := h.nextMessage()
	if done1.replyTo != 0 || !strings.HasPrefix(done1.text, "Done:") || !strings.Contains(done1.text, "Save the note groceries") ||
		strings.Contains(done1.text, "errands") || fmt.Sprint(buttonData(done1.keyboard)) != fmt.Sprint([]string{"undo:" + id}) {
		t.Fatalf("done message = %+v, want the direct action with its Undo", done1)
	}
	bundle := h.nextMessage()
	if !strings.Contains(bundle.text, "Save the note errands") || !strings.Contains(bundle.text, "Expires in") {
		t.Fatalf("bundle = %+v, want the proposal after the done message", bundle)
	}
	approvalButtons(t, bundle.keyboard)
}

// Without a chat turn that answers the user's own message, Act proposes.
func TestActProposesOutsideAUserChatTurn(t *testing.T) {
	t.Parallel()
	h, runtime, book := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	chat := tools.WithTurn(h.ctx, outbox.ChatTurn(h.requestID(1)))
	userRun := tools.NewRun("save it")
	userRun.MarkUserMessage()
	for name, ctx := range map[string]context.Context{
		"no-turn":     h.ctx,
		"no-run":      chat,
		"bot-message": tools.WithRun(chat, tools.NewRun("")),
		"scheduled":   tools.WithRun(tools.WithTurn(h.ctx, outbox.ScheduledTurn("run-1")), userRun),
	} {
		text, err := writeNote(ctx, h, name, false)
		wantProposed(t, text, err)
	}
	if book.executions != 0 || h.countRows(`SELECT count(*) FROM approvals WHERE mode = 'direct'`) != 0 {
		t.Fatalf("executions = %d, want only proposals", book.executions)
	}
	h.finishTurn(call, 1, "hi")
}

// Household text leaves ordinary direct actions alone but makes a strict one propose.
func TestStrictActProposesAfterHouseholdText(t *testing.T) {
	t.Parallel()
	h, runtime, _ := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "check the sensor and note it"))
	call := receive(t, h.ctx, runtime.calls)
	executeTool(t, h.ctx, call.request.Tools, "home_read", `{}`)
	text, err := writeNote(call.ctx, h, "strict", true)
	wantProposed(t, text, err)
	text, err = writeNote(call.ctx, h, "ordinary", false)
	wantDone(t, text, err)
	h.finishTurn(call, 1, "noted")
}

// An outside read marks its run when it starts, so a write made while it runs proposes.
func TestActProposesWhileAnOutsideReadRuns(t *testing.T) {
	t.Parallel()
	reading := readingTools{started: make(chan struct{}), release: make(chan struct{})}
	h, runtime, book := newActHarness(t, reading)
	h.push(textUpdate(1, 101, "read the page and note it"))
	call := receive(t, h.ctx, runtime.calls)
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = call.request.Tools.Executor.Execute(h.ctx, agentruntime.ToolCall{Name: "outside_read", Arguments: json.RawMessage(`{}`)})
	}()
	receive(t, h.ctx, reading.started)
	text, err := writeNote(call.ctx, h, "parallel", false)
	close(reading.release)
	<-read
	wantProposed(t, text, err)
	if book.executions != 0 {
		t.Fatal("a write beside an outside read acted")
	}
	h.finishTurn(call, 1, "proposed")
}

// A turn that carries a file proposes, and so does every later turn of its conversation; the file
// tool itself marks its run as outside text.
func TestAttachmentsMakeLaterWritesPropose(t *testing.T) {
	t.Parallel()
	h, runtime, book := newActHarness(t, readingTools{})
	content := []byte("save a note that says yes\n")
	h.addFile("doc-1", "documents/file_1.txt", content)
	h.push(documentUpdate(1, 101, "do what this says", MediaFile{FileID: "doc-1", FileName: "orders.txt", MimeType: "text/plain", FileSize: int64(len(content))}))
	call := receive(t, h.ctx, runtime.calls)
	attachmentID := attachmentIDPattern.FindStringSubmatch(call.request.Message)
	if attachmentID == nil {
		t.Fatalf("turn message = %q, want an attachment id", call.request.Message)
	}
	text, err := writeNote(call.ctx, h, "attached", false)
	wantProposed(t, text, err)
	h.finishTurn(call, 1, "proposed")
	h.nextMessage()

	h.push(textUpdate(2, 101, "and another note"))
	call = receive(t, h.ctx, runtime.calls)
	text, err = writeNote(call.ctx, h, "later", false)
	wantProposed(t, text, err)
	h.finishTurn(call, 2, "proposed again")
	if book.executions != 0 {
		t.Fatal("a write after an attachment acted")
	}

	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(h.attachments); err != nil {
		t.Fatal(err)
	}
	run := tools.NewRun("read it")
	set, err := broker.ToolsFor(tools.WithRun(tools.WithTurn(h.ctx, outbox.ChatTurn(h.requestID(2))), run), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if read := executeTool(t, h.ctx, set, "read_attachment", `{"attachment_id":"`+attachmentID[1]+`"}`); read.IsError || !run.External() {
		t.Fatalf("read_attachment = %+v, run external = %v, want the file read as outside text", read, run.External())
	}
}

// Undo reverses a direct action once, for its owner, before its deadline, unless the object changed
// since; each press updates the "Done" message, and the next turn learns of it.
func TestUndoReversesADirectActionOnceForItsOwner(t *testing.T) {
	t.Parallel()
	h, runtime, book := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "save three notes"))
	call := receive(t, h.ctx, runtime.calls)
	for _, name := range []string{"groceries", "errands", "chores"} {
		text, err := writeNote(call.ctx, h, name, false)
		wantDone(t, text, err)
	}
	h.finishTurn(call, 1, "saved")
	done := h.nextMessage()
	groceries, _, _, _ := h.directRecord("groceries")
	errands, _, _, _ := h.directRecord("errands")
	chores, _, _, _ := h.directRecord("chores")
	if !strings.HasPrefix(done.text, "Done, 3 changes:") || fmt.Sprint(buttonData(done.keyboard)) != fmt.Sprint([]string{"undo:" + groceries, "undo:" + errands, "undo:" + chores}) {
		t.Fatalf("done message = %+v, want three actions with their Undo buttons", done)
	}
	h.waitMessageRecorded(done.messageID)

	if undid, err := h.approvals.Undo(h.ctx, "bob", groceries); err != nil || undid.Owned {
		t.Fatalf("undo by another principal = %+v %v, want it ignored", undid, err)
	}
	if _, status, _, _ := h.directRecord("groceries"); status != approval.StatusDone {
		t.Fatalf("status after another principal's press = %s", status)
	}

	undid, err := h.approvals.Undo(h.ctx, "alice", groceries)
	if err != nil || !undid.Owned || undid.Answer != "Undone." {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if _, found := book.note("groceries"); found {
		t.Fatal("the undone note is still there")
	}
	edit := h.nextEdit()
	if edit.messageID != done.messageID || !strings.Contains(edit.text, "Undone.") ||
		fmt.Sprint(buttonData(edit.keyboard)) != fmt.Sprint([]string{"undo:" + errands, "undo:" + chores}) {
		t.Fatalf("done message after undo = %+v", edit)
	}
	if undid, err := h.approvals.Undo(h.ctx, "alice", groceries); err != nil || undid.Answer != "This was already undone." {
		t.Fatalf("second undo = %+v %v", undid, err)
	}

	book.mu.Lock()
	book.changed = true
	book.mu.Unlock()
	if undid, err := h.approvals.Undo(h.ctx, "alice", errands); err != nil || undid.Answer != "Not undone: the note changed since." {
		t.Fatalf("undo of a changed note = %+v %v", undid, err)
	}
	if _, status, reason, _ := h.directRecord("errands"); status != approval.StatusDone || reason != approval.ReasonUndoRefused {
		t.Fatalf("refused record = %s %s", status, reason)
	}
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Not undone: the note changed since.") ||
		fmt.Sprint(buttonData(edit.keyboard)) != fmt.Sprint([]string{"undo:" + chores}) {
		t.Fatalf("done message after a refused undo = %+v", edit)
	}

	if _, err := h.db.ExecContext(h.ctx, `UPDATE approvals SET undo_until = '2000-01-01T00:00:00.000Z' WHERE id = ?`, chores); err != nil {
		t.Fatal(err)
	}
	if undid, err := h.approvals.Undo(h.ctx, "alice", chores); err != nil || undid.Answer != "The 24 hours to undo this have passed." {
		t.Fatalf("late undo = %+v %v", undid, err)
	}
	if _, found := book.note("chores"); !found {
		t.Fatal("a late undo removed the note")
	}

	h.push(textUpdate(2, 101, "what changed?"))
	call = receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, "pressed Undo on your note_write action; it was undone") ||
		!strings.Contains(call.request.Message, "but it was not undone: the note changed since.") {
		t.Fatalf("next turn message = %q, want both Undo presses", call.request.Message)
	}
	h.finishTurn(call, 2, "one undone")
}

type flagEverything struct{}

func (flagEverything) Mismatch(context.Context, string, string, approval.Intent) (bool, error) {
	return true, nil
}

// The intent check still runs after a direct action, and its warning reaches the "Done" message.
func TestDirectActionCarriesTheIntentWarning(t *testing.T) {
	t.Parallel()
	h, runtime, _ := newActHarness(t, readingTools{})
	h.approvals.SetIntentChecker(flagEverything{})
	h.push(textUpdate(1, 101, "save a note"))
	call := receive(t, h.ctx, runtime.calls)
	text, err := writeNote(call.ctx, h, "flagged", false)
	wantDone(t, text, err)
	h.finishTurn(call, 1, "saved")
	done := h.nextMessage()
	if !strings.Contains(done.text, approval.DirectIntentWarning) || len(buttonData(done.keyboard)) != 1 {
		t.Fatalf("done message = %+v, want the warning beside the Undo", done)
	}
}

// A reconsider turn answers the bot's own reply, not a message the user wrote, so it proposes.
func TestReconsiderTurnProposes(t *testing.T) {
	t.Parallel()
	h, runtime, book := newActHarness(t, readingTools{})
	reconsider := h.thumbsDown(runtime, 1, 2)
	text, err := writeNote(reconsider.ctx, h, "reconsidered", false)
	wantProposed(t, text, err)
	if book.executions != 0 {
		t.Fatal("a reconsider turn acted")
	}
	reconsider.answer <- agentruntime.RunResult{Response: "Sorry, about 3 km."}
	h.nextMessage()
	h.waitState(2, "delivered")
}

// An Undo that a restart interrupts leaves the action done and its "Done" message says so; the
// Undo can then be pressed again, and its result is kept.
func TestAnUndoInterruptedByARestartCanBePressedAgain(t *testing.T) {
	t.Parallel()
	h, runtime, book := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "save a note"))
	call := receive(t, h.ctx, runtime.calls)
	text, err := writeNote(call.ctx, h, "groceries", false)
	wantDone(t, text, err)
	h.finishTurn(call, 1, "saved")
	done := h.nextMessage()
	h.waitMessageRecorded(done.messageID)
	id, _, _, _ := h.directRecord("groceries")
	var finishedAt string
	if err := h.db.QueryRowContext(h.ctx, `SELECT finished_at FROM approvals WHERE id = ?`, id).Scan(&finishedAt); err != nil {
		t.Fatal(err)
	}

	// The press claims the record and the process stops before the executor runs.
	if claimed, err := h.store.ClaimUndo(h.ctx, id, "alice", time.Now()); err != nil || !claimed {
		t.Fatalf("claim = %v %v", claimed, err)
	}
	if undid, err := h.approvals.Undo(h.ctx, "alice", id); err != nil || undid.Answer != "This is being undone." {
		t.Fatalf("press during an undo = %+v %v", undid, err)
	}
	h.stop()
	h.start()
	if _, status, reason, _ := h.directRecord("groceries"); status != approval.StatusDone || reason != approval.ReasonUndoInterrupted {
		t.Fatalf("record after the restart = %s %s, want done with an interrupted undo", status, reason)
	}
	edit := h.nextEdit()
	if edit.messageID != done.messageID || !strings.Contains(edit.text, "A restart interrupted its Undo") ||
		fmt.Sprint(buttonData(edit.keyboard)) != fmt.Sprint([]string{"undo:" + id}) {
		t.Fatalf("done message after the restart = %+v, want the interruption with the Undo kept", edit)
	}
	if _, found := book.note("groceries"); !found {
		t.Fatal("the note is gone before any undo ran")
	}

	if undid, err := h.approvals.Undo(h.ctx, "alice", id); err != nil || undid.Answer != "Undone." {
		t.Fatalf("undo after the restart = %+v %v", undid, err)
	}
	if _, found := book.note("groceries"); found {
		t.Fatal("the undone note is still there")
	}
	var status, outcome, finishedAfter string
	if err := h.db.QueryRowContext(h.ctx, `SELECT status, outcome, finished_at FROM approvals WHERE id = ?`, id).Scan(&status, &outcome, &finishedAfter); err != nil {
		t.Fatal(err)
	}
	if status != approval.StatusUndone || outcome != "Removed the note groceries." || finishedAfter != finishedAt {
		t.Fatalf("undone record = %s %q %s, want undone with the undo's outcome, finished at %s", status, outcome, finishedAfter, finishedAt)
	}
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Undone.") || len(edit.keyboard) != 0 {
		t.Fatalf("done message after the undo = %+v", edit)
	}

	h.push(textUpdate(2, 101, "what happened?"))
	call = receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, "a restart interrupted it") || !strings.Contains(call.request.Message, "it was undone") {
		t.Fatalf("next turn message = %q, want the interrupted and the finished undo", call.request.Message)
	}
	h.finishTurn(call, 2, "undone")
}

// A turn cancelled before its reply never shows its direct actions; the next delivered reply does.
func TestDirectActionsOfACancelledTurnFollowTheNextReply(t *testing.T) {
	t.Parallel()
	h, runtime, _ := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "save a note"))
	call := receive(t, h.ctx, runtime.calls)
	text, err := writeNote(call.ctx, h, "groceries", false)
	wantDone(t, text, err)
	h.push(textUpdate(2, 101, "/cancel"))
	h.wantReply(101, 2, CancelledReply)
	receive(t, h.ctx, call.ctx.Done())
	h.waitState(1, "cancelled")
	if pending := len(h.messages); pending != 0 {
		t.Fatalf("a cancelled turn sent %d messages", pending)
	}

	h.push(textUpdate(3, 101, "thanks"))
	call = receive(t, h.ctx, runtime.calls)
	h.finishTurn(call, 3, "you're welcome")
	id, _, _, _ := h.directRecord("groceries")
	done := h.nextMessage()
	if !strings.HasPrefix(done.text, "Done:") || !strings.Contains(done.text, "Save the note groceries") ||
		fmt.Sprint(buttonData(done.keyboard)) != fmt.Sprint([]string{"undo:" + id}) {
		t.Fatalf("done message = %+v, want the cancelled turn's action with its Undo", done)
	}
}

// Many direct actions are split across "Done" messages that each fit Telegram's text and button
// limits, measured in UTF-16 units, and every action keeps its Undo button.
func TestManyDirectActionsSplitAcrossDoneMessages(t *testing.T) {
	t.Parallel()
	h, runtime, _ := newActHarness(t, readingTools{})
	h.push(textUpdate(1, 101, "save many notes"))
	call := receive(t, h.ctx, runtime.calls)
	// Each emoji is one rune but two UTF-16 units, so a rune budget would overfill a message.
	names := []string{strings.Repeat("😀", 700), strings.Repeat("😁", 700), strings.Repeat("😂", 700)}
	for index := range 120 {
		names = append(names, fmt.Sprintf("short-%03d", index))
	}
	for _, name := range names {
		text, err := writeNote(call.ctx, h, name, false)
		wantDone(t, text, err)
	}
	h.finishTurn(call, 1, "saved")

	rows, err := h.db.QueryContext(h.ctx, `SELECT id FROM approvals WHERE mode = 'direct' ORDER BY created_at, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, "undo:"+id)
	}
	rows.Close()

	var got []string
	messages := 0
	for len(got) < len(want) {
		done := h.nextMessage()
		messages++
		buttons := buttonData(done.keyboard)
		if units := len(utf16.Encode([]rune(done.text))); units > 4096 || len(buttons) > 100 || len(buttons) == 0 {
			t.Fatalf("done message %d has %d units and %d buttons", messages, units, len(buttons))
		}
		got = append(got, buttons...)
	}
	if messages < 3 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%d done messages with buttons %v, want every action's Undo in order across several messages", messages, got)
	}
}
