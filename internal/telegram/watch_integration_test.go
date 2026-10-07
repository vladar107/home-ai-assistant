package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// stubEvaluator answers every check with the observation currently set.
type stubEvaluator struct {
	mu          sync.Mutex
	observation watch.Observation
}

func (s *stubEvaluator) set(observation watch.Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observation = observation
}

func (s *stubEvaluator) Evaluate(context.Context, string, watch.Watch, watch.Occurrence) (watch.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observation, nil
}

// trainWatch is a weekday departure whose window contains mondayMorning.
func trainWatch() watch.Spec {
	return watch.Spec{
		Kind:         watch.KindDeparture,
		Target:       watch.Target{Mode: watch.ModeTrain, From: "Utrecht Centraal", To: "Amsterdam Centraal"},
		Triggers:     []string{watch.TriggerCancelled, watch.TriggerPartlyCancelled, watch.TriggerDelay, watch.TriggerPlatform},
		DelayMinutes: 5,
		Days:         []string{"mon", "tue", "wed", "thu", "fri"},
		Start:        "07:00",
		End:          "08:30",
		Zone:         "Europe/Amsterdam",
	}
}

func (h *conversationHarness) createWatch(principalID, id string, spec watch.Spec) {
	h.t.Helper()
	now := h.scheduler.Now()
	created, err := h.store.CreateWatch(h.ctx, watch.Watch{ID: id, PrincipalID: principalID, Spec: spec, Enabled: true, CreatedAt: now, UpdatedAt: now}, watch.MaximumWatchesPerPrincipal)
	if err != nil || !created {
		h.t.Fatalf("create watch: %v %v", created, err)
	}
}

func (h *conversationHarness) watchState(id string) (bool, string) {
	h.t.Helper()
	var enabled bool
	var muted sql.NullString
	if err := h.db.QueryRowContext(h.ctx, `SELECT enabled, muted_on FROM watches WHERE id = ?`, id).Scan(&enabled, &muted); err != nil {
		h.t.Fatal(err)
	}
	return enabled, muted.String
}

// An alert reaches its owner with Mute today and Pause buttons; another user's press changes
// nothing, a press mutes or pauses the owner's watch, and a repeated press is harmless.
func TestWatchAlertButtonsMuteAndPause(t *testing.T) {
	clock := newTestClock(t, mondayMorning)
	evaluator := &stubEvaluator{}
	evaluator.set(watch.Observation{State: "cancelled", Triggered: true, Headline: "It is cancelled."})
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{schedules: clock, watches: evaluator})
	h.start()
	const id = "00000000000000a1"
	h.createWatch("alice", id, trainWatch())

	h.watches.Cycle(h.ctx)
	alert := h.nextMessage()
	want := []string{"watch:mute:" + id, "watch:pause:" + id}
	if alert.chatID != 101 || !strings.Contains(alert.text, "08:30 train Utrecht Centraal → Amsterdam Centraal") ||
		!strings.Contains(alert.text, "It is cancelled.") || !slices.Equal(buttonData(alert.keyboard), want) {
		t.Fatalf("alert = %+v", alert)
	}

	h.push(callbackUpdate(1, 202, alert.messageID, "watch:mute:"+id))
	h.wantAnswer(1)
	if edit := h.nextEdit(); !strings.Contains(edit.text, WatchGoneReply) {
		t.Fatalf("bob's press = %+v", edit)
	}
	if enabled, muted := h.watchState(id); !enabled || muted != "" {
		t.Fatalf("bob's press changed alice's watch: enabled=%v muted=%q", enabled, muted)
	}

	for update := int64(2); update <= 3; update++ {
		h.push(callbackUpdate(update, 101, alert.messageID, "watch:mute:"+id))
		h.wantAnswer(update)
		if edit := h.nextEdit(); !strings.Contains(edit.text, WatchMutedReply) || len(buttonData(edit.keyboard)) != 0 {
			t.Fatalf("mute press %d = %+v", update, edit)
		}
	}
	if _, muted := h.watchState(id); muted != "2026-09-28" {
		t.Fatalf("muted on %q", muted)
	}

	h.push(callbackUpdate(4, 101, alert.messageID, "watch:pause:"+id))
	h.wantAnswer(4)
	if edit := h.nextEdit(); !strings.Contains(edit.text, WatchPausedReply) {
		t.Fatalf("pause press = %+v", edit)
	}
	if enabled, _ := h.watchState(id); enabled {
		t.Fatal("the watch was not paused")
	}
}

// runTool runs one tool in a chat turn of the given user and answers the turn.
func (h *conversationHarness) runTool(runtime *controlledRuntime, updateID, userID int64, name, arguments string) agentruntime.ToolResult {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "about my watches"))
	call := receive(h.t, h.ctx, runtime.calls)
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		h.t.Fatal(err)
	}
	call.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(userID, updateID, "done")
	h.waitState(updateID, "delivered")
	return result
}

