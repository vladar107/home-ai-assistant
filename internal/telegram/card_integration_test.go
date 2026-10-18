package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type sentPhoto struct {
	chatID    int64
	replyTo   int64
	caption   string
	parseMode string
	data      []byte
	status    int
	threadID  int64
}

// cardProvider stands in for a tool that renders a card: it stages bytes naming the principal for
// the turn the broker says the call belongs to, in the harness's outbox.
type cardProvider struct{ h *conversationHarness }

func (cardProvider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Definitions: []tools.Definition{{
		Name: "stage_card", Description: "Attach a card to the reply.", Risk: tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}}, nil
}

func (p cardProvider) Execute(ctx context.Context, principalID string, _ tools.Call) (tools.Result, error) {
	turn := tools.TurnFrom(ctx)
	if turn == "" {
		return tools.Result{Text: "no turn", IsError: true}, nil
	}
	if err := p.h.outbox.Stage(principalID, turn, cardBytes(principalID)); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Text: "staged"}, nil
}

func cardBytes(principalID string) []byte { return []byte("\x89PNG card of " + principalID) }

// servePhotoUpload answers sendPhoto, whose multipart body ParseForm does not read.
func (h *conversationHarness) servePhotoUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		h.t.Error(err)
		return
	}
	chatID, err := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	if err != nil {
		h.t.Error(err)
		return
	}
	var replyTo int64
	if raw := r.FormValue("reply_parameters"); raw != "" {
		var parameters struct {
			MessageID                int64 `json:"message_id"`
			AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
		}
		if err := json.Unmarshal([]byte(raw), &parameters); err != nil || parameters.MessageID <= 0 || !parameters.AllowSendingWithoutReply {
			h.t.Errorf("reply_parameters %q must quote a message and allow sending without it: %v", raw, err)
		}
		replyTo = parameters.MessageID
	}
	file, _, err := r.FormFile("photo")
	if err != nil {
		h.t.Error(err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		h.t.Error(err)
		return
	}
	threadID := h.threadOf(r.FormValue("message_thread_id"))
	if h.refuseGoneThread(w, r, "sendPhoto", threadID) {
		return
	}
	h.mu.Lock()
	status := http.StatusOK
	if len(h.photoStatuses) > 0 {
		status = h.photoStatuses[0]
		h.photoStatuses = h.photoStatuses[1:]
	}
	reason := h.photoReason
	h.sentCount++
	messageID := sentMessageID(h.sentCount)
	h.mu.Unlock()
	select {
	case h.photos <- sentPhoto{chatID: chatID, replyTo: replyTo, caption: r.FormValue("caption"), parseMode: r.FormValue("parse_mode"), data: data, status: status, threadID: threadID}:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": reason})
		return
	}
	result := Message{MessageID: messageID, Chat: Chat{ID: chatID, Type: ChatTypePrivate}}
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}); err != nil && r.Context().Err() == nil {
		h.t.Error(err)
	}
}

func (h *conversationHarness) nextPhoto() sentPhoto { return receive(h.t, h.ctx, h.photos) }

func (h *conversationHarness) stageCard(call runtimeCall) {
	h.t.Helper()
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "stage_card", Arguments: json.RawMessage(`{}`)})
	if err != nil || result.IsError {
		h.t.Fatalf("stage_card = %+v %v", result, err)
	}
}

// cardFiles lists what the outbox holds for a principal, as paths below their directory.
func (h *conversationHarness) cardFiles(principalID string) []string {
	h.t.Helper()
	var names []string
	for _, kind := range []string{"chat", "schedule"} {
		entries, err := os.ReadDir(filepath.Join(h.outboxDir, principalID, kind))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			h.t.Fatal(err)
		}
		for _, entry := range entries {
			names = append(names, kind+"/"+entry.Name())
		}
	}
	return names
}

func (h *conversationHarness) wantNoCards(principalID string) {
	h.t.Helper()
	if files := h.cardFiles(principalID); len(files) != 0 {
		h.t.Fatalf("outbox of %s still holds %v", principalID, files)
	}
}

