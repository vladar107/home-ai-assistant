package telegram

import (
	"net/http"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// longReport splits into three chunks, one per letter.
var longReport = strings.Join([]string{strings.Repeat("a", 3000), strings.Repeat("b", 3000), strings.Repeat("c", 3000)}, "\n\n")

// wantChunks expects the next sends to hold the chunks of longReport with the given letters, in
// order, and returns their statuses.
func (h *conversationHarness) wantChunks(letters ...string) []int {
	h.t.Helper()
	var statuses []int
	for _, letter := range letters {
		message := h.nextMessage()
		if !strings.Contains(message.text, strings.Repeat(letter, 3000)) {
			h.t.Fatalf("sent %.40q, want the chunk of %q", message.text, letter)
		}
		statuses = append(statuses, message.status)
	}
	return statuses
}

func TestScheduledReportResumesWithTheChunkThatFailed(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock})
	h.provision("alice", "account-alice")
	job := h.createJob("alice", "00000000000000ee", "Digest", "0 9 * * *", "Digest.")
	run := schedule.Run{ID: "00000000000000ff", JobID: job.ID, PrincipalID: "alice", ScheduledFor: clock.Now()}
	if err := h.store.QueueScheduledRun(h.ctx, run); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{schedule.StatusQueued, schedule.StatusRunning}, {schedule.StatusRunning, schedule.StatusReady}} {
		if ok, err := h.store.TransitionScheduledRun(h.ctx, run.ID, step[0], step[1], longReport, "", clock.Now()); err != nil || !ok {
			t.Fatalf("move run to %s: %v %v", step[1], ok, err)
		}
	}
	h.sendStatuses = []int{http.StatusOK, http.StatusInternalServerError}

	h.start()
	if statuses := h.wantChunks("a", "b", "b", "c"); statuses[1] != http.StatusInternalServerError {
		t.Fatalf("send statuses = %v", statuses)
	}
	h.waitRun(job.ID, schedule.StatusDelivered)
	// The report names every chunk it was sent as, the one sent before the failure too.
	if count := h.countRows(`SELECT count(*) FROM job_report_messages WHERE report_id = ?`, run.ID); count != 3 {
		t.Fatalf("report kept %d messages, want 3", count)
	}
}

func TestTaskReportResumesWithTheChunkThatFailed(t *testing.T) {
	runtime := newControlledRuntime()
	cfg := testTasks()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{tasks: cfg})
	h.provision("alice", "account-alice")
	h.start()

	chat, status, _ := h.startTask(runtime, 1, "Long read", "Read everything and report.")
	run := receive(t, h.ctx, runtime.calls)
	h.wantStatusEdit(status, "Working on: Long read", true)
	chat.answer <- agentruntime.RunResult{Response: "On it."}
	h.wantReply(101, 1, "On it.")
	h.waitState(1, "delivered")

	h.mu.Lock()
	h.sendStatuses = []int{http.StatusOK, http.StatusInternalServerError}
	h.mu.Unlock()
	run.answer <- agentruntime.RunResult{Response: longReport, Final: longReport}
	if statuses := h.wantChunks("a", "b"); statuses[1] != http.StatusInternalServerError {
		t.Fatalf("send statuses = %v", statuses)
	}
	// The retry waits several seconds, so a restart picks the delivery up instead.
	h.restartTasks(*cfg)
	h.wantChunks("b", "c")
	h.wantStatusEdit(status, "Done: Long read", false)
}
