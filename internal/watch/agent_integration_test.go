package watch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// fakeChecks stands in for the conversation service's check turn: it records each turn's message,
// signals started, and answers with the reply currently set, after the gate opens when one is set.
type fakeChecks struct {
	started    chan struct{}
	mu         sync.Mutex
	reply      turn.ScheduledReply
	messages   []string
	principals []string
	gate       chan struct{}
}

func (f *fakeChecks) answer(reply turn.ScheduledReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

func (f *fakeChecks) hold() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	return f.gate
}

func (f *fakeChecks) RunCheck(ctx context.Context, principalID, message string) (turn.ScheduledReply, error) {
	f.mu.Lock()
	f.messages = append(f.messages, message)
	f.principals = append(f.principals, principalID)
	gate := f.gate
	f.mu.Unlock()
	f.started <- struct{}{}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return turn.ScheduledReply{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reply, nil
}

func (f *fakeChecks) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messages...)
}

func (f *fakeChecks) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.principals...)
}

type agentEngine struct {
	*homeEngine
	checks   *fakeChecks
	finished chan struct{}
}

func newAgentEngine(t *testing.T) *agentEngine {
	t.Helper()
	return agentEngineOn(t, newHomeEngine(t))
}

// newRewritingAgentEngine is an agent engine whose service rewrites alerts with the rewriter.
func newRewritingAgentEngine(t *testing.T, rewriter watch.Rewriter) *agentEngine {
	t.Helper()
	e := newEngine(t)
	e.rewriter = rewriter
	e.service = e.newService()
	return agentEngineOn(t, &homeEngine{engine: e})
}

