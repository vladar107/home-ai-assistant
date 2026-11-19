package assistantmail_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// triageRun is one triage run as the checker saw it: what it was asked, the tools it was offered
// through the broker, and a way to call them as the runtime does.
type triageRun struct {
	principal string
	message   string
	offered   []string
	guidance  string
	call      func(name, arguments string) string
	// callWithin calls a tool on the context given, such as one that ends sooner than its deadline.
	callWithin func(ctx context.Context, name, arguments string) string
}

// asks reports whether the run was asked about the message.
func (r *triageRun) asks(id string) bool { return strings.Contains(r.message, id) }

var newMessages = regexp.MustCompile(`The new messages are (.+?)\. Read`)

// ids are the new messages the run was asked about.
func (r *triageRun) ids() []string {
	found := newMessages.FindStringSubmatch(r.message)
	if found == nil {
		return nil
	}
	return strings.Split(found[1], ", ")
}

// triageChecker stands in for the turn engine: it collects the principal's tools through a real
// broker under the run's scope, and answers with what the test says.
type triageChecker struct {
	broker *tools.Broker
	mu     sync.Mutex
	answer func(*triageRun) (string, string, error)
	runs   []*triageRun
	// hold, when set, keeps every run waiting until it is closed; started hears each run start.
	hold    chan struct{}
	started chan struct{}
	// Each run reads every new message first, as a model would. A read of failRead fails, as Gmail
	// failing on it would; sim is where it fails.
	sim      *googlesim.Simulator
	failRead string
}

func (c *triageChecker) RunConfined(ctx context.Context, principalID, message string, scope tools.Scope) (string, string, error) {
	set, err := c.broker.ReadOnlyToolsFor(tools.WithScope(ctx, scope), principalID)
	if err != nil {
		return "", "", err
	}
	run := &triageRun{principal: principalID, message: message, guidance: set.Guidance}
	for _, definition := range set.Definitions {
		run.offered = append(run.offered, definition.Name)
	}
	run.callWithin = func(ctx context.Context, name, arguments string) string {
		result, err := set.Executor.Execute(ctx, agentruntime.ToolCall{ID: "call", Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			return "error: " + err.Error()
		}
		return result.Text
	}
	run.call = func(name, arguments string) string {
		// The runtime calls tools back on a context of its own.
		return run.callWithin(context.Background(), name, arguments)
	}
	c.mu.Lock()
	c.runs = append(c.runs, run)
	answer, hold, started, failRead := c.answer, c.hold, c.started, c.failRead
	c.mu.Unlock()
	for _, id := range run.ids() {
		if id == failRead {
			c.sim.FailMessage(id, http.StatusInternalServerError)
		}
		run.call("assistant_mail_read", `{"id":"`+id+`"}`)
		if id == failRead {
			c.sim.FailMessage(id, 0)
		}
	}
	if started != nil {
		started <- struct{}{}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	return answer(run)
}

func (c *triageChecker) answerWith(answer func(*triageRun) (string, string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answer = answer
}

func (c *triageChecker) ran() []*triageRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.runs)
}

// notesTool is another integration's read tool, which a triage run must never be offered.
type notesTool struct{}

func (notesTool) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{Guidance: "Notes guidance.", Definitions: []tools.Definition{{
		Name: "notes_read", Description: "Read the user's notes.", InputSchema: json.RawMessage(`{"type":"object"}`), Risk: tools.RiskRead,
	}}}, nil
}

func (notesTool) Execute(context.Context, string, tools.Call) (tools.Result, error) {
	return tools.Result{Text: "Private notes."}, nil
}

func silent(*triageRun) (string, string, error) { return `{"needs_you":[]}`, "", nil }

