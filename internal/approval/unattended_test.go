package approval_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

// replyExecutor stands in for the routine-reply send.
type replyExecutor struct{ tool string }

func (e replyExecutor) ActionTools() []string { return []string{e.tool} }

func (replyExecutor) ExecuteApproved(context.Context, approval.Record) (approval.Outcome, error) {
	return approval.Outcome{Text: "Sent.", UserText: "Sent."}, nil
}

// countingDeliverer counts the "Done" messages it is asked to post.
type countingDeliverer struct {
	quietDeliverer
	done int
}

func (d *countingDeliverer) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	d.done++
	return 1, nil
}

// An unattended action is made only for the routine-reply tool, through the executor its caller
// passes, and is recorded as a done direct action without Undo that no "Done" message ever shows.
func TestAnUnattendedActionIsOnlyTheRoutineReplyAndIsRecordedInFull(t *testing.T) {
	t.Parallel()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	deliverer := &countingDeliverer{}
	service, err := approval.NewService(approval.Config{}, store, deliverer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.SetExposure(userChat{})
	arguments, _ := json.Marshal(map[string]string{"reply_to_id": "msg-1", "to": "person@example.org", "subject": "Re: Visit", "body": "Thank you, received."})
	proposal := approval.Proposal{Tool: approval.UnattendedTool, Arguments: arguments, Summary: "Routine reply\nTo: person@example.org\n\nThank you, received."}

	for _, refused := range []struct {
		proposal approval.Proposal
		executor approval.Executor
	}{
		{chooseProposal("Save the note A"), chooseExecutor{}},
		{approval.Proposal{Tool: chooseTool, Arguments: arguments, Summary: "x"}, replyExecutor{tool: chooseTool}},
		{proposal, replyExecutor{tool: chooseTool}},
		{proposal, nil},
	} {
		if _, err := service.ActUnattended(t.Context(), "alice", refused.proposal, refused.executor); err == nil {
			t.Fatalf("%s through %T was made without asking", refused.proposal.Tool, refused.executor)
		}
	}

	record, err := service.ActUnattended(t.Context(), "alice", proposal, replyExecutor{tool: approval.UnattendedTool})
	if err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.LoadApproval(t.Context(), record.ID)
	if err != nil || !found {
		t.Fatalf("record = %v %v", found, err)
	}
	var kept map[string]string
	if err := json.Unmarshal(stored.Arguments, &kept); err != nil {
		t.Fatal(err)
	}
	if stored.Status != approval.StatusDone || stored.Mode != approval.ModeDirect || len(stored.Undo) != 0 || stored.Outcome != "Sent." ||
		kept["body"] != "Thank you, received." || kept["to"] != "person@example.org" || kept["reply_to_id"] != "msg-1" {
		t.Fatalf("stored = %+v, arguments %v", stored, kept)
	}
	if err := service.AfterDelivery(t.Context(), "alice", 101); err != nil {
		t.Fatal(err)
	}
	if deliverer.done != 0 {
		t.Fatalf("%d Done messages showed the unattended action", deliverer.done)
	}
	if undid, err := service.Undo(t.Context(), "alice", record.ID); err != nil || undid.Answer == "" {
		t.Fatalf("undo = %+v %v", undid, err)
	}
	if again, _, _ := store.LoadApproval(t.Context(), record.ID); again.Status != approval.StatusDone {
		t.Fatalf("after Undo = %+v", again)
	}
}
