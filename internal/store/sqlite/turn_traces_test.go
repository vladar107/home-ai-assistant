package sqlite

import (
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/turn"
)

// A trace is found by the messages linked to it, by its owner only; a second save under its key adds
// its calls after the first's; and pruning removes traces past their retention with their calls and
// messages, and sooner the checks no delivered message needs.
func TestTurnTracesAreKeptPerMessageAndPruned(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	save := func(key, kind string, finished time.Time, calls ...turn.TraceCall) {
		t.Helper()
		if err := store.SaveTurnTrace(ctx, turn.Trace{PrincipalID: "alice", Key: key, Kind: kind, StartedAt: finished.Add(-time.Minute), FinishedAt: finished, Calls: calls}); err != nil {
			t.Fatal(err)
		}
	}
	link := func(key string, messageID int64) {
		t.Helper()
		if err := store.RecordTurnMessage(ctx, "alice", key, 101, messageID); err != nil {
			t.Fatal(err)
		}
	}
	found := func(principalID string, messageID int64) (turn.Trace, bool) {
		t.Helper()
		trace, ok, err := store.TurnTraceByMessage(ctx, principalID, 101, messageID)
		if err != nil {
			t.Fatal(err)
		}
		return trace, ok
	}

	save("task/t1/1", turn.TraceTask, now.Add(-time.Hour), turn.TraceCall{CallID: "a", Tool: "web_search", Outcome: "success", Label: "first"})
	save("task/t1/1", turn.TraceTask, now, turn.TraceCall{CallID: "b", Tool: "fetch_page", Outcome: "failed", Duration: 1500 * time.Millisecond, ResultBytes: 12})
	link("task/t1/1", 11)
	trace, ok := found("alice", 11)
	if !ok || trace.Kind != turn.TraceTask || !trace.FinishedAt.Equal(now) || len(trace.Calls) != 2 ||
		trace.Calls[0].Sequence != 1 || trace.Calls[0].Label != "first" ||
		trace.Calls[1].Sequence != 2 || trace.Calls[1].Outcome != "failed" || trace.Calls[1].Duration != 1500*time.Millisecond || trace.Calls[1].ResultBytes != 12 {
		t.Fatalf("trace = %+v %v", trace, ok)
	}
	if _, ok := found("bob", 11); ok {
		t.Fatal("bob found alice's trace")
	}
	// A message whose key has no trace, such as a reminder's, is not linked.
	link("schedule/none", 12)
	if _, ok := found("alice", 12); ok {
		t.Fatal("a message without a trace was linked")
	}

	save("chat/old", turn.TraceChat, now.Add(-31*24*time.Hour), turn.TraceCall{Tool: "web_search", Outcome: "success"})
	link("chat/old", 21)
	save("check/unlinked", turn.TraceCheck, now.Add(-3*24*time.Hour))
	save("check/linked", turn.TraceCheck, now.Add(-3*24*time.Hour))
	link("check/linked", 22)
	save("check/recent", turn.TraceCheck, now.Add(-time.Hour))
	if err := store.PruneTurnTraces(ctx, now.Add(-30*24*time.Hour), now.Add(-2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := found("alice", 21); ok {
		t.Fatal("a trace older than 30 days was kept")
	}
	if _, ok := found("alice", 22); !ok {
		t.Fatal("a linked check's trace was pruned")
	}
	var left int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM turn_traces WHERE trace_key IN ('check/unlinked', 'check/recent')`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("unlinked check traces left = %d %v, want only the recent one", left, err)
	}
	var orphans int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM turn_trace_calls WHERE trace_key = 'chat/old') + (SELECT count(*) FROM turn_messages WHERE trace_key = 'chat/old')`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("calls and messages of a pruned trace = %d %v", orphans, err)
	}
}
