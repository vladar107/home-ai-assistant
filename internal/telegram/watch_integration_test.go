package telegram

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"testing"

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