func agentEngineOn(t *testing.T, h *homeEngine) *agentEngine {
	t.Helper()
	a := &agentEngine{homeEngine: h, checks: &fakeChecks{started: make(chan struct{}, 16)}, finished: make(chan struct{}, 16)}
	evaluator, err := watch.NewAgentEvaluator(a.checks, watch.AgentConfig{Now: a.clock.Now, Finished: func() { a.finished <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.service.Register(watch.KindAgent, evaluator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := evaluator.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a
}

func (a *agentEngine) waitFinished() {
	a.t.Helper()
	select {
	case <-a.finished:
	case <-time.After(5 * time.Second):
		a.t.Fatal("the agent check did not finish")
	}
}

// waitStarted waits for count checks to have started.
func (a *agentEngine) waitStarted(count int) {
	a.t.Helper()
	for range count {
		select {
		case <-a.checks.started:
		case <-time.After(5 * time.Second):
			a.t.Fatal("the agent check did not start")
		}
	}
}

// check runs one whole agent check at the local time: the cycle that starts it and, once it has
// finished, the cycle that collects its answer.
func (a *agentEngine) check(local string, reply turn.ScheduledReply) {
	a.t.Helper()
	a.checks.answer(reply)
	before := len(a.checks.sent())
	a.at(local)
	a.waitStarted(1)
	a.waitFinished()
	if len(a.checks.sent()) != before+1 {
		a.t.Fatalf("%d checks started at %s, want one", len(a.checks.sent())-before, local)
	}
	a.service.Cycle(a.ctx)
}

// wantAlert expects exactly one message, to alice, containing every given text.
func (a *agentEngine) wantAlert(contains ...string) {
	a.t.Helper()
	sent := a.wantSent(1)
	for _, want := range contains {
		if !strings.Contains(sent[0].text, want) {
			a.t.Fatalf("alert = %q, want it to contain %q", sent[0].text, want)
		}
	}
}

func verdict(met bool, value, headline string) turn.ScheduledReply {
	encoded, _ := json.Marshal(map[string]any{"met": met, "value": value, "headline": headline})
	return turn.ScheduledReply{Text: "I searched the airline's fares.\n\n" + string(encoded)}
}

// linked is a met verdict with details and the given links, each a title and an address.
func linked(value, headline, details string, links ...[2]string) turn.ScheduledReply {
	entries := make([]map[string]string, 0, len(links))
	for _, link := range links {
		entries = append(entries, map[string]string{"title": link[0], "url": link[1]})
	}
	encoded, _ := json.Marshal(map[string]any{"met": true, "value": value, "headline": headline, "details": details, "links": entries})
	return turn.ScheduledReply{Text: "I searched the listings.\n\n" + string(encoded)}
}

// agentSpec is a stored agent watch with the given triggers, enter alone when none are given.
func agentSpec(triggers ...string) watch.Spec {
	if len(triggers) == 0 {
		triggers = []string{watch.TriggerEnter}
	}
	return watch.Spec{
		Kind:   watch.KindAgent,
		Target: watch.Target{Name: "Lisbon flight price"},
		Check: &watch.Check{
			Instruction:     "Search for the cheapest return flight from Amsterdam to Lisbon on 20 December.",
			Condition:       "the cheapest price is below 150 euros",
			IntervalMinutes: 60,
		},
		Triggers: triggers,
		Days:     []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		Start:    "00:00",
		End:      "24:00",
		Zone:     "Europe/Amsterdam",
	}
}

// A first answer that does not meet the condition stays silent, the met answer alerts once, the
// same answer again and a new value without the change trigger stay silent, and the next day
// starts from the last answer, so nothing is reported again at midnight. Checks keep the interval
// and each turn is told the value last reported and the local time.
func TestAgentWatchAlertsOnlyWhenTheAnswerChanges(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec(watch.TriggerEnter))

	a.check("2026-09-28 10:00", verdict(false, "€180", "The cheapest return is €180."))
	a.wantSent(0)
	first := a.checks.sent()[0]
	for _, want := range []string{
		`watch "Lisbon flight price"`,
		"Monday 28 September 2026, 10:00 CEST (Europe/Amsterdam)",
		"Check: Search for the cheapest return flight from Amsterdam to Lisbon on 20 December.",
		"Tell the user when: the cheapest price is below 150 euros",
		"Value last reported: none yet",
		`{"met": true or false`,
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("check message lacks %q:\n%s", want, first)
		}
	}

	a.at("2026-09-28 10:30")
	a.at("2026-09-28 10:58")
	if checks := len(a.checks.sent()); checks != 1 {
		t.Fatalf("%d checks before the interval passed, want 1", checks)
	}

	a.check("2026-09-28 11:00", verdict(true, "€140", "The cheapest return is €140."))
	a.wantAlert("Lisbon flight price", "The cheapest return is €140.")
	if !strings.Contains(a.checks.sent()[1], "Value last reported: €180") {
		t.Fatalf("second check message = %s", a.checks.sent()[1])
	}

	a.check("2026-09-28 12:00", verdict(true, "€140", "Still €140."))
	a.check("2026-09-28 13:00", verdict(true, "€135", "Now €135."))
	a.wantSent(0)
	if !strings.Contains(a.checks.sent()[3], "Value last reported: €140") {
		t.Fatalf("a silent change was not remembered: %s", a.checks.sent()[3])
	}

	a.check("2026-09-29 00:00", verdict(true, "€135", "Still €135."))
	a.wantSent(0)
	if !strings.Contains(a.checks.sent()[4], "Value last reported: €135") {
		t.Fatalf("the next day did not start from the last answer: %s", a.checks.sent()[4])
	}
	if a.telemetry.count("check alice agent triggered") != 4 || a.telemetry.count("check alice agent normal") != 1 {
		t.Fatalf("telemetry = %v", a.telemetry.events)
	}
}

// With change, a new value while the condition holds alerts again, and with clear, the end of the
// condition after an alert sends one message; without them neither does.
func TestAgentWatchChangeAndClearAreOptIn(t *testing.T) {
	t.Run("asked", func(t *testing.T) {
		a := newAgentEngine(t)
		a.create("alice", watchID, agentSpec(watch.TriggerEnter, watch.TriggerChange, watch.TriggerClear))
		a.check("2026-09-28 10:00", verdict(false, "€180", "€180."))
		a.check("2026-09-28 11:00", verdict(true, "€140", "Down to €140."))
		a.wantAlert("Down to €140.")
		a.check("2026-09-28 12:00", verdict(true, "€ 135", "Down to €135."))
		a.wantAlert("Down to €135.")
		a.check("2026-09-28 13:00", verdict(true, "€  135", "Still €135."))
		a.wantSent(0)
		a.check("2026-09-28 14:00", verdict(false, "€190", "Up to €190."))
		a.wantAlert("No longer the case. Up to €190.")
		a.check("2026-09-28 15:00", verdict(false, "€200", "Up to €200."))
		a.wantSent(0)
	})
	t.Run("not asked", func(t *testing.T) {
		a := newAgentEngine(t)
		a.create("alice", watchID, agentSpec(watch.TriggerEnter))
		a.check("2026-09-28 10:00", verdict(false, "€180", "€180."))
		a.check("2026-09-28 11:00", verdict(true, "€140", "Down to €140."))
		a.wantAlert("Down to €140.")
		a.check("2026-09-28 12:00", verdict(true, "€135", "Down to €135."))
		a.check("2026-09-28 13:00", verdict(false, "€190", "Up to €190."))
		a.wantSent(0)
		a.check("2026-09-28 14:00", verdict(true, "€130", "Down to €130."))
		a.wantAlert("Down to €130.")
	})
}

// A reply without a valid answer object, a check that reports it could not be done, and a failed
// turn are failed checks: three in a row give one notice, and the next good answer counts again.
func TestAgentWatchCountsUnreadableAnswersAsFailures(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec())

	a.check("2026-09-28 10:00", turn.ScheduledReply{Text: "The price seems to be about 140 euros."})
	a.check("2026-09-28 11:00", turn.ScheduledReply{Text: `Nothing loaded. {"error": "the fare page did not load"}`})
	a.wantSent(0)
	a.check("2026-09-28 12:00", turn.ScheduledReply{Text: "The model is unavailable.", ErrorKind: "runtime"})
	a.wantAlert("Lisbon flight price", "I can't check this right now.")
	a.check("2026-09-28 13:00", turn.ScheduledReply{Text: `{"met": "maybe"}`})
	a.wantSent(0)
	if a.telemetry.count("check alice agent failed") != 4 || a.telemetry.count("upstream agent model") != 1 {
		t.Fatalf("telemetry = %v", a.telemetry.events)
	}

	a.check("2026-09-28 14:00", turn.ScheduledReply{Text: "```json\n{\"met\": true, \"value\": \"€140\", \"headline\": \"Down to €140.\"}\n```"})
	a.wantAlert("Down to €140.")
}

// A check runs in the background: a slow one does not hold back a home watch in the same cycle,
// each principal runs one check at a time, and at most two run at once.
func TestAgentChecksRunInTheBackground(t *testing.T) {
	a := newAgentEngine(t)
	if err := a.store.BootstrapConfiguredPrincipals(a.ctx, []identity.ConfiguredPrincipal{{ID: "carol", DisplayName: "Carol", TelegramUserID: 303}}); err != nil {
		t.Fatal(err)
	}
	a.clock.Set(t, "2026-09-28 10:00")
	a.create("alice", watchID, agentSpec())
	a.create("alice", "0000000000000002", agentSpec())
	a.create("bob", "0000000000000003", agentSpec())
	a.create("carol", "0000000000000004", agentSpec())
	a.create("alice", "0000000000000005", homeSpec("00:00", "24:00"))
	a.home.set(doorOpen, nil)
	gate := a.checks.hold()
	a.checks.answer(verdict(true, "€140", "Down to €140."))

	a.at("2026-09-28 10:00")
	a.wantAlert("Balcony door has been open for 10 minutes.")
	a.waitStarted(2)
	// Each check starts in its own goroutine, so a wave's checks start in either order.
	if principals := a.checks.ran(); len(principals) != 2 || !sameStarts(principals, "alice", "bob") {
		t.Fatalf("checks started for %v, want one of alice's and bob's", principals)
	}
	a.at("2026-09-28 10:01")
	if checks := len(a.checks.sent()); checks != 2 {
		t.Fatalf("%d checks while two were running, want 2", checks)
	}

	close(gate)
	a.waitFinished()
	a.waitFinished()
	a.service.Cycle(a.ctx)
	a.waitStarted(2)
	sent := a.sender.take()
	if len(sent) != 2 || sent[0].chatID != 101 || sent[1].chatID != 202 || !strings.Contains(sent[1].text, "Down to €140.") {
		t.Fatalf("sent = %+v, want alice's and bob's alerts", sent)
	}
	if principals := a.checks.ran(); len(principals) != 4 || !sameStarts(principals[2:], "alice", "carol") {
		t.Fatalf("checks started for %v, want alice's second and carol's next", principals)
	}
}

func sameStarts(principals []string, want ...string) bool {
	got := slices.Clone(principals)
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	return slices.Equal(got, want)
}

// A check that finishes after its watch was edited is thrown away, and the edited watch is checked
// afresh.
func TestAgentCheckOfAnEditedWatchIsDiscarded(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec())
	gate := a.checks.hold()
	a.checks.answer(verdict(true, "€140", "Down to €140."))
	a.at("2026-09-28 10:00")
	a.waitStarted(1)

	current, _, err := a.store.LoadWatch(a.ctx, "alice", watchID)
	if err != nil {
		t.Fatal(err)
	}
	edited := current
	edited.Check = &watch.Check{Instruction: current.Check.Instruction, Condition: "the cheapest price is below 120 euros", IntervalMinutes: 60}
	edited.UpdatedAt = current.UpdatedAt.Add(time.Minute)
	if updated, err := a.store.UpdateWatch(a.ctx, edited, current.UpdatedAt); err != nil || !updated {
		t.Fatalf("update = %v %v", updated, err)
	}
	close(gate)
	a.waitFinished()

	a.at("2026-09-28 10:05")
	a.waitStarted(1)
	a.wantSent(0)
	if checks := a.checks.sent(); len(checks) != 2 || !strings.Contains(checks[1], "below 120 euros") {
		t.Fatalf("checks after the edit = %v", checks)
	}
}

