package looseend_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

type switchedOn struct{}

func (switchedOn) Enabled(context.Context, string) (bool, error) { return true, nil }

type keptNudges struct{ sent [][]looseend.DueItem }

func (n *keptNudges) Nudge(_ context.Context, _ string, items []looseend.DueItem) (string, schedule.Sent, error) {
	n.sent = append(n.sent, items)
	return "Loose ends", schedule.Sent{}, nil
}

type noReports struct{}

func (noReports) SaveSentReport(context.Context, schedule.Report) error { return nil }

// slowThreads is a thread listing that never answers before its deadline, and keeps the deadline
// each lookup had.
type slowThreads struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (s *slowThreads) ListThread(ctx context.Context, _, _, _ string) ([]gmail.ThreadMessage, error) {
	deadline, _ := ctx.Deadline()
	s.mu.Lock()
	s.deadlines = append(s.deadlines, deadline)
	s.mu.Unlock()
	return nil, context.DeadlineExceeded
}

func runJob(t *testing.T, f *fixture, cfg looseend.JobConfig) schedule.Result {
	t.Helper()
	cfg.Switch, cfg.Reports, cfg.Now = switchedOn{}, noReports{}, func() time.Time { return f.now }
	jobType, err := f.service.JobType(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := jobType.Handler.Run(t.Context(), schedule.Occurrence{RunID: "run-1", PrincipalID: "alice", JobID: "job-1", JobName: "Loose ends", Location: amsterdam})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A switch that is on without a scan state, as a restore leaves it, gets one from the next run,
// and the scan reads its backfill at once.
func TestTheJobStartsAMissingScanStateAndScans(t *testing.T) {
	f := newScanFixture(t)
	f.deliver("b1", "t1", "orders@shop.example", "Your order: return within 30 days", 2*time.Hour)
	f.model.respond = func(ctx context.Context, prompt string) string { return reply(f.openOffered(ctx, prompt)...) }
	if result := runJob(t, f.fixture, looseend.JobConfig{Nudger: &keptNudges{}, Scanner: f.scan}); result.ErrorKind != "" {
		t.Fatalf("result = %+v", result)
	}
	if _, found, err := f.service.ScanState(t.Context(), "alice"); err != nil || !found {
		t.Fatalf("scan state after the run = %v %v", found, err)
	}
	if items := f.openItems(); len(f.model.prompts) != 1 || len(items) != 1 || items[0].SourceRef != "b1" {
		t.Fatalf("turns %d, items %+v", len(f.model.prompts), items)
	}
}

// The thread lookups of one nudge message share one short deadline, and a lookup that misses it
// only leaves its note out.
func TestThreadLookupsOfOneNudgeShareOneShortDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, id := range []string{"p1", "p2"} {
		f.created("alice", looseend.Draft{
			Kind: looseend.KindPromise, Title: "Send the form " + id, Counterpart: "partner.example", Due: "2026-12-01T18:00",
			Source: looseend.SourceSent, Account: "user@example.com", SourceRef: id, ThreadRef: "t-" + id,
		})
	}
	threads, nudges := &slowThreads{}, &keptNudges{}
	before := time.Now()
	runJob(t, f, looseend.JobConfig{Nudger: nudges, Threads: threads})
	if len(threads.deadlines) != 2 || threads.deadlines[0].IsZero() || !threads.deadlines[0].Equal(threads.deadlines[1]) ||
		threads.deadlines[0].After(before.Add(10*time.Second+time.Second)) {
		t.Fatalf("lookup deadlines = %v, want one shared deadline within 10 s", threads.deadlines)
	}
	if len(nudges.sent) != 1 || len(nudges.sent[0]) != 2 || slices.ContainsFunc(nudges.sent[0], func(item looseend.DueItem) bool { return !item.WroteAgain.IsZero() }) {
		t.Fatalf("nudges = %+v", nudges.sent)
	}
}
