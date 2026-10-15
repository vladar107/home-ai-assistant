package telegram

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

// react calls the turn's react tool and fails the test unless the reaction was set.
func react(t *testing.T, call runtimeCall, emoji string) {
	t.Helper()
	result, err := call.request.Tools.Executor.Execute(call.ctx, agentruntime.ToolCall{Name: "react", Arguments: json.RawMessage(`{"emoji":"` + emoji + `"}`)})
	if err != nil || result.IsError || !strings.Contains(result.Text, "The reaction is set") {
		t.Fatalf("react %s = %+v %v", emoji, result, err)
	}
}

// A turn that only reacts sends no text, yet is delivered and still sends the proposals it made.
func TestReactOnlyTurnSendsNothingAndStillRunsTheFollowUp(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "got it, and check my train at 9"))
	chat := receive(t, h.ctx, runtime.calls)
	if !slices.Contains(toolNames(chat.request.Tools), "react") || !strings.Contains(chat.request.Tools.Guidance, "react and then reply with exactly "+conversation.NothingToAdd) {
		t.Fatalf("chat turn tools = %v, guidance %q", toolNames(chat.request.Tools), chat.request.Tools.Guidance)
	}
	react(t, chat, "👍")
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name:      "schedule_propose",
		Arguments: json.RawMessage(`{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my 9:30 train delayed?"}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(1, "delivered")
	if proposal := h.nextMessage(); proposal.replyTo != 0 || !strings.Contains(proposal.text, "Runs once: Mon 28 Sep 09:00 CEST") {
		t.Fatalf("follow-up message = %+v", proposal)
	}
	if sent := h.sentTexts(); len(sent) != 1 {
		t.Fatalf("a react-only turn sent %d messages: %q", len(sent), sent)
	}
	if got := h.setReactions(); !slices.Equal(got, []setReaction{{chatID: 101, messageID: requestMessageID(1), emoji: "👍"}}) {
		t.Fatalf("reactions = %+v", got)
	}
	if count := h.reactionCount("👍"); count != 1 {
		t.Fatalf("counted %d 👍 reactions, want 1", count)
	}
}

// A reaction can go with a reply; a later reaction replaces it, and only the curated emoji are set.
func TestReactionWithReplySendsBoth(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "I passed my exam!"))
	chat := receive(t, h.ctx, runtime.calls)
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{Name: "react", Arguments: json.RawMessage(`{"emoji":"👎"}`)})
	if err != nil || !result.IsError {
		t.Fatalf("react with an emoji outside the set = %+v %v", result, err)
	}
	react(t, chat, "🎉")
	react(t, chat, "❤")
	chat.answer <- agentruntime.RunResult{Response: "Congratulations!"}
	h.wantReply(101, 1, "Congratulations!")
	h.waitState(1, "delivered")
	want := []setReaction{{chatID: 101, messageID: requestMessageID(1), emoji: "🎉"}, {chatID: 101, messageID: requestMessageID(1), emoji: "❤"}}
	if got := h.setReactions(); !slices.Equal(got, want) {
		t.Fatalf("reactions = %+v, want %+v", got, want)
	}
}

// A react-only turn that returned to an earlier conversation sends nothing, not even the line that
// marks the return.
func TestReactOnlyTurnInAResumedConversationSendsNothing(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	first := receive(t, h.ctx, runtime.calls)
	first.answer <- agentruntime.RunResult{Response: "The ferns."}
	earlierAnswer := h.nextMessage()
	h.waitState(1, "delivered")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	h.push(textUpdate(3, 101, "book a table for Friday"))
	later := receive(t, h.ctx, runtime.calls)
	later.answer <- agentruntime.RunResult{Response: "Booked."}
	h.wantReply(101, 3, "Booked.")
	h.waitState(3, "delivered")

	thanks := textUpdate(4, 101, "понял")
	thanks.Message.ReplyToMessage = &Message{MessageID: earlierAnswer.messageID}
	h.push(thanks)
	resumed := receive(t, h.ctx, runtime.calls)
	if resumed.request.SessionID != first.request.SessionID {
		t.Fatalf("reply continued in %s, want the earlier %s", resumed.request.SessionID, first.request.SessionID)
	}
	react(t, resumed, "👌")
	resumed.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(4, "delivered")
	if pending := len(h.messages); pending != 0 {
		t.Fatalf("a react-only resumed turn sent %d messages: %+v", pending, h.nextMessage())
	}
	if got := h.setReactions(); !slices.Equal(got, []setReaction{{chatID: 101, messageID: requestMessageID(4), emoji: "👌"}}) {
		t.Fatalf("reactions = %+v", got)
	}
}

// The turn after Confirm and a scheduled run answer no message the user wrote, so neither can react.
func TestContinuationAndScheduledRunsAreNotOfferedReact(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, reactions: true, providers: []tools.Provider{outsideProvider{}}})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "at 9 check whether my train is delayed"))
	chat := receive(t, h.ctx, runtime.calls)
	readOutside(t, chat)
	result, err := chat.request.Tools.Executor.Execute(chat.ctx, agentruntime.ToolCall{
		Name:      "schedule_propose",
		Arguments: json.RawMessage(`{"name":"Train check","at":"2026-09-28 09:00","prompt":"Is my 9:30 train delayed?"}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("proposal = %+v %v", result, err)
	}
	chat.answer <- agentruntime.RunResult{Response: "proposed"}
	h.wantReply(101, 1, "proposed")
	h.waitState(1, "delivered")
	proposal := h.nextMessage()
	confirm, _ := approvalButtons(t, proposal.keyboard)
	h.push(callbackUpdate(2, 101, proposal.messageID, confirm))
	h.wantAnswer(2)
	h.nextEdit()

	continuation := receive(t, h.ctx, runtime.calls)
	if !strings.HasSuffix(continuation.request.Message, conversation.ContinueMessage) {
		t.Fatalf("continuation message = %q", continuation.request.Message)
	}
	if names := toolNames(continuation.request.Tools); slices.Contains(names, "react") || strings.Contains(continuation.request.Tools.Guidance, "react sets") {
		t.Fatalf("continuation tools = %v", names)
	}
	if result, err := continuation.request.Tools.Executor.Execute(continuation.ctx, agentruntime.ToolCall{Name: "react", Arguments: json.RawMessage(`{"emoji":"👍"}`)}); err != nil || !result.IsError {
		t.Fatalf("react in a continuation = %+v %v", result, err)
	}
	continuation.answer <- agentruntime.RunResult{Response: conversation.NothingToAdd}
	h.waitState(2, "delivered")

	clock.Set(t, "2026-09-28T09:00:20+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if names := toolNames(run.request.Tools); slices.Contains(names, "react") {
		t.Fatalf("scheduled run tools = %v", names)
	}
	run.answer <- agentruntime.RunResult{Response: "No delay.", Final: "No delay."}
	h.wantMessage(101, "<b>Scheduled: Train check</b>\n\nNo delay.")
	h.waitRun(h.jobID("alice"), schedule.StatusDelivered)
	if got := h.setReactions(); len(got) != 0 {
		t.Fatalf("reactions = %+v", got)
	}
}