// A watch with an end date runs through that day and is deleted after it.
func TestAgentWatchEndsAfterItsLastDay(t *testing.T) {
	a := newAgentEngine(t)
	spec := agentSpec()
	spec.Check.Until = "2026-09-29"
	a.create("alice", watchID, spec)

	a.check("2026-09-29 23:00", verdict(false, "€180", "€180."))
	a.at("2026-09-30 00:30")
	if checks := len(a.checks.sent()); checks != 1 {
		t.Fatalf("%d checks, want none after the last day", checks)
	}
	if watches, err := a.store.ListWatches(a.ctx, "alice"); err != nil || len(watches) != 0 {
		t.Fatalf("watches after the last day = %+v %v", watches, err)
	}
}

func agentArguments(change func(arguments, check map[string]any)) map[string]any {
	check := map[string]any{
		"instruction": "Look at the release notes page of the example project for the next release date.",
		"condition":   "a release date is announced",
	}
	arguments := map[string]any{"kind": "agent", "name": "Release date", "check": check}
	if change != nil {
		change(arguments, check)
	}
	return arguments
}

// The model gives a name, an instruction, and a condition; the defaults fill the rest, invalid
// checks are refused, and each user keeps at most five agent watches, also when confirming.
func TestAgentWatchProposalValidatesAndLimits(t *testing.T) {
	a := newAgentEngine(t)
	a.clock.Set(t, "2026-09-28 10:00")

	if result := a.propose(agentArguments(nil)); result.IsError {
		t.Fatalf("proposal = %+v", result)
	}
	spec := a.proposed()
	if spec.Check == nil || spec.Check.IntervalMinutes != 60 || strings.Join(spec.Triggers, ",") != "enter" ||
		len(spec.Days) != 7 || spec.Start != "00:00" || spec.End != "24:00" {
		t.Fatalf("proposed spec = %+v %+v", spec, spec.Check)
	}
	summary := a.proposer.proposals[len(a.proposer.proposals)-1].proposal.Summary
	for _, want := range []string{"Watch Release date", "Checks: Look at the release notes page", "Condition: a release date is announced", "checked every 60 minutes, each check a model turn"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary lacks %q: %s", want, summary)
		}
	}

	for name, change := range map[string]func(arguments, check map[string]any){
		"no check":          func(arguments, _ map[string]any) { delete(arguments, "check") },
		"no name":           func(arguments, _ map[string]any) { delete(arguments, "name") },
		"no condition":      func(_, check map[string]any) { delete(check, "condition") },
		"long instruction":  func(_, check map[string]any) { check["instruction"] = strings.Repeat("a", 601) },
		"fast interval":     func(_, check map[string]any) { check["interval_minutes"] = 10 },
		"past last day":     func(_, check map[string]any) { check["until"] = "2026-09-27" },
		"a home condition":  func(arguments, _ map[string]any) { arguments["condition"] = map[string]any{"state": []string{"on"}} },
		"a departure":       func(arguments, _ map[string]any) { arguments["departure"] = "08:00" },
		"a transit trigger": func(arguments, _ map[string]any) { arguments["triggers"] = []string{"delay"} },
	} {
		if result := a.propose(agentArguments(change)); !result.IsError {
			t.Fatalf("%s: proposal = %+v", name, result)
		}
	}
	home := homeSpec("00:00", "24:00")
	home.Check = &watch.Check{Instruction: "x", Condition: "y"}
	raw, _ := json.Marshal(home)
	if _, err := a.service.ExecuteApproved(a.ctx, approval.Record{ID: "00000000000000ff", PrincipalID: "alice", Tool: "watch_propose", Arguments: raw}); err == nil {
		t.Fatal("a home watch with a check was stored")
	}

	for index := 1; index <= watch.MaximumAgentWatchesPerPrincipal; index++ {
		a.create("alice", fmt.Sprintf("00000000000000a%d", index), agentSpec())
	}
	if result := a.propose(agentArguments(nil)); !result.IsError || !strings.Contains(result.Text, "5 agent watches") {
		t.Fatalf("sixth proposal = %+v", result)
	}
	raw, _ = json.Marshal(agentSpec(watch.TriggerEnter))
	var refused *approval.ActionError
	if _, err := a.service.ExecuteApproved(a.ctx, approval.Record{ID: "00000000000000b1", PrincipalID: "alice", Tool: "watch_propose", Arguments: raw}); !errors.As(err, &refused) || !strings.Contains(refused.Text, "5 agent watches") {
		t.Fatalf("sixth confirmed watch = %v", err)
	}
	if outcome, err := a.service.ExecuteApproved(a.ctx, approval.Record{ID: "00000000000000b2", PrincipalID: "bob", Tool: "watch_propose", Arguments: raw}); err != nil || !strings.HasPrefix(outcome.Text, "Watching. Checking all day, every day, checked every") || strings.Contains(outcome.Text, "midnight") {
		t.Fatalf("bob's first agent watch = %+v %v", outcome, err)
	}
}