// newTriageHarness is a harness whose mail is triaged by the checker, over a broker that also
// offers another integration's tool.
func newTriageHarness(t *testing.T, configure ...func(*assistantmail.Config)) (*harness, *triageChecker) {
	t.Helper()
	checker := &triageChecker{answer: silent}
	h := newHarness(t, append([]func(*assistantmail.Config){func(cfg *assistantmail.Config) { cfg.Checker = checker }}, configure...)...)
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	broker.SetSwitches(h.switches)
	for _, provider := range []tools.Provider{h.service, notesTool{}} {
		if err := broker.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	checker.broker, checker.sim = broker, h.sim
	return h, checker
}

func needs(reasons map[string]string) func(*triageRun) (string, string, error) {
	return func(run *triageRun) (string, string, error) {
		type item struct {
			ID  string `json:"id"`
			Why string `json:"why"`
		}
		listed := []item{}
		for id, why := range reasons {
			if run.asks(id) {
				listed = append(listed, item{ID: id, Why: why})
			}
		}
		encoded, err := json.Marshal(map[string][]item{"needs_you": listed})
		return string(encoded), "", err
	}
}

func TestATriagedMessageThatNeedsNothingStaysSilent(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	h.deliver("assistant+a1@example.com", googlesim.Message{From: "Alice <alice@example.net>", Subject: "test"})
	h.poll()

	h.expectNotices()
	if pending := h.pending("alice"); pending != 0 {
		t.Fatalf("pending after a silent triage = %d", pending)
	}
	if runs, silentRuns := len(checker.ran()), h.metrics.triages("silent"); runs != 1 || silentRuns != 1 {
		t.Fatalf("runs %d, silent %d", runs, silentRuns)
	}
	if told := h.notices.PendingNotice("alice"); told != "" {
		t.Fatalf("turn notice without a notice: %q", told)
	}
	h.poll()
	if runs := len(checker.ran()); runs != 1 {
		t.Fatalf("settled mail was triaged again: %d runs", runs)
	}
	h.expectQuietLogs()
}

func TestANoticeListsOnlyWhatNeedsTheUserFromSeparateThreadRuns(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	if err := h.store.SaveLanguage(h.ctx, "bob", locale.Russian); err != nil {
		t.Fatal(err)
	}
	h.start()
	order := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Delivery slot"})
	digest := h.deliver("assistant+a1@example.com", googlesim.Message{From: "Digest <news@digest.example.org>", Subject: "Weekly digest"})
	venue := h.deliver("assistant+b1@example.com", googlesim.Message{From: "Club <desk@club.example.org>", Subject: "Venue"})
	checker.answerWith(needs(map[string]string{order: "Pick a delivery slot by Friday.", venue: "Нужно выбрать площадку до пятницы."}))
	h.poll()

	h.expectNotices(
		sentNotice{chatID: 101, text: "📬 1 email at your assistant address needs you:\n" +
			"• Shop (orders@shop.example.org) · Delivery slot\n" +
			"  Pick a delivery slot by Friday.\n" +
			"\nAsk me to read any of them."},
		sentNotice{chatID: 102, text: "📬 Тебе стоит посмотреть 1 письмо на твой адрес у ассистента:\n" +
			"• Club (desk@club.example.org) · Venue\n" +
			"  Нужно выбрать площадку до пятницы.\n" +
			"\nПопроси меня прочитать любое из них."},
	)
	runs := checker.ran()
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want one per thread", len(runs))
	}
	for _, run := range runs {
		asked := 0
		for _, id := range []string{order, digest, venue} {
			if run.asks(id) {
				asked++
			}
		}
		if asked != 1 {
			t.Fatalf("a run was asked about %d threads' mail:\n%s", asked, run.message)
		}
	}
	if notified, silentRuns := h.metrics.triages("notified"), h.metrics.triages("silent"); notified != 2 || silentRuns != 1 {
		t.Fatalf("notified %d, silent %d", notified, silentRuns)
	}
	if told := h.notices.PendingNotice("alice"); !strings.Contains(told, "id "+order+`, from "Shop (orders@shop.example.org)"`) ||
		!strings.Contains(told, `needs the user: "Pick a delivery slot by Friday."`) || strings.Contains(told, digest) {
		t.Fatalf("turn notice = %q", told)
	}
	if h.pending("alice") != 0 || h.pending("bob") != 0 {
		t.Fatalf("pending alice %d, bob %d", h.pending("alice"), h.pending("bob"))
	}
	h.expectQuietLogs()
}