func reactionUpdate(id, userID, messageID int64, emojis ...string) Update {
	reactions := make([]ReactionType, 0, len(emojis))
	for _, emoji := range emojis {
		reactions = append(reactions, ReactionType{Type: "emoji", Emoji: emoji})
	}
	return Update{UpdateID: id, MessageReaction: &MessageReactionUpdated{
		Chat: Chat{ID: userID, Type: ChatTypePrivate}, MessageID: messageID, User: &User{ID: userID, FirstName: "Test"}, NewReaction: reactions,
	}}
}

// feedback returns the reaction kept for one of the principal's replies.
func (h *conversationHarness) feedback(principalID string, messageID int64) (class, emoji string, found bool) {
	h.t.Helper()
	err := h.db.QueryRowContext(h.ctx, `SELECT class, emoji FROM reply_feedback WHERE principal_id = ? AND telegram_message_id = ?`, principalID, messageID).Scan(&class, &emoji)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return class, emoji, true
}

func (h *conversationHarness) count(query string, args ...any) int {
	h.t.Helper()
	var count int
	if err := h.db.QueryRowContext(h.ctx, query, args...).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

// A reaction to a delivered reply keeps its latest class and first emoji, and removing it removes
// the feedback; none of it starts a turn or sends anything.
func TestReactionsToRepliesAreKeptAsFeedback(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "The ferns."}
	reply := h.nextMessage()
	h.waitState(1, "delivered")

	for _, step := range []struct {
		id           int64
		emojis       []string
		class, emoji string
	}{
		{2, []string{"👍"}, "positive", "👍"},
		{3, []string{"❤️"}, "positive", "❤️"},
		{4, []string{"🤔"}, "other", "🤔"},
		{5, []string{"🤔", "🔥"}, "positive", "🤔"},
		{6, nil, "", ""},
	} {
		h.push(reactionUpdate(step.id, 101, reply.messageID, step.emojis...))
		h.waitOffset(step.id + 1)
		class, emoji, found := h.feedback("alice", reply.messageID)
		if class != step.class || emoji != step.emoji || found != (step.class != "") {
			t.Fatalf("after %v feedback = %q %q %v, want %q %q", step.emojis, class, emoji, found, step.class, step.emoji)
		}
	}
	if positive, other := h.feedbackCount("class:positive"), h.feedbackCount("class:other"); positive != 3 || other != 1 {
		t.Fatalf("counted %d positive and %d other reactions", positive, other)
	}
	if requests := h.count(`SELECT count(*) FROM conversation_requests`); requests != 1 || len(h.messages) != 0 {
		t.Fatalf("reactions made %d requests and sent %d messages", requests-1, len(h.messages))
	}
}