// Agent alerts reach the next turn through their own notice source, which says when the check
// read private content, while home and transit alerts stay ordinary notices.
func TestAgentAlertsAreKeptApartForTheNextTurn(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec())
	a.create("alice", "0000000000000002", homeSpec("00:00", "24:00"))
	a.home.set(doorOpen, nil)
	reply := verdict(true, "€140", "Down to €140.")
	reply.Private = true
	a.check("2026-09-28 10:00", reply)
	a.wantSent(2)

	agent := a.service.AgentNotices()
	if text := a.service.PendingNotice("alice"); !strings.Contains(text, "Balcony door") || strings.Contains(text, "Down to €140.") {
		t.Fatalf("ordinary notice = %q", text)
	}
	text, private := agent.TakeNotice("alice")
	if !private || !strings.Contains(text, "Down to €140.") || strings.Contains(text, "Balcony door") {
		t.Fatalf("agent notice = %q private=%v", text, private)
	}
	if text, private := agent.TakeNotice("alice"); text != "" || private {
		t.Fatalf("agent notice taken twice: %q %v", text, private)
	}
	if text := agent.PendingNotice("bob"); text != "" {
		t.Fatalf("bob's agent notice = %q", text)
	}
}

// An alert carries a line for the page of each thing the check found, between the headline and the
// details. Addresses that are not plain https ones and entries past the fifth are dropped without
// failing the check, and other links alone are not a change.
func TestAgentWatchAlertLinksWhatItFound(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec(watch.TriggerEnter, watch.TriggerChange))

	a.check("2026-09-28 10:00", linked("2 listings", "Two new listings.", "Found in the saved search.",
		[2]string{"Oak desk", "https://example.com/item/1?ref=a_b&size=2*3"},
		[2]string{"Plain page", "http://example.com/item/2"},
		[2]string{"Hidden", "https://user@example.com/item/3"},
		[2]string{"Bracketed", "https://example.com/item/(4)"},
		[2]string{"Spaced", "https://example.com/item 5"},
		[2]string{"No host", "https:///item/6"},
		[2]string{"", "https://shop.example.com/item/7"},
		[2]string{"Third", "https://example.com/item/8"},
		[2]string{"Fourth", "https://example.com/item/9"},
		[2]string{"Fifth", "https://example.com/item/10"},
		[2]string{"Sixth", "https://example.com/item/11"},
	))
	sent := a.wantSent(1)
	want := "**⚠️ Lisbon flight price**\n\nTwo new listings.\n" +
		"🔗 Oak desk: https://example.com/item/1?ref=a_b&size=2*3\n" +
		"🔗 shop.example.com: https://shop.example.com/item/7\n" +
		"🔗 Third: https://example.com/item/8\n" +
		"🔗 Fourth: https://example.com/item/9\n" +
		"🔗 Fifth: https://example.com/item/10\n" +
		"Found in the saved search."
	if sent[0].text != want {
		t.Fatalf("alert = %q, want %q", sent[0].text, want)
	}
	if !strings.Contains(a.checks.sent()[0], `"links": [{"title"`) {
		t.Fatalf("check message does not ask for links:\n%s", a.checks.sent()[0])
	}

	a.check("2026-09-28 11:00", linked("2 listings", "Two new listings.", "", [2]string{"Other", "https://example.com/item/12"}))
	a.wantSent(0)

	reply := turn.ScheduledReply{Text: `{"met": true, "value": "3 listings", "headline": "Three now.", "links": "https://example.com/item/1"}`}
	a.check("2026-09-28 12:00", reply)
	if sent := a.wantSent(1); sent[0].text != "**⚠️ Lisbon flight price**\n\nThree now." {
		t.Fatalf("alert with a malformed links field = %q", sent[0].text)
	}
	reply = turn.ScheduledReply{Text: `{"met": true, "value": "4 listings", "headline": "Four now.", "links": ["https://example.com/item/1", {"title": 5, "url": "https://example.com/item/2"}, {"title": "Kept", "url": "https://example.com/item/3"}]}`}
	a.check("2026-09-28 13:00", reply)
	if sent := a.wantSent(1); sent[0].text != "**⚠️ Lisbon flight price**\n\nFour now.\n🔗 example.com: https://example.com/item/2\n🔗 Kept: https://example.com/item/3" {
		t.Fatalf("alert with malformed link entries = %q", sent[0].text)
	}
}

