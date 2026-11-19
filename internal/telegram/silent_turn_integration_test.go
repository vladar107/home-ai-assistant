package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// settle is how long a test waits to see that nothing more follows a delivered turn.
const settle = 300 * time.Millisecond

func newSilentTurnHarness(t *testing.T) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: writableNotionSimulator(t)})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()
	h.enableNotionWrites(1, 2, 101)
	return h, runtime
}

// failWrite makes a write call of the turn that fails, as a write to a page the integration
// cannot find does.
func (h *conversationHarness) failWrite(call runtimeCall) {
	h.t.Helper()
	result := executeTool(h.t, call.ctx, call.request.Tools, "notion_append_blocks", `{"page_id":"page-missing","paragraphs":["Bread"]}`)
	if !result.IsError {
		h.t.Fatalf("write to a missing page = %+v", result)
	}
}

// A turn that only proposes lets the card speak: its silent reply sends the card alone, even after
// a write that failed before the proposal was staged.
func TestASilentProposalTurnSendsOnlyTheCard(t *testing.T) {
	t.Parallel()
	h, runtime := newSilentTurnHarness(t)

	h.push(textUpdate(3, 101, "add bread to groceries"))
	call := receive(t, h.ctx, runtime.calls)
	h.failWrite(call)
	if proposed := executeTool(t, call.ctx, call.request.Tools, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Bread"]}`); proposed.IsError {
		t.Fatalf("proposal = %+v", proposed)
	}
	call.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	card := h.nextMessage()
	if !strings.Contains(card.text, "Bread") || !strings.Contains(card.text, "Expires in") {
		t.Fatalf("first message of a silent proposal turn = %+v, want the card", card)
	}
	h.waitState(3, "delivered")
	h.expectNoMessage(settle)
}

// A silent reply cannot hide a failed write: a turn that left no card answers with the failed
// change's reply, in chat, after a successful reaction, and when continuing after a confirmation.
func TestASilentReplyAfterAFailedWriteSaysTheChangeFailed(t *testing.T) {
	t.Parallel()
	h, runtime := newSilentTurnHarness(t)
	failed := localetest.English(conversation.ChangeFailedReply)

	h.push(textUpdate(3, 101, "thanks, and add bread to the shopping page"))
	call := receive(t, h.ctx, runtime.calls)
	react(t, call, "👍")
	h.failWrite(call)
	call.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	h.wantReply(101, 3, failed)
	h.waitState(3, "delivered")
	h.expectNoMessage(settle)

	proposal := h.proposeTurn(runtime, 4, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Eggs"]}`)
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(5, 101, proposal.messageID, confirm))
	h.wantAnswer(5)
	h.nextEdit()
	continued := receive(t, h.ctx, runtime.calls)
	if !strings.HasSuffix(continued.request.Message, prompt.Continue) {
		t.Fatalf("continuation message = %q", continued.request.Message)
	}
	h.failWrite(continued)
	continued.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	if reply := h.nextMessage(); reply.text != failed {
		t.Fatalf("continuation reply = %+v, want %q", reply, failed)
	}
	h.waitState(5, "delivered")
	h.expectNoMessage(settle)
}

// A turn that withdrew everything it proposed tells the user nothing waits instead of going silent.
func TestASilentReplyAfterWithdrawingEveryProposalSaysNothingWaits(t *testing.T) {
	t.Parallel()
	h, runtime := newSilentTurnHarness(t)

	h.push(textUpdate(3, 101, "add bread to groceries"))
	call := receive(t, h.ctx, runtime.calls)
	if proposed := executeTool(t, call.ctx, call.request.Tools, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Bread"]}`); proposed.IsError {
		t.Fatalf("proposal = %+v", proposed)
	}
	if withdrawn := executeTool(t, call.ctx, call.request.Tools, "proposal_withdraw", `{"all":true}`); withdrawn.IsError {
		t.Fatalf("withdraw = %+v", withdrawn)
	}
	call.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	h.wantReply(101, 3, localetest.English(conversation.NothingWaitingReply))
	h.waitState(3, "delivered")
	h.expectNoMessage(settle)
}

// actingNotes offers note_write as a write tool that acts at once in a clean turn; a note named
// "broken" fails when it runs.
type actingNotes struct{ approvals *approval.Service }

func (p *actingNotes) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Definitions: []tools.Definition{{Name: noteTool, Description: "Write a note.", InputSchema: json.RawMessage(`{"type":"object"}`), Risk: tools.RiskWrite}}}, nil
}

func (p *actingNotes) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	text, err := p.approvals.Act(ctx, principalID, approval.Proposal{Tool: noteTool, Arguments: call.Arguments, Summary: "Save a note"})
	var failed *approval.FailedError
	if errors.As(err, &failed) {
		return tools.Result{Text: failed.Text, IsError: true}, nil
	}
	return tools.Result{Text: text}, err
}

type brokenNotes struct{ noteBook }

func (b *brokenNotes) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	if strings.Contains(string(record.Arguments), "broken") {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "The note store is down."}
	}
	return b.noteBook.ExecuteApproved(ctx, record)
}

// A direct action that failed leaves no "Done" message, so the silent reply becomes the failed
// change's reply, once; one that succeeded is shown by its message and stays silent.
func TestASilentReplyAfterAFailedDirectActionSaysTheChangeFailedOnce(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	notes := &actingNotes{}
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{providers: []tools.Provider{notes}})
	h.provision("alice", "acct-alice-1111")
	notes.approvals = h.approvals
	if err := h.approvals.RegisterExecutor(&brokenNotes{noteBook{notes: make(map[string]string)}}); err != nil {
		t.Fatal(err)
	}
	h.start()

	h.push(textUpdate(1, 101, "save a broken note"))
	call := receive(t, h.ctx, runtime.calls)
	if result := executeTool(t, call.ctx, call.request.Tools, noteTool, `{"name":"broken"}`); !result.IsError {
		t.Fatalf("failing direct action = %+v", result)
	}
	call.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	h.wantReply(101, 1, localetest.English(conversation.ChangeFailedReply))
	h.waitState(1, "delivered")
	h.expectNoMessage(settle)

	h.push(textUpdate(2, 101, "save a grocery note"))
	call = receive(t, h.ctx, runtime.calls)
	if result := executeTool(t, call.ctx, call.request.Tools, noteTool, `{"name":"broken"}`); !result.IsError {
		t.Fatalf("failing direct action = %+v", result)
	}
	if result := executeTool(t, call.ctx, call.request.Tools, noteTool, `{"name":"groceries"}`); result.IsError {
		t.Fatalf("direct action = %+v", result)
	}
	call.answer <- agentruntime.RunResult{Response: prompt.NothingToAdd}
	if done := h.nextMessage(); !strings.HasPrefix(done.text, "Done:") {
		t.Fatalf("message after a silent direct action = %+v, want its Done message", done)
	}
	h.waitState(2, "delivered")
	h.expectNoMessage(settle)
}