// The first 👎 on any message of a long reply starts one reconsider turn in its conversation that
// quotes the reacted message; neither another 👎, a redelivered update, nor a later 👎 starts a
// second, and the reconsider's message is never taken as the user's.
func TestFirstThumbsDownStartsOneReconsiderTurn(t *testing.T) {
	runtime := newControlledRuntime()
	registry := turnstate.New(time.Now)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true, recent: registry})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "explain the lease"))
	first := receive(t, h.ctx, runtime.calls)
	first.answer <- agentruntime.RunResult{Response: strings.Repeat("word ", 1000)}
	head, tail := h.nextMessage(), h.nextMessage()
	h.waitState(1, "delivered")

	h.push(reactionUpdate(2, 101, tail.messageID, "👎"))
	reconsider := receive(t, h.ctx, runtime.calls)
	if !strings.HasPrefix(reconsider.request.Message, conversation.ReconsiderMessage) || reconsider.request.SessionID != first.request.SessionID {
		t.Fatalf("reconsider turn = %q in %s, want the reconsider message in %s", reconsider.request.Message, reconsider.request.SessionID, first.request.SessionID)
	}
	if names := toolNames(reconsider.request.Tools); slices.Contains(names, "react") {
		t.Fatalf("reconsider tools = %v", names)
	}
	h.push(reactionUpdate(3, 101, head.messageID, "👎"))
	h.waitOffset(4)

	// An update redelivered after its turn was queued but before the claim was kept finds its own
	// request again.
	if _, err := h.db.ExecContext(h.ctx, `DELETE FROM reply_reconsiders`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conversation.React(h.ctx, conversation.ReactionInput{
		BotUserID: 999, TelegramUpdateID: 2, PrincipalID: "alice", ChatID: 101, MessageID: tail.messageID, Emojis: []string{"👎"},
	}); err != nil {
		t.Fatal(err)
	}
	if requests, claims := h.count(`SELECT count(*) FROM conversation_requests`), h.count(`SELECT count(*) FROM reply_reconsiders`); requests != 2 || claims != 1 {
		t.Fatalf("requests = %d, claims = %d, want 2 and 1", requests, claims)
	}

	reconsider.answer <- agentruntime.RunResult{Response: "I misread the notice period; it is two months."}
	if answer := h.nextMessage(); answer.replyTo != tail.messageID || answer.text != "I misread the notice period; it is two months." {
		t.Fatalf("reconsider answer = %+v, want it quoting %d", answer, tail.messageID)
	}
	h.waitState(2, "delivered")
	if messages := registry.RecentMessages("alice", 0); !slices.Equal(messages, []string{"explain the lease"}) {
		t.Fatalf("recent messages = %q", messages)
	}

	h.push(reactionUpdate(4, 101, head.messageID), reactionUpdate(5, 101, head.messageID, "👎"))
	h.waitOffset(6)
	if requests := h.count(`SELECT count(*) FROM conversation_requests`); requests != 2 || len(runtime.calls) != 0 || len(h.messages) != 0 {
		t.Fatalf("a later 👎 made %d requests", requests-2)
	}
	if class, _, _ := h.feedback("alice", head.messageID); class != "negative" {
		t.Fatalf("feedback = %q", class)
	}
}