// A title cannot form markup, a hidden or second link, or a second line, nor reverse the address
// after it, and a headline or details line cannot pass for a link line.
func TestAgentWatchLinkTitlesArePlainText(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec())

	a.check("2026-09-28 10:00", linked("1 listing", "\u200b🔗 Fake: https://example.com/headline", "First.\n 🔗 Fake: https://example.com/details",
		[2]string{"**Bold** `code` [not a link](x)\nsecond\tline\x07\u202e", "https://example.com/item/1"},
		[2]string{"*[]*`*", "https://example.com/item/2"},
		[2]string{"[here](https://example.org/x)", "https://example.com/item/3"},
		[2]string{"See example.org", "https://example.com/item/4"},
		[2]string{"Reversed", "https://example.com/\u202eitem/5"},
		[2]string{"Sentence", "https://example.com/item/6."},
	))
	sent := a.wantSent(1)
	want := "**⚠️ Lisbon flight price**\n\nFake: https://example.com/headline\n" +
		"🔗 Bold code not a linkx second line: https://example.com/item/1\n" +
		"🔗 example.com: https://example.com/item/2\n" +
		"🔗 example.com: https://example.com/item/3\n" +
		"🔗 example.com: https://example.com/item/4\n" +
		"First.\n Fake: https://example.com/details"
	if sent[0].text != want {
		t.Fatalf("alert = %q, want %q", sent[0].text, want)
	}
}