func (h *conversationHarness) wantNoPhoto() {
	h.t.Helper()
	select {
	case photo := <-h.photos:
		h.t.Fatalf("unexpected photo: %+v", photo)
	default:
	}
}

func (h *conversationHarness) requestID(updateID int64) string {
	h.t.Helper()
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM conversation_requests WHERE telegram_update_id = ?`, updateID).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func TestCardTurnDeliversOnePhotoWithTheReplyAsCaption(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "plan my day"))
	call := receive(t, h.ctx, runtime.calls)
	h.stageCard(call)
	call.answer <- agentruntime.RunResult{Response: "**Rain** later & take an umbrella."}
	photo := h.nextPhoto()
	if photo.chatID != 101 || photo.replyTo != requestMessageID(1) || photo.parseMode != "HTML" ||
		photo.caption != "<b>Rain</b> later &amp; take an umbrella." || string(photo.data) != string(cardBytes("alice")) {
		t.Fatalf("photo = %+v", photo)
	}
	h.waitState(1, "delivered")
	h.wantNoCards("alice")
	if sent := h.sentTexts(); len(sent) != 0 {
		t.Fatalf("a captioned card also sent text: %q", sent)
	}

	h.push(textUpdate(2, 101, "thanks"))
	call = receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "welcome"}
	h.wantReply(101, 2, "welcome")
	h.waitState(2, "delivered")
	h.wantNoPhoto()
}

func TestCardWithALongReplyArrivesAsPhotoThenText(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "plan my day"))
	call := receive(t, h.ctx, runtime.calls)
	h.stageCard(call)
	answer := strings.Repeat("a", 1100)
	call.answer <- agentruntime.RunResult{Response: answer}
	if photo := h.nextPhoto(); photo.replyTo != requestMessageID(1) || photo.caption != "" || photo.parseMode != "" {
		t.Fatalf("photo = %+v, want no caption quoting the request", photo)
	}
	h.wantMessage(101, answer)
	h.waitState(1, "delivered")
	h.wantNoCards("alice")
}

func TestCardCaptionRefusedByTelegramIsResentAsVisibleText(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.photoStatuses = []int{http.StatusBadRequest}
	h.photoReason = "Bad Request: can't parse entities: Unsupported start tag"
	h.start()

	h.push(textUpdate(1, 101, "plan my day"))
	call := receive(t, h.ctx, runtime.calls)
	h.stageCard(call)
	call.answer <- agentruntime.RunResult{Response: "**Rain** & wind"}
	if refused := h.nextPhoto(); refused.parseMode != "HTML" || refused.caption != "<b>Rain</b> &amp; wind" {
		t.Fatalf("first attempt = %+v", refused)
	}
	if retry := h.nextPhoto(); retry.parseMode != "" || retry.caption != "Rain & wind" || retry.replyTo != requestMessageID(1) {
		t.Fatalf("retry = %+v, want the visible text without a parse mode", retry)
	}
	h.waitState(1, "delivered")
	h.wantNoPhoto()
}

func TestCardPhotoFailureIsRetriedWithTheCardStillStaged(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.photoStatuses = []int{http.StatusInternalServerError}
	h.start()

	h.push(textUpdate(1, 101, "plan my day"))
	call := receive(t, h.ctx, runtime.calls)
	h.stageCard(call)
	call.answer <- agentruntime.RunResult{Response: "Dry all day."}
	if failed := h.nextPhoto(); failed.status != http.StatusInternalServerError {
		t.Fatalf("first attempt = %+v", failed)
	}
	if files := h.cardFiles("alice"); !slices.Equal(files, []string{"chat/" + h.requestID(1) + ".png"}) {
		t.Fatalf("outbox after a failed upload = %v", files)
	}
	if retry := h.nextPhoto(); retry.status != http.StatusOK || retry.caption != "Dry all day." || retry.replyTo != requestMessageID(1) {
		t.Fatalf("retry = %+v", retry)
	}
	h.waitState(1, "delivered")
	h.wantNoCards("alice")
	if sent := h.sentTexts(); len(sent) != 0 {
		t.Fatalf("sent text %q besides the captioned card", sent)
	}
}

func TestCardTextFailureAfterThePhotoIsRetriedWithoutASecondPhoto(t *testing.T) {
	var links strings.Builder
	for range 34 {
		links.WriteString("[x](https://example.com/" + strings.Repeat("p", 80) + ")\n")
	}
	tail := strings.Repeat("b", 1000)
	for _, tc := range []struct {
		name        string
		answer      string
		caption     string
		wantMessage string
	}{
		{name: "uncaptioned", answer: strings.Repeat("a", 1100), wantMessage: strings.Repeat("a", 1100)},
		// The first chunk is long as markup but short once parsed, so it rides as the caption and
		// only the second chunk is left to fail.
		{name: "captioned", answer: links.String() + tail, caption: "captioned", wantMessage: tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newControlledRuntime()
			h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
			h.provision("alice", "acct-alice")
			h.sendStatuses = []int{http.StatusInternalServerError}
			h.start()

			h.push(textUpdate(1, 101, "plan my day"))
			call := receive(t, h.ctx, runtime.calls)
			h.stageCard(call)
			call.answer <- agentruntime.RunResult{Response: tc.answer}
			photo := h.nextPhoto()
			if (photo.caption != "") != (tc.caption != "") || photo.replyTo != requestMessageID(1) {
				t.Fatalf("photo = %+v", photo)
			}
			if failed := h.nextMessage(); failed.status != http.StatusInternalServerError || failed.text != tc.wantMessage {
				t.Fatalf("first text attempt = %+v", failed)
			}
			if files := h.cardFiles("alice"); !slices.Equal(files, []string{"chat/" + h.requestID(1) + ".sent"}) {
				t.Fatalf("outbox after the photo = %v", files)
			}
			h.wantMessage(101, tc.wantMessage)
			h.waitState(1, "delivered")
			h.wantNoPhoto()
			h.wantNoCards("alice")
		})
	}
}

func TestFailedTurnsDeliverTheirTextWithoutTheCard(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true, turnTimeout: 300 * time.Millisecond})
		h.provision("alice", "acct-alice")
		h.start()
		h.push(textUpdate(1, 101, "plan my day"))
		call := receive(t, h.ctx, runtime.calls)
		h.stageCard(call)
		h.wantReply(101, 1, conversation.TimeoutReply)
		h.waitState(1, "delivered")
		h.wantNoPhoto()
		h.wantNoCards("alice")
	})
	t.Run("runtime failure", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
		h.provision("alice", "acct-alice")
		h.start()
		h.push(textUpdate(1, 101, "plan my day"))
		call := receive(t, h.ctx, runtime.calls)
		h.stageCard(call)
		call.answer <- agentruntime.RunResult{}
		h.wantReply(101, 1, conversation.UnavailableReply)
		h.waitState(1, "delivered")
		h.wantNoPhoto()
		h.wantNoCards("alice")
	})
	t.Run("interrupted", func(t *testing.T) {
		runtime := newControlledRuntime()
		h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
		h.provision("alice", "acct-alice")
		input := conversation.EnqueueInput{BotUserID: 999, TelegramUpdateID: 1, TelegramMessageID: requestMessageID(1), PrincipalID: "alice", ChatID: 101, Message: "plan my day"}
		if _, err := h.store.EnqueueConversation(h.ctx, input, conversation.Session{ID: recoverySessionID, PrincipalID: "alice", RuntimeSessionID: recoveryRuntimeSessionID}, recoveryRequestID, nil); err != nil {
			t.Fatal(err)
		}
		if ok, err := h.store.ClaimConversationRequest(h.ctx, recoveryRequestID); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if err := h.outbox.Stage("alice", outbox.ChatTurn(recoveryRequestID), cardBytes("alice")); err != nil {
			t.Fatal(err)
		}
		h.start()
		h.wantReply(101, 1, conversation.InterruptedReply)
		h.waitState(1, "delivered")
		h.wantNoPhoto()
		h.wantNoCards("alice")
	})
}

func TestScheduledRunDeliversItsCardUnderTheScheduledCaption(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true, schedules: clock})
	h.provision("alice", "acct-alice")
	h.start()
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Weekly tasks", "0 9 * * 1", "Plan my day.")

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	h.stageCard(run)
	run.answer <- agentruntime.RunResult{Response: "Dry all day.", Final: "Dry all day."}
	photo := h.nextPhoto()
	if photo.chatID != 101 || photo.replyTo != 0 || photo.parseMode != "HTML" ||
		photo.caption != "<b>Scheduled: Weekly tasks</b>\n\nDry all day." || string(photo.data) != string(cardBytes("alice")) {
		t.Fatalf("scheduled photo = %+v", photo)
	}
	h.waitRun(job.ID, schedule.StatusDelivered)
	h.wantNoCards("alice")
	if sent := h.sentTexts(); len(sent) != 0 {
		t.Fatalf("a captioned scheduled card also sent text: %q", sent)
	}

	// The report alone would fit a caption; with the scheduled prefix it no longer does.
	report := strings.Repeat("r", 1010)
	if queued, err := h.scheduler.RunNow(h.ctx, "alice", job.ID, ""); err != nil || !queued {
		t.Fatalf("run now: %v %v", queued, err)
	}
	run = receive(t, h.ctx, runtime.calls)
	h.stageCard(run)
	run.answer <- agentruntime.RunResult{Response: report, Final: report}
	if photo := h.nextPhoto(); photo.caption != "" || photo.replyTo != 0 {
		t.Fatalf("photo = %+v, want no caption", photo)
	}
	h.wantMessage(101, "<b>Scheduled: Weekly tasks</b>\n\n"+report)
	for h.countRows(`SELECT count(*) FROM schedule_runs WHERE status = ?`, schedule.StatusDelivered) != 2 {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-h.ctx.Done():
			t.Fatal("the second run was not delivered")
		}
	}
	h.wantNoCards("alice")
}

func TestCardsStayWithTheirPrincipal(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.provision("bob", "acct-bob")
	h.start()

	h.push(textUpdate(1, 101, "plan my day"), textUpdate(2, 202, "hello"))
	calls := map[string]runtimeCall{}
	for range 2 {
		call := receive(t, h.ctx, runtime.calls)
		calls[call.request.PrincipalID] = call
	}
	h.stageCard(calls["alice"])
	if files := h.cardFiles("bob"); len(files) != 0 {
		t.Fatalf("Alice's card reached Bob's outbox: %v", files)
	}
	if _, found, err := h.outbox.Card("bob", outbox.ChatTurn(h.requestID(1))); err != nil || found {
		t.Fatalf("Bob can read Alice's card: found=%v err=%v", found, err)
	}
	calls["bob"].answer <- agentruntime.RunResult{Response: "hi bob"}
	h.wantReply(202, 2, "hi bob")
	h.waitState(2, "delivered")
	h.wantNoPhoto()

	calls["alice"].answer <- agentruntime.RunResult{Response: "Dry all day."}
	if photo := h.nextPhoto(); photo.chatID != 101 || string(photo.data) != string(cardBytes("alice")) {
		t.Fatalf("photo = %+v", photo)
	}
	h.waitState(1, "delivered")
}

func TestStartupSweepAndNewSessionRemoveStagedCards(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	for _, turn := range []string{outbox.ChatTurn("01d0"), outbox.ChatTurn("f2e5"), outbox.ScheduledTurn("0a1b"), outbox.ScheduledTurn("c3d4")} {
		if err := h.outbox.Stage("alice", turn, cardBytes("alice")); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * conversation.CardRetention)
	for _, stale := range []string{"chat/01d0.png", "schedule/0a1b.png"} {
		if err := os.Chtimes(filepath.Join(h.outboxDir, "alice", stale), old, old); err != nil {
			t.Fatal(err)
		}
	}
	h.start()
	if files := h.cardFiles("alice"); !slices.Equal(files, []string{"chat/f2e5.png", "schedule/c3d4.png"}) {
		t.Fatalf("outbox after the startup sweep = %v", files)
	}
	h.push(textUpdate(1, 101, "/new"))
	h.wantReply(101, 1, NoSessionReply)
	if files := h.cardFiles("alice"); !slices.Equal(files, []string{"schedule/c3d4.png"}) {
		t.Fatalf("outbox after /new = %v, want only the scheduled run's card", files)
	}
}

func TestScheduledCardSurvivesANewConversation(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true, schedules: clock})
	h.provision("alice", "acct-alice")
	h.start()
	job := h.createJob("alice", "a1b2c3d4e5f60718", "Weekly tasks", "0 9 * * 1", "Plan my day.")

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	h.stageCard(run)
	h.push(textUpdate(1, 101, "/new"))
	h.wantReply(101, 1, NoSessionReply)
	run.answer <- agentruntime.RunResult{Response: "Dry all day.", Final: "Dry all day."}
	if photo := h.nextPhoto(); photo.caption != "<b>Scheduled: Weekly tasks</b>\n\nDry all day." || string(photo.data) != string(cardBytes("alice")) {
		t.Fatalf("scheduled photo = %+v", photo)
	}
	h.waitRun(job.ID, schedule.StatusDelivered)
	h.wantNoCards("alice")
}

func TestCardMarkSentFailureDeliversTheReplyWithoutASecondPhoto(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "plan my day"))
	call := receive(t, h.ctx, runtime.calls)
	h.stageCard(call)
	// A read-only directory still lets delivery read the card but not record it as sent.
	chat := filepath.Join(h.outboxDir, "alice", "chat")
	if err := os.Chmod(chat, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(chat, 0o700) })
	call.answer <- agentruntime.RunResult{Response: "Dry all day."}
	if photo := h.nextPhoto(); photo.caption != "Dry all day." || photo.replyTo != requestMessageID(1) {
		t.Fatalf("photo = %+v", photo)
	}
	h.waitState(1, "delivered")
	h.wantNoPhoto()
	if sent := h.sentTexts(); len(sent) != 0 {
		t.Fatalf("sent text %q besides the captioned card", sent)
	}
}

func TestUnreadableCardStillDeliversTheReplyAsText(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{cards: true})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "plan my day"))
	call := receive(t, h.ctx, runtime.calls)
	h.stageCard(call)
	if err := os.Chmod(filepath.Join(h.outboxDir, "alice", "chat", h.requestID(1)+".png"), 0); err != nil {
		t.Fatal(err)
	}
	call.answer <- agentruntime.RunResult{Response: "Dry all day."}
	h.wantReply(101, 1, "Dry all day.")
	h.waitState(1, "delivered")
	h.wantNoPhoto()
	h.wantNoCards("alice")
}

func TestCaptionFitsMeasuresTheParsedTextInUTF16(t *testing.T) {
	client, err := NewClient(ClientConfig{BaseURL: "http://telegram.invalid", Token: "999:test-token", PollTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		text string
		fits bool
	}{
		{"bold markers are not counted", "**" + strings.Repeat("a", 1024) + "**", true},
		{"one unit over", strings.Repeat("a", 1025), false},
		{"escaped characters count once", strings.Repeat("&", 1024), true},
		{"astral characters count twice", strings.Repeat("😀", 512), true},
		{"astral characters over", strings.Repeat("😀", 512) + "a", false},
		{"a link counts its label", "[x](https://example.com/" + strings.Repeat("p", 2000) + ")", true},
		{"the scheduled prefix counts", "**Scheduled: Weekly tasks**\n\n" + strings.Repeat("r", 1000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if fits := client.CaptionFits(tc.text); fits != tc.fits {
				t.Fatalf("CaptionFits = %v, want %v", fits, tc.fits)
			}
		})
	}
}