// A 👎 while a turn is pending is kept as feedback without a reconsider and sends nothing, so a 👎
// after the turn finished still starts one.
func TestThumbsDownWhileATurnIsPendingStartsNoReconsider(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	first := receive(t, h.ctx, runtime.calls)
	first.answer <- agentruntime.RunResult{Response: "The ferns."}
	reply := h.nextMessage()
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 101, "and the cactus?"))
	second := receive(t, h.ctx, runtime.calls)
	h.push(reactionUpdate(3, 101, reply.messageID, "👎"))
	h.waitOffset(4)
	if class, _, _ := h.feedback("alice", reply.messageID); class != "negative" {
		t.Fatalf("feedback = %q", class)
	}
	if claims, requests := h.count(`SELECT count(*) FROM reply_reconsiders`), h.count(`SELECT count(*) FROM conversation_requests WHERE telegram_update_id = 3`); claims != 0 || requests != 0 || len(h.messages) != 0 {
		t.Fatalf("a busy 👎 claimed %d, queued %d, sent %d", claims, requests, len(h.messages))
	}
	second.answer <- agentruntime.RunResult{Response: "Rarely."}
	h.wantReply(101, 2, "Rarely.")
	h.waitState(2, "delivered")

	h.push(reactionUpdate(4, 101, reply.messageID, "👎"))
	reconsider := receive(t, h.ctx, runtime.calls)
	if !strings.HasPrefix(reconsider.request.Message, conversation.ReconsiderMessage) {
		t.Fatalf("reconsider message = %q", reconsider.request.Message)
	}
	reconsider.answer <- agentruntime.RunResult{Response: "The orchid too."}
	if answer := h.nextMessage(); answer.replyTo != reply.messageID || answer.text != "The orchid too." {
		t.Fatalf("reconsider answer = %+v", answer)
	}
	h.waitState(4, "delivered")
	if busy, started := h.feedbackCount("reconsider:busy"), h.feedbackCount("reconsider:started"); busy != 1 || started != 1 {
		t.Fatalf("counted %d busy and %d started reconsiders", busy, started)
	}
	if negative := h.feedbackCount("class:negative"); negative != 2 {
		t.Fatalf("counted %d negative reactions", negative)
	}
}

// Only the principal's own reaction in their private chat to one of their replies counts.
func TestReactionsFromElsewhereOrOnOtherMessagesDoNothing(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	first := receive(t, h.ctx, runtime.calls)
	first.answer <- agentruntime.RunResult{Response: "The ferns."}
	reply := h.nextMessage()
	h.waitState(1, "delivered")

	foreign := reactionUpdate(2, 202, reply.messageID, "👎")
	group := reactionUpdate(3, 101, reply.messageID, "👎")
	group.MessageReaction.Chat = Chat{ID: -100, Type: "group"}
	anonymous := reactionUpdate(4, 101, reply.messageID, "👎")
	anonymous.MessageReaction.User, anonymous.MessageReaction.ActorChat = nil, &Chat{ID: -200, Type: "channel"}
	own := reactionUpdate(5, 101, requestMessageID(1), "👎")
	stranger := reactionUpdate(6, 303, reply.messageID, "👎")
	h.push(foreign, group, anonymous, own, stranger)
	h.waitOffset(7)
	if feedback, claims, requests := h.count(`SELECT count(*) FROM reply_feedback`), h.count(`SELECT count(*) FROM reply_reconsiders`), h.count(`SELECT count(*) FROM conversation_requests`); feedback != 0 || claims != 0 || requests != 1 {
		t.Fatalf("feedback = %d, claims = %d, requests = %d", feedback, claims, requests)
	}
	if len(runtime.calls) != 0 || len(h.messages) != 0 {
		t.Fatalf("ignored reactions started %d turns and sent %d messages", len(runtime.calls), len(h.messages))
	}
}