// Five links with the longest titles and addresses kept, under the longest headline and details,
// still fit one message.
func TestAgentWatchAlertWithLongLinksFitsOneMessage(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec())

	links := make([][2]string, 0, 6)
	for index := range 6 {
		address := fmt.Sprintf("https://example.com/item/%d/", index)
		links = append(links, [2]string{strings.Repeat("Ж", 80), address + strings.Repeat("a", 200-len(address))})
	}
	a.check("2026-09-28 10:00", linked("5 listings", strings.Repeat("Ж", 400), strings.Repeat("Ж", 1200), links...))
	sent := a.wantSent(1)
	if length := len([]rune(sent[0].text)); length > 4096 {
		t.Fatalf("alert is %d characters, more than one message holds", length)
	}
	if count := strings.Count(sent[0].text, "\n🔗 "); count != 5 {
		t.Fatalf("alert carries %d links, want 5: %q", count, sent[0].text)
	}
	for index := range 5 {
		address := fmt.Sprintf("https://example.com/item/%d/", index)
		line := "\n🔗 " + strings.Repeat("Ж", 59) + "…: " + address + strings.Repeat("a", 200-len(address)) + "\n"
		if !strings.Contains(sent[0].text, line) {
			t.Fatalf("alert lacks the link line %q: %q", line, sent[0].text)
		}
	}
}