func TestTriageDropsIdsOutsideTheThreadsNewMail(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	earlier := h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-a", Subject: "Order"})
	h.poll()
	fresh := h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-a", Subject: "Re: Order"})
	other := h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-b", Subject: "Other"})
	checker.answerWith(func(run *triageRun) (string, string, error) {
		if run.asks(fresh) {
			return `{"needs_you":[{"id":"` + earlier + `","why":"Earlier."},{"id":"` + other + `","why":"Other thread."},{"id":"msg-999","why":"Made up."}]}`, "", nil
		}
		return `{"needs_you":[]}`, "", nil
	})
	h.poll()

	h.expectNotices()
	if pending := h.pending("alice"); pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
	if silentRuns := h.metrics.triages("silent"); silentRuns != 3 {
		t.Fatalf("silent runs = %d", silentRuns)
	}
}

func TestAnUnverifiedSenderIsListedWhateverTriageSays(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	h, checker := newTriageHarness(t, func(cfg *assistantmail.Config) {
		cfg.Mailbox.Tags = map[string][]string{"alice": {"test1"}, "bob": {"b1"}}
		cfg.Now = func() time.Time { return clock }
	})
	h.start()
	failing := h.sim.DeliverRaw(googlesim.SampleDeliveryFailingDMARC(), "INBOX")
	h.poll()

	h.expectNotices(sentNotice{chatID: 101, text: "📬 1 email at your assistant address needs you:\n" +
		"• Sam Sender (sender@example.org) · test email\n" +
		"  ⚠️ sender not verified\n" +
		"  Its sender could not be verified, so it may be forged.\n" +
		"\nAsk me to read any of them."})
	if runs := checker.ran(); len(runs) != 1 || !runs[0].asks(failing) {
		t.Fatalf("runs = %d", len(runs))
	}
	if notified := h.metrics.triages("notified"); notified != 1 {
		t.Fatalf("notified runs = %d", notified)
	}
}

func TestAFailedOrUnreadableTriageListsEveryMessageAsItArrived(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(*triageRun) (string, string, error){
		"fallback": func(*triageRun) (string, string, error) { return "Nothing here needs you.", "", nil },
		"failed":   func(*triageRun) (string, string, error) { return "The model timed out.", "timeout", nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, checker := newTriageHarness(t)
			checker.answerWith(answer)
			h.start()
			received := time.Now().Add(time.Minute)
			h.deliver("assistant+a1@example.com", googlesim.Message{Received: received})
			h.poll()

			h.expectNotices(sentNotice{chatID: 101, text: "📬 1 new email at your assistant address:\n• Shop (orders@shop.example.org) · Your order · " +
				locale.Date(locale.English, received.In(noticeZone), locale.DayTime) + "\n\nAsk me to read any of them."})
			if runs := h.metrics.triages(name); runs != 1 {
				t.Fatalf("%s runs = %d", name, runs)
			}
			if pending := h.pending("alice"); pending != 0 {
				t.Fatalf("pending = %d", pending)
			}
			h.expectQuietLogs()
		})
	}
}

func TestTriagedMailIsSettledOnlyOnceItsNoticeIsDelivered(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	order := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Delivery slot"})
	checker.answerWith(needs(map[string]string{order: "Pick a delivery slot."}))
	h.chat.failWith(errors.New("telegram is down"))
	h.poll()
	h.poll()
	if pending, failed := h.pending("alice"), h.metrics.notices("failed"); pending != 1 || failed != 2 {
		t.Fatalf("pending %d, failed %d after undelivered notices", pending, failed)
	}

	h.chat.failWith(nil)
	h.poll()
	if sent := h.chat.take(); len(sent) != 1 || !strings.Contains(sent[0].text, "Pick a delivery slot.") {
		t.Fatalf("notices = %+v", sent)
	}
	if pending := h.pending("alice"); pending != 0 {
		t.Fatalf("pending after the notice = %d", pending)
	}
	if runs := len(checker.ran()); runs != 1 {
		t.Fatalf("triage ran %d times for one thread", runs)
	}

	// A notice that never gets through is abandoned after three polls, still without a rerun.
	h.chat.failWith(errors.New("telegram is down"))
	stuck := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Stuck"})
	checker.answerWith(needs(map[string]string{stuck: "Answer the question."}))
	for range 3 {
		h.poll()
	}
	if abandoned, pending, runs := h.metrics.notices("abandoned"), h.pending("alice"), len(checker.ran()); abandoned != 1 || pending != 0 || runs != 2 {
		t.Fatalf("abandoned %d, pending %d, runs %d", abandoned, pending, runs)
	}
}

