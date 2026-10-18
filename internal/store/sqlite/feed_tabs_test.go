package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

const (
	reportsTab = 40
	binsJob    = "b1b1b1b1b1b1b1b1"
	plantsJob  = "c2c2c2c2c2c2c2c2"
	trainWatch = "d3d3d3d3d3d3d3d3"
)

// feedEnv drives the store the way a feed tab's deliveries and the user's messages in it would.
type feedEnv struct {
	t      *testing.T
	ctx    context.Context
	store  *Store
	update int64
}

func newFeedEnv(t *testing.T) *feedEnv {
	t.Helper()
	store, ctx := openTestStore(t)
	env := &feedEnv{t: t, ctx: ctx, store: store}
	for _, principal := range []string{"alice", "bob"} {
		if err := store.AddSubjectTab(ctx, principal, reportsTab, "Reports", thread.FeedSubject(thread.FeedReports)); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

// later makes what comes next happen in a later millisecond, which is what the store orders by.
func (e *feedEnv) later() { time.Sleep(3 * time.Millisecond) }

func (e *feedEnv) report(principalID, jobID, name string, messageID, threadID int64) {
	e.t.Helper()
	e.update++
	report := schedule.Report{
		ID: fmt.Sprintf("%016x", e.update), PrincipalID: principalID, JobID: jobID, JobName: name, JobType: schedule.TypeModelTurn,
		Text: "report of " + name, DeliveredAt: time.Now(), Messages: []schedule.Sent{{MessageID: messageID, ThreadID: threadID}},
	}
	if err := e.store.SaveJobReport(e.ctx, report, 200); err != nil {
		e.t.Fatal(err)
	}
	e.later()
}

func (e *feedEnv) alert(principalID, watchID, label string, messageID, threadID int64) {
	e.t.Helper()
	if err := e.store.SaveWatchAlertMessage(e.ctx, watch.AlertMessage{
		PrincipalID: principalID, MessageID: messageID, ThreadID: threadID, WatchID: watchID, WatchLabel: label, Text: "alert of " + label,
	}, 200); err != nil {
		e.t.Fatal(err)
	}
	e.later()
}

// send enqueues a message and runs its turn to delivery, so the next message is not busy.
func (e *feedEnv) send(principalID string, threadID, replyTo int64) conversation.EnqueueResult {
	e.t.Helper()
	result := e.enqueue(principalID, threadID, replyTo)
	if result.Created {
		e.answer(result.Request.ID)
	}
	return result
}

// enqueue queues a message whose turn is still to be answered.
func (e *feedEnv) enqueue(principalID string, threadID, replyTo int64) conversation.EnqueueResult {
	e.t.Helper()
	e.update++
	result, err := e.store.EnqueueConversation(e.ctx, conversation.EnqueueInput{
		BotUserID: 999, TelegramUpdateID: e.update, TelegramMessageID: 5000 + e.update, PrincipalID: principalID,
		ThreadID: threadID, ChatID: 101, Message: "message", ReplyToMessageID: replyTo,
	}, conversation.Session{
		ID: fmt.Sprintf("%08d-0000-4000-8000-000000000000", e.update), PrincipalID: principalID,
		RuntimeSessionID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", e.update),
	}, fmt.Sprintf("%08d-2222-4222-8222-222222222222", e.update), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.later()
	return result
}

// answer runs the request's turn and delivers its answer.
func (e *feedEnv) answer(requestID string) {
	e.t.Helper()
	for _, step := range []func() (bool, error){
		func() (bool, error) { return e.store.ClaimConversationRequest(e.ctx, requestID) },
		func() (bool, error) { return e.store.SaveConversationResponse(e.ctx, requestID, "answer", "") },
		func() (bool, error) { return e.store.ClaimConversationDelivery(e.ctx, requestID) },
	} {
		if done, err := step(); err != nil || !done {
			e.t.Fatalf("finish request: %v %v", done, err)
		}
	}
	if err := e.store.MarkConversationDelivered(e.ctx, requestID); err != nil {
		e.t.Fatal(err)
	}
	e.later()
}

func (e *feedEnv) subject(sessionID string) string {
	e.t.Helper()
	var subject string
	if err := e.store.db.QueryRowContext(e.ctx, `SELECT COALESCE(subject, '') FROM conversation_sessions WHERE id = ?`, sessionID).Scan(&subject); err != nil {
		e.t.Fatal(err)
	}
	return subject
}

// wantSession checks which conversation a message joined, what it is about, and the line naming it.
func (e *feedEnv) wantSession(result conversation.EnqueueResult, subject, label string) string {
	e.t.Helper()
	if !result.Created || result.Elsewhere {
		e.t.Fatalf("message was not queued: %+v", result)
	}
	if got := e.subject(result.Request.SessionID); got != subject || result.Request.SubjectLabel != label {
		e.t.Fatalf("message joined a conversation about %q labelled %q, want %q labelled %q", got, result.Request.SubjectLabel, subject, label)
	}
	return result.Request.SessionID
}

func TestFeedTabRepliesKeepEachJobInItsOwnConversation(t *testing.T) {
	env := newFeedEnv(t)
	env.report("alice", binsJob, "Bins", 101, reportsTab)
	bins := env.wantSession(env.send("alice", reportsTab, 101), conversation.JobSubject(binsJob), "Bins")

	env.report("alice", plantsJob, "Plants", 201, reportsTab)
	plantsResult := env.send("alice", reportsTab, 201)
	plants := env.wantSession(plantsResult, conversation.JobSubject(plantsJob), "Plants")
	if plants == bins || plantsResult.SwitchedFrom != bins || !plantsResult.Request.ResumedFrom.IsZero() {
		t.Fatalf("a reply to another job's report = %+v, want a new conversation leaving %s", plantsResult, bins)
	}

	// A later report of the first job returns to its conversation.
	env.report("alice", binsJob, "Bins", 102, reportsTab)
	back := env.send("alice", reportsTab, 102)
	if env.wantSession(back, conversation.JobSubject(binsJob), "Bins") != bins || back.SwitchedFrom != plants || back.Request.ResumedFrom.IsZero() {
		t.Fatalf("a reply to the first job's later report = %+v, want %s resumed", back, bins)
	}

	// A reply to one of the bot's answers returns to that answer's conversation.
	if err := env.store.RecordConversationReply(env.ctx, "alice", plants, plantsResult.Request.ID, 9001); err != nil {
		t.Fatal(err)
	}
	if env.wantSession(env.send("alice", reportsTab, 9001), conversation.JobSubject(plantsJob), "Plants") != plants {
		t.Fatal("a reply to an answer did not return to its conversation")
	}
	if env.wantSession(env.send("alice", reportsTab, 0), conversation.JobSubject(plantsJob), "") != plants {
		t.Fatal("a message with no newer report left the current conversation")
	}

	// An alert's watch gets its own conversation too.
	env.alert("alice", trainWatch, "Morning train", 301, reportsTab)
	env.wantSession(env.send("alice", reportsTab, 301), conversation.WatchSubject(trainWatch), "Morning train")

	// Another principal's message IDs resolve nothing.
	if subject := env.subject(env.send("bob", reportsTab, 101).Request.SessionID); subject != "" {
		t.Fatalf("bob's reply to alice's report joined a conversation about %q", subject)
	}
	if count := countRows(t, env.store, `SELECT count(*) FROM conversation_sessions WHERE principal_id = 'alice' AND status = 'active'`); count != 1 {
		t.Fatalf("alice has %d active conversations in one tab", count)
	}
}

func TestFeedTabMessagesWithoutAReplyFollowTheLatestReport(t *testing.T) {
	env := newFeedEnv(t)
	env.report("alice", binsJob, "Bins", 101, reportsTab)
	bins := env.wantSession(env.send("alice", reportsTab, 0), conversation.JobSubject(binsJob), "Bins")
	if env.wantSession(env.send("alice", reportsTab, 0), conversation.JobSubject(binsJob), "") != bins {
		t.Fatal("a follow-up left the report's conversation")
	}

	env.report("alice", plantsJob, "Plants", 201, reportsTab)
	plants := env.send("alice", reportsTab, 0)
	env.wantSession(plants, conversation.JobSubject(plantsJob), "Plants")
	if plants.SwitchedFrom != bins {
		t.Fatalf("a new subject's conversation left %q, want %s", plants.SwitchedFrom, bins)
	}

	// After /new, the next message stays in the new conversation.
	env.report("alice", binsJob, "Bins", 102, reportsTab)
	if _, reset, err := env.store.ResetConversation(env.ctx, conversation.Lane{PrincipalID: "alice", ThreadID: reportsTab}); err != nil || !reset {
		t.Fatalf("reset = %v %v", reset, err)
	}
	env.later()
	fresh := env.wantSession(env.send("alice", reportsTab, 0), "", "")

	// A reply to the tab's root, or to a message nothing knows, is no reply.
	env.report("alice", plantsJob, "Plants", 202, reportsTab)
	env.wantSession(env.send("alice", reportsTab, reportsTab), conversation.JobSubject(plantsJob), "Plants")
	env.report("alice", binsJob, "Bins", 103, reportsTab)
	env.wantSession(env.send("alice", reportsTab, 7777), conversation.JobSubject(binsJob), "Bins")
	if fresh == bins {
		t.Fatal("/new did not start a fresh conversation")
	}
}

// A report that lands while a turn is still running is older than that turn's answer, so the
// follow-up to the answer stays in its conversation.
func TestFeedTabFollowUpToAnAnswerStaysInItsConversation(t *testing.T) {
	env := newFeedEnv(t)
	env.report("alice", binsJob, "Bins", 101, reportsTab)
	bins := env.wantSession(env.send("alice", reportsTab, 0), conversation.JobSubject(binsJob), "Bins")
	asking := env.enqueue("alice", reportsTab, 0)
	env.report("alice", plantsJob, "Plants", 201, reportsTab)
	env.answer(asking.Request.ID)
	if env.wantSession(env.send("alice", reportsTab, 0), conversation.JobSubject(binsJob), "") != bins {
		t.Fatal("a follow-up to an answer moved to a report that landed before the answer")
	}
}

// /new in a feed tab that holds no conversation still counts as its last exchange.
func TestNewInAnEmptyFeedTabIsItsLastExchange(t *testing.T) {
	env := newFeedEnv(t)
	env.report("alice", binsJob, "Bins", 101, reportsTab)
	if _, reset, err := env.store.ResetConversation(env.ctx, conversation.Lane{PrincipalID: "alice", ThreadID: reportsTab}); err != nil || reset {
		t.Fatalf("reset of an empty tab = %v %v", reset, err)
	}
	env.later()
	env.wantSession(env.send("alice", reportsTab, 0), "", "")
}

func TestRepliesOutsideFeedTabsBehaveAsBefore(t *testing.T) {
	env := newFeedEnv(t)
	main := env.send("alice", 0, 0)
	env.report("alice", binsJob, "Bins", 101, 0)
	if env.wantSession(env.send("alice", 0, 101), "", "") != main.Request.SessionID {
		t.Fatal("a reply to a report in the main view left the main view's conversation")
	}
	env.report("alice", binsJob, "Bins", 102, 50)
	env.wantSession(env.send("alice", 50, 102), "", "")
	env.wantSession(env.send("alice", 50, 0), "", "")
}

func TestFeedTabReplyToAConversationOpenElsewhereIsRefused(t *testing.T) {
	env := newFeedEnv(t)
	env.report("alice", binsJob, "Bins", 101, reportsTab)
	bins := env.wantSession(env.send("alice", reportsTab, 101), conversation.JobSubject(binsJob), "Bins")
	if _, _, err := env.store.ResetConversation(env.ctx, conversation.Lane{PrincipalID: "alice", ThreadID: reportsTab}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AddTab(env.ctx, "alice", 60, "Garden", thread.NamedByUser); err != nil {
		t.Fatal(err)
	}
	if moved, err := env.store.SwitchConversation(env.ctx, conversation.Lane{PrincipalID: "alice", ThreadID: 60}, bins); err != nil || !moved.Switched {
		t.Fatalf("move to another tab = %+v %v", moved, err)
	}
	env.later()

	refused := env.send("alice", reportsTab, 101)
	if refused.Created || !refused.Elsewhere || refused.ElsewhereThread != 60 || refused.ElsewhereTab != "Garden" {
		t.Fatalf("an explicit reply to a conversation open elsewhere = %+v", refused)
	}
	if count := countRows(t, env.store, `SELECT count(*) FROM conversation_requests WHERE principal_id = 'alice' AND thread_id = ?`, reportsTab); count != 1 {
		t.Fatalf("the refused reply queued a turn: %d requests", count)
	}

	// Without a reply, the tab keeps its own conversation instead of getting stuck.
	env.report("alice", binsJob, "Bins", 102, reportsTab)
	env.wantSession(env.send("alice", reportsTab, 0), "", "")
}

// Alert messages are found only by their owner and pruned by count and by age.
func TestWatchAlertMessagesAreKeptPerPrincipal(t *testing.T) {
	env := newFeedEnv(t)
	for messageID := int64(1); messageID <= 3; messageID++ {
		if err := env.store.SaveWatchAlertMessage(env.ctx, watch.AlertMessage{
			PrincipalID: "alice", MessageID: messageID, ThreadID: reportsTab, WatchID: trainWatch, WatchLabel: "Morning train",
			Text: "alert", Private: messageID == 3, External: true,
		}, 2); err != nil {
			t.Fatal(err)
		}
		env.later()
	}
	if _, found, err := env.store.WatchAlertByMessage(env.ctx, "alice", 1); err != nil || found {
		t.Fatalf("the oldest alert beyond the cap = %v %v, want it trimmed", found, err)
	}
	message, found, err := env.store.WatchAlertByMessage(env.ctx, "alice", 3)
	if err != nil || !found || message.ThreadID != reportsTab || message.WatchID != trainWatch || !message.Private || !message.External || message.SentAt.IsZero() {
		t.Fatalf("alert by message = %+v %v %v", message, found, err)
	}
	if _, found, err := env.store.WatchAlertByMessage(env.ctx, "bob", 3); err != nil || found {
		t.Fatalf("bob found alice's alert: %v %v", found, err)
	}
	if err := env.store.PruneJobReports(env.ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if count := countRows(t, env.store, `SELECT count(*) FROM watch_alert_messages`); count != 0 {
		t.Fatalf("%d alert messages outlived their retention", count)
	}
}
