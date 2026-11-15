package approval_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// expiringRepository has another path expire a record just before the service finishes it.
type expiringRepository struct {
	approval.Repository
	before func(id string)
}

func (r expiringRepository) FinishApproval(ctx context.Context, id, from, status, reason, outcome, userOutcome string) (bool, error) {
	if r.before != nil {
		r.before(id)
	}
	return r.Repository.FinishApproval(ctx, id, from, status, reason, outcome, userOutcome)
}

// A record that a concurrent replace or expiry finished before the reject reached the store is not
// rejected by the user, so the hook is not told.
func TestRejectHookSkipsARecordFinishedElsewhereFirst(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newLanguageFixture(t, approval.Config{})
	raced := false
	repository := expiringRepository{Repository: f.store}
	repository.before = func(id string) {
		if raced {
			return
		}
		raced = true
		if _, err := f.store.FinishApproval(ctx, id, approval.StatusPending, approval.StatusExpired, approval.ReasonReplaced, "", ""); err != nil {
			t.Error(err)
		}
	}
	service, err := approval.NewService(approval.Config{}, repository, quietDeliverer{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	executor := &rejectExecutor{}
	if err := service.RegisterExecutor(executor); err != nil {
		t.Fatal(err)
	}
	record, err := service.Propose(ctx, "bob", rejectProposal("Raced"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Press(ctx, "bob", approval.ActionReject, record.ID); err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("the record was not finished by the other path")
	}
	if ids := executor.rejectedIDs(); len(ids) != 0 {
		t.Fatalf("hook fired for a record another path expired: %v", ids)
	}
}

// A change the user leaves out of a Confirm is rejected explicitly and tells the executor, with its
// final status; the kept change does not.
func TestRejectHookFiresForAChangeLeftOutOfAConfirm(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f, executor := newRejectFixture(t, approval.Config{})
	records, err := f.service.ProposeAll(ctx, "bob", []approval.Proposal{rejectProposal("Kept"), rejectProposal("Left out")})
	if err != nil || len(records) != 2 {
		t.Fatalf("proposals = %+v %v", records, err)
	}
	if err := f.service.AfterDelivery(ctx, "bob", 202); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PressKept(ctx, "bob", records[0].ID, []bool{true, false}); err != nil {
		t.Fatal(err)
	}
	if ids := executor.rejectedIDs(); len(ids) != 1 || ids[0] != records[1].ID {
		t.Fatalf("hook records = %v, want only %s", ids, records[1].ID)
	}
	if executor.rejected[0].Status != approval.StatusRejected || executor.rejected[0].Reason != approval.ReasonLeftOut {
		t.Fatalf("hook record = %+v, want rejected as left out", executor.rejected[0])
	}
}
