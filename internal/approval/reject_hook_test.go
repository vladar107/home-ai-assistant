package approval_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

const rejectTool = "reject_write"

// rejectExecutor keeps the records it was told were rejected.
type rejectExecutor struct {
	mu       sync.Mutex
	rejected []approval.Record
	fail     bool
}

func (*rejectExecutor) ActionTools() []string { return []string{rejectTool} }

func (*rejectExecutor) ExecuteApproved(context.Context, approval.Record) (approval.Outcome, error) {
	return approval.Outcome{Text: "Saved."}, nil
}

func (e *rejectExecutor) Rejected(_ context.Context, record approval.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rejected = append(e.rejected, record)
	if e.fail {
		return errors.New("hook failed")
	}
	return nil
}

func (e *rejectExecutor) rejectedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	for _, record := range e.rejected {
		ids = append(ids, record.ID)
	}
	return ids
}

func newRejectFixture(t *testing.T, cfg approval.Config) (*languageFixture, *rejectExecutor) {
	t.Helper()
	f := newLanguageFixture(t, cfg)
	executor := &rejectExecutor{}
	if err := f.service.RegisterExecutor(executor); err != nil {
		t.Fatal(err)
	}
	return f, executor
}

func rejectProposal(summary string) approval.Proposal {
	proposal := languageProposal(summary)
	proposal.Tool = rejectTool
	return proposal
}

// An explicit reject tells the executor, even when its hook fails; a proposal that is replaced by a
// newer one, or expires, does not.
func TestRejectHookFiresOnExplicitRejectOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	f, executor := newRejectFixture(t, approval.Config{})
	executor.fail = true
	replaced, err := f.service.Propose(ctx, "bob", rejectProposal("First"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.AfterDelivery(ctx, "bob", 202); err != nil {
		t.Fatal(err)
	}
	rejected, err := f.service.Propose(ctx, "bob", rejectProposal("Second"))
	if err != nil {
		t.Fatal(err)
	}
	if ids := executor.rejectedIDs(); len(ids) != 0 {
		t.Fatalf("hook fired for a replaced proposal: %v", ids)
	}
	if stored, _, err := f.store.LoadApproval(ctx, replaced.ID); err != nil || stored.Reason != approval.ReasonReplaced {
		t.Fatalf("first proposal = %+v %v, want replaced", stored, err)
	}
	if err := f.service.AfterDelivery(ctx, "bob", 202); err != nil {
		t.Fatal(err)
	}
	pressed, err := f.service.Press(ctx, "bob", approval.ActionReject, rejected.ID)
	if err != nil || !pressed.Owned {
		t.Fatalf("press = %+v %v", pressed, err)
	}
	if ids := executor.rejectedIDs(); len(ids) != 1 || ids[0] != rejected.ID {
		t.Fatalf("hook records = %v, want only %s", ids, rejected.ID)
	}
	if stored, _, err := f.store.LoadApproval(ctx, rejected.ID); err != nil || stored.Status != approval.StatusRejected {
		t.Fatalf("stored = %+v %v, want rejected despite the failing hook", stored, err)
	}

	now := time.Now()
	expiring, expiringExecutor := newRejectFixture(t, approval.Config{Now: func() time.Time { return now }})
	record, err := expiring.service.Propose(ctx, "bob", rejectProposal("Expiring"))
	if err != nil {
		t.Fatal(err)
	}
	if err := expiring.service.AfterDelivery(ctx, "bob", 202); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := expiring.service.Press(ctx, "bob", approval.ActionReject, record.ID); err != nil {
		t.Fatal(err)
	}
	if stored, _, err := expiring.store.LoadApproval(ctx, record.ID); err != nil || stored.Status != approval.StatusExpired {
		t.Fatalf("stored = %+v %v, want expired", stored, err)
	}
	if ids := expiringExecutor.rejectedIDs(); len(ids) != 0 {
		t.Fatalf("hook fired for an expired proposal: %v", ids)
	}
}