// The rewrite into the watch's language never sees a link line: the addresses come back exactly and
// in place, their digits are not held against the rewrite, and a rewrite that did not keep the
// original's lines carries them at its end.
func TestAgentWatchRewriteLeavesLinksAlone(t *testing.T) {
	rewriter := &fakeRewriter{}
	a := newRewritingAgentEngine(t, rewriter)
	spec := agentSpec(watch.TriggerEnter, watch.TriggerChange)
	spec.Language = "Russian"
	a.create("alice", watchID, spec)

	var seen []string
	rewriter.answer(func(text string) (string, error) {
		seen = append(seen, text)
		return "**⚠️ Lisbon flight price**\n\nДва новых объявления.\nНайдено в сохранённом поиске.", nil
	})
	first, second := "🔗 Oak desk: https://example.com/item/1001", "🔗 Lamp 40 cm: https://example.com/item/1002?a=1&b=2"
	links := [][2]string{{"Oak desk", "https://example.com/item/1001"}, {"Lamp 40 cm", "https://example.com/item/1002?a=1&b=2"}}
	a.check("2026-09-28 10:00", linked("2 listings", "Two new listings.", "Found in the saved search.", links...))
	sent := a.wantSent(1)
	if want := "**⚠️ Lisbon flight price**\n\nДва новых объявления.\n" + first + "\n" + second + "\nНайдено в сохранённом поиске."; sent[0].text != want {
		t.Fatalf("rewritten alert = %q, want %q", sent[0].text, want)
	}
	if want := "**⚠️ Lisbon flight price**\n\nTwo new listings.\nFound in the saved search."; len(seen) != 1 || seen[0] != want {
		t.Fatalf("the rewrite saw %q, want %q", seen, want)
	}

	rewriter.answer(func(string) (string, error) {
		return "**⚠️ Lisbon flight price**\n\nТри новых объявления. Найдено в сохранённом поиске.", nil
	})
	a.check("2026-09-28 11:00", linked("3 listings", "Three new listings.", "Found in the saved search.", links...))
	sent = a.wantSent(1)
	if want := "**⚠️ Lisbon flight price**\n\nТри новых объявления. Найдено в сохранённом поиске.\n" + first + "\n" + second; sent[0].text != want {
		t.Fatalf("rewritten alert with other lines = %q, want %q", sent[0].text, want)
	}

	rewriter.answer(func(string) (string, error) {
		return "**⚠️ Lisbon flight price**\n\nТеперь 5 объявлений.\nНайдено в сохранённом поиске.", nil
	})
	a.check("2026-09-28 12:00", linked("4 listings", "Now 4 listings.", "Found in the saved search.", links[:1]...))
	sent = a.wantSent(1)
	if want := "**⚠️ Lisbon flight price**\n\nNow 4 listings.\n" + first + "\nFound in the saved search."; sent[0].text != want {
		t.Fatalf("alert after a rewrite that changed a number = %q, want %q", sent[0].text, want)
	}
	rewriter.answer(func(string) (string, error) { return "", errors.New("model unavailable") })
	a.check("2026-09-28 13:00", linked("5 listings", "Five listings.", "Found in the saved search.", links...))
	sent = a.wantSent(1)
	if want := "**⚠️ Lisbon flight price**\n\nFive listings.\n" + first + "\n" + second + "\nFound in the saved search."; sent[0].text != want {
		t.Fatalf("alert after a failed rewrite = %q, want %q", sent[0].text, want)
	}

	rewriter.answer(func(string) (string, error) {
		return "**⚠️ Lisbon flight price**\n\nШесть объявлений.\n" + strings.Repeat("ж", 4000), nil
	})
	a.check("2026-09-28 14:00", linked("6 listings", "Six listings.", "Found in the saved search.", links...))
	sent = a.wantSent(1)
	if want := "**⚠️ Lisbon flight price**\n\nSix listings.\n" + first + "\n" + second + "\nFound in the saved search."; sent[0].text != want {
		t.Fatalf("alert after a rewrite too long with its links = %q, want %q", sent[0].text, want)
	}
	if a.telemetry.count("rewrite alice rewritten") != 2 || a.telemetry.count("rewrite alice rejected") != 2 || a.telemetry.count("rewrite alice failed") != 1 {
		t.Fatalf("rewrite metrics = %v", a.telemetry.events)
	}
}

// The note of an alert for the next turn is cut before a link that would not fit whole, never
// inside its address.
func TestAgentNoticeDoesNotCutAnAddress(t *testing.T) {
	a := newAgentEngine(t)
	a.create("alice", watchID, agentSpec())

	links := make([][2]string, 0, 5)
	for index := range 5 {
		address := fmt.Sprintf("https://example.com/item/%d/", index)
		links = append(links, [2]string{fmt.Sprintf("Listing %d", index), address + strings.Repeat("a", 200-len(address))})
	}
	a.check("2026-09-28 10:00", linked("5 listings", strings.Repeat("Ж", 250), "Found in the saved search.", links...))
	sent := a.wantSent(1)
	if !strings.Contains(sent[0].text, links[4][1]+"\nFound in the saved search.") {
		t.Fatalf("alert = %q, want all five links", sent[0].text)
	}
	text, _ := a.service.AgentNotices().TakeNotice("alice")
	if !strings.HasSuffix(text, "\n🔗 Listing 2: "+links[2][1]+"\n…") || strings.Contains(text, "Listing 3") {
		t.Fatalf("notice = %q, want it to end after the third link", text)
	}
}