// A 👎 on a reply from an earlier conversation returns to it, as replying to that reply does.
func TestThumbsDownOnAnEarlierConversationReturnsToIt(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	first := receive(t, h.ctx, runtime.calls)
	first.answer <- agentruntime.RunResult{Response: "The ferns."}
	earlier := h.nextMessage()
	h.waitState(1, "delivered")
	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	h.push(textUpdate(3, 101, "book a table for Friday"))
	later := receive(t, h.ctx, runtime.calls)
	later.answer <- agentruntime.RunResult{Response: "Booked."}
	h.wantReply(101, 3, "Booked.")
	h.waitState(3, "delivered")

	h.push(reactionUpdate(4, 101, earlier.messageID, "👎"))
	reconsider := receive(t, h.ctx, runtime.calls)
	if reconsider.request.SessionID != first.request.SessionID || reconsider.request.SessionID == later.request.SessionID {
		t.Fatalf("reconsider ran in %s, want the earlier %s", reconsider.request.SessionID, first.request.SessionID)
	}
	reconsider.answer <- agentruntime.RunResult{Response: "The orchid too."}
	answer := h.nextMessage()
	if answer.replyTo != earlier.messageID || !strings.HasPrefix(answer.text, "Back in the conversation from ") || !strings.HasSuffix(answer.text, "The orchid too.") {
		t.Fatalf("reconsider answer = %+v", answer)
	}
	h.waitState(4, "delivered")

	h.push(textUpdate(5, 101, "thanks"))
	next := receive(t, h.ctx, runtime.calls)
	if next.request.SessionID != first.request.SessionID {
		t.Fatalf("next turn ran in %s, want %s", next.request.SessionID, first.request.SessionID)
	}
	next.answer <- agentruntime.RunResult{Response: "You're welcome."}
	h.wantReply(101, 5, "You're welcome.")
}

// A reply of only emoji to the user's message becomes a reaction and sends no text: the first emoji
// is set when the turn has not reacted, and the reply is dropped when it has. An emoji outside the
// curated set and a reply with words are sent as written.
func TestEmojiOnlyRepliesBecomeReactions(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "thanks"))
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Response: " 👍 "}
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 101, "the plants are watered"))
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Response: "❤️❤️ 🎉"}
	h.waitState(2, "delivered")

	h.push(textUpdate(3, 101, "got it"))
	reacted := receive(t, h.ctx, runtime.calls)
	react(t, reacted, "👌")
	reacted.answer <- agentruntime.RunResult{Response: "👌🏻"}
	h.waitState(3, "delivered")

	if sent := h.sentTexts(); len(sent) != 0 {
		t.Fatalf("emoji-only replies sent %q", sent)
	}
	want := []setReaction{
		{chatID: 101, messageID: requestMessageID(1), emoji: "👍"},
		{chatID: 101, messageID: requestMessageID(2), emoji: "❤"},
		{chatID: 101, messageID: requestMessageID(3), emoji: "👌"},
	}
	if got := h.setReactions(); !slices.Equal(got, want) {
		t.Fatalf("reactions = %+v, want %+v", got, want)
	}
	if reacted, dropped := h.feedbackCount("converted:reacted"), h.feedbackCount("converted:dropped"); reacted != 2 || dropped != 1 {
		t.Fatalf("counted %d reacted and %d dropped conversions, want 2 and 1", reacted, dropped)
	}

	h.push(textUpdate(4, 101, "a unicorn please"))
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Response: "🦄"}
	h.wantReply(101, 4, "🦄")
	h.waitState(4, "delivered")

	h.push(textUpdate(5, 101, "I fixed the tap"))
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Response: "Nice 👍"}
	h.wantReply(101, 5, "Nice 👍")
	h.waitState(5, "delivered")
	if got := h.setReactions(); len(got) != len(want) {
		t.Fatalf("reactions = %+v, want only the first %d", got, len(want))
	}
}

// A turn that answers one of our own messages has no user message to react to, so its emoji-only
// reply is sent as text.
func TestEmojiOnlyReplyToOurOwnMessageIsSent(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{reactions: true})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "which plants need water on Sundays"))
	receive(t, h.ctx, runtime.calls).answer <- agentruntime.RunResult{Response: "The ferns."}
	reply := h.nextMessage()
	h.waitState(1, "delivered")

	h.push(reactionUpdate(2, 101, reply.messageID, "👎"))
	reconsider := receive(t, h.ctx, runtime.calls)
	reconsider.answer <- agentruntime.RunResult{Response: "🤔"}
	if answer := h.nextMessage(); answer.replyTo != reply.messageID || answer.text != "🤔" {
		t.Fatalf("reconsider answer = %+v", answer)
	}
	h.waitState(2, "delivered")
	if got := h.setReactions(); len(got) != 0 {
		t.Fatalf("reactions = %+v", got)
	}
}