// A watch is proposed in conversation with a summary built from the parsed watch, created once on
// Confirm, listed and managed in /jobs, and never visible or changeable for another user.
func TestWatchIsProposedConfirmedAndManagedByItsOwnerOnly(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, watches: &stubEvaluator{}})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	h.push(textUpdate(1, 101, "tell me if my 08:30 to Amsterdam is cancelled or late on weekdays"))
	chat := receive(t, h.ctx, runtime.calls)
	names := toolNames(chat.request.Tools)
	for _, name := range []string{"watch_list", "watch_propose", "watch_update", "watch_remove"} {
		if !slices.Contains(names, name) {
			t.Fatalf("chat tools = %v", names)
		}
	}
	if !strings.Contains(chat.request.Tools.Guidance, "instead of a scheduled report") {
		t.Fatalf("guidance = %q", chat.request.Tools.Guidance)
	}
	for _, invalid := range []string{
		`{"kind":"departure","mode":"train","from":"Utrecht Centraal","to":"Amsterdam Centraal","days":["mon"]}`,
		`{"kind":"route","route":"work","departure":"08:30","days":["mon"]}`,
		`{"kind":"departure","mode":"train","from":"Utrecht Centraal","to":"Amsterdam Centraal","departure":"08:30"}`,
	} {
		result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: "watch_propose", Arguments: json.RawMessage(invalid)})
		if err != nil || !result.IsError {
			t.Fatalf("invalid proposal %s = %+v %v", invalid, result, err)
		}
	}
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: "watch_propose", Arguments: json.RawMessage(
		`{"kind":"departure","mode":"train","from":"Utrecht Centraal","to":"Amsterdam Centraal","departure":"08:30","days":["fri","mon","tue","wed","thu"]}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "awaiting the user's confirmation") {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	for _, want := range []string{
		"Watch 08:30 train Utrecht Centraal → Amsterdam Centraal",
		"Days: weekdays",
		"Window: 07:00–08:30 (Europe/Amsterdam)",
		"Tells you about: a cancellation, a partial cancellation, a delay of 5 minutes or more, a platform change",
	} {
		if !strings.Contains(proposal.text, want) {
			t.Fatalf("proposal lacks %q: %s", want, proposal.text)
		}
	}
	if count := h.countRows(`SELECT count(*) FROM watches`); count != 0 {
		t.Fatalf("a proposal created %d watches", count)
	}
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	if edit := h.nextEdit(); !strings.Contains(edit.text, "Watching. Checking now until 08:30.") {
		t.Fatalf("proposal after confirm = %+v", edit)
	}
	h.continueTurn(runtime, 2)
	var id string
	if err := h.db.QueryRowContext(h.ctx, `SELECT id FROM watches WHERE principal_id = 'alice'`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	if listed := h.runTool(runtime, 3, 202, "watch_list", `{}`); !strings.HasSuffix(listed.Text, "The user has no watches.") {
		t.Fatalf("bob's list = %+v", listed)
	}
	if result := h.runTool(runtime, 4, 202, "watch_remove", `{"watch_id":"`+id+`"}`); !result.IsError || !strings.Contains(result.Text, "No watch has that id") {
		t.Fatalf("bob's removal = %+v", result)
	}
	if result := h.runTool(runtime, 5, 202, "watch_update", `{"watch_id":"`+id+`","departure":"09:00"}`); !result.IsError || !strings.Contains(result.Text, "No watch has that id") {
		t.Fatalf("bob's change = %+v", result)
	}
	if listed := h.runTool(runtime, 6, 101, "watch_list", `{}`); !strings.Contains(listed.Text, "id "+id+": departure watch, 08:30 train Utrecht Centraal → Amsterdam Centraal") {
		t.Fatalf("alice's list = %+v", listed)
	}
	if result := h.runTool(runtime, 7, 101, "watch_update", `{"watch_id":"`+id+`","departure":"09:00","minutes_before":30,"triggers":["cancelled"]}`); result.IsError {
		t.Fatalf("alice's change = %+v", result)
	}
	if change := h.nextMessage(); !strings.Contains(change.text, "Change the watch 08:30 train Utrecht Centraal → Amsterdam Centraal to:\n09:00 train") ||
		!strings.Contains(change.text, "Window: 08:30–09:00") || !strings.Contains(change.text, "Tells you about: a cancellation\n") {
		t.Fatalf("change proposal = %q", change.text)
	}

	jobs := h.requestJobs(8)
	if !strings.Contains(jobs.text, "1. 👁 08:30 train Utrecht Centraal → Amsterdam Centraal\nweekdays, 07:00–08:30") {
		t.Fatalf("jobs = %q", jobs.text)
	}
	if data := buttonData(jobs.keyboard); !slices.Equal(data, []string{"jobs:wpause:" + id + ":1", "jobs:wdelete:" + id + ":1"}) {
		t.Fatalf("watch buttons = %v", data)
	}

	h.push(callbackUpdate(9, 202, jobs.messageID, "jobs:wdelete:"+id+":1"))
	h.wantAnswer(9)
	if edit := h.nextEdit(); edit.text != NoJobsReply {
		t.Fatalf("bob's list after his press = %+v", edit)
	}
	h.wantMessage(202, WatchGoneReply)
	if count := h.countRows(`SELECT count(*) FROM watches`); count != 1 {
		t.Fatalf("bob's press deleted alice's watch: %d left", count)
	}

	paused := h.pressJob(10, jobs.messageID, "jobs:wpause:"+id+":1")
	if !strings.Contains(paused.text, "weekdays, 07:00–08:30, paused") || buttonData(paused.keyboard)[0] != "jobs:wresume:"+id+":1" {
		t.Fatalf("after pause = %+v", paused)
	}
	if deleted := h.pressJob(11, jobs.messageID, "jobs:wdelete:"+id+":1"); deleted.text != NoJobsReply {
		t.Fatalf("after delete = %+v", deleted)
	}
	if count := h.countRows(`SELECT count(*) FROM watches`); count != 0 {
		t.Fatalf("watches after delete = %d", count)
	}
}