func TestATriageRunReachesOnlyItsThread(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	older := h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-old", Subject: "Older"})
	foreign := h.deliver("assistant+b1@example.com", googlesim.Message{ThreadID: "thread-bob", Subject: "Bob's mail"})
	h.poll()
	earlierReads := h.fullReads(older) + h.fullReads(foreign)
	fresh := h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-new", Subject: "Fresh"})
	var listed, readOlder, readForeign, readFresh, notes string
	checker.answerWith(func(run *triageRun) (string, string, error) {
		if run.asks(fresh) {
			listed = run.call("assistant_mail_list", `{}`)
			readOlder = run.call("assistant_mail_read", `{"id":"`+older+`"}`)
			readForeign = run.call("assistant_mail_read", `{"id":"`+foreign+`"}`)
			readFresh = run.call("assistant_mail_read", `{"id":"`+fresh+`"}`)
			notes = run.call("notes_read", `{}`)
		}
		return `{"needs_you":[]}`, "", nil
	})
	h.poll()

	var run *triageRun
	for _, candidate := range checker.ran() {
		if candidate.asks(fresh) {
			run = candidate
		}
	}
	if run == nil || run.principal != "alice" {
		t.Fatal("the new thread was not triaged for its principal")
	}
	if !slices.Equal(run.offered, []string{"assistant_mail_list", "assistant_mail_read"}) || strings.Contains(run.guidance, "Notes") ||
		!strings.Contains(run.guidance, "only the thread being checked") {
		t.Fatalf("offered %v with guidance %q", run.offered, run.guidance)
	}
	if !strings.Contains(listed, "1 messages in the thread being checked") || !strings.Contains(listed, "id "+fresh) || strings.Contains(listed, older) {
		t.Fatalf("list = %q", listed)
	}
	if !strings.Contains(readOlder, "No message with that id is in the thread being checked") || !strings.Contains(readForeign, "No message with that id") {
		t.Fatalf("reads outside the thread = %q, %q", readOlder, readForeign)
	}
	if !strings.Contains(readFresh, "Subject: Fresh") {
		t.Fatalf("read of the thread's message = %q", readFresh)
	}
	if strings.Contains(notes, "Private notes") {
		t.Fatalf("another integration's tool answered: %q", notes)
	}
	if reads := h.fullReads(older) + h.fullReads(foreign); reads != earlierReads {
		t.Fatalf("Gmail was asked for mail outside the thread %d times", reads-earlierReads)
	}

	// Outside triage, the same tools still reach all of the principal's mail.
	if result := h.execute("alice", "assistant_mail_read", map[string]string{"id": older}); result.IsError {
		t.Fatalf("read outside triage = %+v", result)
	}
}

func TestASlowTriageNeverHoldsUpPolling(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	checker.hold, checker.started = make(chan struct{}), make(chan struct{}, 4)
	h.start()
	first := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "First"})
	if err := h.service.Poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the triage did not start")
	}
	second := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Second"})
	if err := h.service.Poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectRoutes(second, "alice")
	if runs := checker.ran(); len(runs) != 1 || !runs[0].asks(first) {
		t.Fatalf("a second triage started beside the first: %d runs", len(runs))
	}

	close(checker.hold)
	if err := h.service.WaitTriage(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if runs := checker.ran(); len(runs) != 2 || !runs[1].asks(second) || runs[1].asks(first) {
		t.Fatalf("runs after release = %d", len(runs))
	}
	if pending := h.pending("alice"); pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
}

