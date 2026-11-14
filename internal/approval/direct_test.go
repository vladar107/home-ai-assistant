package approval_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
)

type userChat struct{}

func (userChat) UserChatRequest(context.Context) (string, bool) { return "request-1", true }
func (userChat) External(context.Context, string) bool          { return false }
func (userChat) Exposed(context.Context, string) bool           { return false }

// cancellingDeliverer ends the delivery the way a newer turn of the tab does, right after the
// "Done" message went out.
type cancellingDeliverer struct {
	quietDeliverer
	cancel context.CancelFunc
	sent   int
}

func (d *cancellingDeliverer) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	d.sent++
	d.cancel()
	return 7, nil
}

// A "Done" message that went out is recorded even when the delivery is cancelled right after the
// send, so the next delivery does not post it again.
func TestDoneMessageIsRecordedWhenTheDeliveryIsCancelled(t *testing.T) {
	t.Parallel()
	store := testutil.OpenStore(t, filepath.Join(t.TempDir(), "assistant.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BootstrapConfiguredPrincipals(t.Context(), []identity.ConfiguredPrincipal{{ID: "alice", DisplayName: "Alice", TelegramUserID: 101}}); err != nil {
		t.Fatal(err)
	}
	deliverer := &cancellingDeliverer{}
	service, err := approval.NewService(approval.Config{}, store, deliverer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterExecutor(chooseExecutor{}); err != nil {
		t.Fatal(err)
	}
	service.SetExposure(userChat{})
	if text, err := service.Act(t.Context(), "alice", chooseProposal("Save the note A")); err != nil {
		t.Fatalf("act = %q %v", text, err)
	}

	delivery, cancel := context.WithCancel(t.Context())
	deliverer.cancel = cancel
	_ = service.AfterDelivery(delivery, "alice", 101)
	if err := service.AfterDelivery(t.Context(), "alice", 101); err != nil {
		t.Fatal(err)
	}
	if deliverer.sent != 1 {
		t.Fatalf("%d Done messages sent, want the one", deliverer.sent)
	}
}