func TestMailTheRunNeverReadIsListedAsItArrived(t *testing.T) {
	t.Parallel()
	t.Run("failed read", func(t *testing.T) {
		t.Parallel()
		h, checker := newTriageHarness(t)
		h.start()
		h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-x", Subject: "First"})
		received := time.Now().Add(time.Minute)
		second := h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-x", Subject: "Second", Received: received})
		checker.failRead = second
		h.poll()

		h.expectNotices(sentNotice{chatID: 101, text: "📬 1 new email at your assistant address:\n• Shop (orders@shop.example.org) · Second · " +
			locale.Date(locale.English, received.In(noticeZone), locale.DayTime) + "\n\nAsk me to read any of them."})
		if pending := h.pending("alice"); pending != 0 {
			t.Fatalf("pending = %d", pending)
		}
	})
	t.Run("past the call budget", func(t *testing.T) {
		t.Parallel()
		h, _ := newTriageHarness(t)
		h.start()
		base := time.Now().Add(time.Minute)
		for index := range 13 {
			h.deliver("assistant+a1@example.com", googlesim.Message{ThreadID: "thread-y", Subject: fmt.Sprintf("Order %02d", index), Received: base.Add(time.Duration(index) * time.Second)})
		}
		h.poll()

		sent := h.chat.take()
		if len(sent) != 1 || strings.Count(sent[0].text, "• ") != 1 || !strings.Contains(sent[0].text, "• Shop (orders@shop.example.org) · Order 12 · ") {
			t.Fatalf("notices = %+v", sent)
		}
	})
}

func TestTriageRunsAreCappedPerPrincipalAndDay(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	base := time.Now().Add(time.Minute)
	for index := range 31 {
		h.deliver("assistant+a1@example.com", googlesim.Message{Subject: fmt.Sprintf("Order %02d", index), Received: base.Add(time.Duration(index) * time.Second)})
	}
	for range 4 {
		h.poll()
	}

	if runs, capped := len(checker.ran()), h.metrics.triages("capped"); runs != 30 || capped != 1 {
		t.Fatalf("runs %d, capped %d", runs, capped)
	}
	if sent := h.chat.take(); len(sent) != 1 || !strings.HasPrefix(sent[0].text, "📬 1 new email at your assistant address:\n• Shop (orders@shop.example.org) · Order 30 · ") {
		t.Fatalf("notices = %+v", sent)
	}
	if pending := h.pending("alice"); pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
}

func TestAReasonWithALinkGivesWayToTheCodes(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	order := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Account"})
	checker.answerWith(needs(map[string]string{order: "Verify your account at evil.example now."}))
	h.poll()

	h.expectNotices(sentNotice{chatID: 101, text: "📬 1 email at your assistant address needs you:\n" +
		"• Shop (orders@shop.example.org) · Account\n" +
		"  It may need you.\n" +
		"\nAsk me to read any of them."})
	if told := h.notices.PendingNotice("alice"); strings.Contains(told, "evil.example") {
		t.Fatalf("turn notice = %q", told)
	}
}

func TestMailOfASwitchTurnedOffDuringTriageIsSettledUnannounced(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	order := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Delivery slot"})
	answer := needs(map[string]string{order: "Pick a delivery slot."})
	checker.answerWith(func(run *triageRun) (string, string, error) {
		if err := h.switches.SetEnabled(h.ctx, "alice", assistantmail.Switch, false); err != nil {
			return "", "", err
		}
		return answer(run)
	})
	h.poll()

	h.expectNotices()
	if pending, switchedOff := h.pending("alice"), h.metrics.notices("switched_off"); pending != 0 || switchedOff != 1 {
		t.Fatalf("pending %d, switched_off %d", pending, switchedOff)
	}
	h.setSwitch("alice", true)
	h.poll()
	h.expectNotices()
}

func TestAMessageWhoseHeadersKeepFailingIsGivenUp(t *testing.T) {
	t.Parallel()
	h, checker := newTriageHarness(t)
	h.start()
	stuck := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Stuck"})
	checker.answerWith(needs(map[string]string{stuck: "Answer the question."}))
	h.chat.failWith(errors.New("telegram is down"))
	h.poll()

	h.chat.failWith(nil)
	h.sim.FailMessage(stuck, http.StatusInternalServerError)
	healthy := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Healthy"})
	checker.answerWith(needs(map[string]string{healthy: "Confirm the order."}))
	for range 2 {
		h.poll()
		h.expectNotices()
	}
	h.poll()
	if sent := h.chat.take(); len(sent) != 1 || !strings.Contains(sent[0].text, "· Healthy\n") || strings.Contains(sent[0].text, "Stuck") {
		t.Fatalf("notices = %+v", sent)
	}
	if abandoned, pending := h.metrics.notices("abandoned"), h.pending("alice"); abandoned != 1 || pending != 0 {
		t.Fatalf("abandoned %d, pending %d", abandoned, pending)
	}
}
