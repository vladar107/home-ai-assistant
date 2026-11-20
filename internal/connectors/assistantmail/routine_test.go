package assistantmail_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	routineTool  = "assistant_mail_reply_routine"
	carol        = "carol@partner.example.net"
	carolFrom    = "Carol Jones <" + carol + ">"
	tableSubject = "Table for four on Friday"
	thanks       = "Thank you, that works for us.\nAssistant, assistant to Alice Example"
)

// recordingUnattended is the approval service's unattended entry point, keeping each record it made.
type recordingUnattended struct {
	approvals *approval.Service
	mu        sync.Mutex
	records   []approval.Record
}

func (r *recordingUnattended) ActUnattended(ctx context.Context, principalID string, proposal approval.Proposal, executor approval.Executor) (approval.Record, error) {
	record, err := r.approvals.ActUnattended(ctx, principalID, proposal, executor)
	if err == nil {
		r.mu.Lock()
		r.records = append(r.records, record)
		r.mu.Unlock()
	}
	return record, err
}

func (r *recordingUnattended) made() []approval.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records)
}

// quietApprovals is a chat that takes every approval message and shows nothing.
type quietApprovals struct{}

func (quietApprovals) SendText(context.Context, int64, string) (int64, error) { return 1, nil }
func (quietApprovals) SendProposal(context.Context, int64, approval.Shown, approval.Buttons) (int64, approval.WidgetKind, error) {
	return 1, approval.NotWidget, nil
}
func (quietApprovals) EditText(context.Context, int64, int64, approval.Shown) error { return nil }
func (quietApprovals) EditProposal(context.Context, int64, int64, approval.Shown, approval.Buttons) error {
	return nil
}
func (quietApprovals) SendPreview(context.Context, int64, int64, string) error { return nil }
func (quietApprovals) SendDirectActions(context.Context, int64, approval.DirectActions) (int64, error) {
	return 1, nil
}
func (quietApprovals) EditDirectActions(context.Context, int64, int64, approval.DirectActions) error {
	return nil
}

// routineHarness is a triage harness whose runs may send routine replies through the real approval
// service, for alice, who has routine replies on.
type routineHarness struct {
	*harness
	checker  *triageChecker
	recorded *recordingUnattended
	// confirmed counts the sends made as confirmed ones, which every routine reply follows.
	confirmed int
	mu        sync.Mutex
	results   []string
}

func newRoutineHarness(t *testing.T, configure ...func(*assistantmail.Config)) *routineHarness {
	t.Helper()
	return newRoutineHarnessOver(t, func(store *sqlite.Store) approval.Repository { return store }, configure...)
}

// newRoutineHarnessOver is a routine harness whose approval service keeps its records in the
// repository made over the store.
func newRoutineHarnessOver(t *testing.T, repository func(*sqlite.Store) approval.Repository, configure ...func(*assistantmail.Config)) *routineHarness {
	t.Helper()
	recorded := &recordingUnattended{}
	h, checker := newTriageHarness(t, append([]func(*assistantmail.Config){func(cfg *assistantmail.Config) {
		approvals, err := approval.NewService(approval.Config{}, repository(cfg.Repository.(*sqlite.Store)), quietApprovals{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		recorded.approvals = approvals
		cfg.Unattended = recorded
		cfg.DisplayNames = map[string]string{"alice": "Alice Example", "bob": "Bob Example"}
	}}, configure...)...)
	if err := h.switches.SetEnabled(h.ctx, "alice", assistantmail.RoutineSwitch, true); err != nil {
		t.Fatal(err)
	}
	h.start()
	return &routineHarness{harness: h, checker: checker, recorded: recorded}
}

// restart builds the service again over the same store, as a new process would, and lets the
// triage runs reach the new one.
func (h *routineHarness) restart() {
	h.t.Helper()
	h.harness.restart()
	broker, err := tools.NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		h.t.Fatal(err)
	}
	broker.SetSwitches(h.switches)
	for _, provider := range []tools.Provider{h.service, notesTool{}} {
		if err := broker.Register(provider); err != nil {
			h.t.Fatal(err)
		}
	}
	h.checker.broker = broker
}

// confirmSend sends an email for alice as a confirmed send does and returns the sent message's id
// and thread.
func (h *routineHarness) confirmSend(arguments map[string]any) googlesim.SentMessage {
	h.t.Helper()
	h.confirmed++
	encoded, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	record := approval.Record{ID: fmt.Sprintf("confirmed-%d", h.confirmed), PrincipalID: "alice", Tool: "assistant_mail_send", Arguments: encoded}
	if _, err := h.service.ExecuteApproved(h.ctx, record); err != nil {
		h.t.Fatalf("confirmed send: %v", err)
	}
	sent := h.sim.Sent()
	return sent[len(sent)-1]
}

// startThread sends alice's email that starts a thread with the recipients, and returns the thread
// and the sent message's Message-ID.
func (h *routineHarness) startThread(subject string, to []string, cc ...string) (string, string) {
	h.t.Helper()
	arguments := map[string]any{"to": to, "subject": subject, "body": "Hello,\nCould you hold a table for four on Friday?\nAssistant, assistant to Alice Example"}
	if len(cc) > 0 {
		arguments["cc"] = cc
	}
	sent := h.confirmSend(arguments)
	return sent.ThreadID, "<" + sent.ID + "@googlesim.example>"
}

// answer delivers to alice's address a message in the thread that answers the one with the
// Message-ID, from Carol unless it names another sender.
func (h *routineHarness) answer(threadID, messageID string, message googlesim.Message) string {
	h.t.Helper()
	message.ThreadID, message.InReplyTo, message.References = threadID, messageID, messageID
	if message.From == "" {
		message.From = carolFrom
	}
	if message.Subject == "" {
		message.Subject = "Re: " + tableSubject
	}
	if message.PlainText == "" {
		message.PlainText = "Received, thanks. See you on Friday."
	}
	return h.deliver("assistant+a1@example.com", message)
}

// replying makes each of alice's runs that asks about a message send body as a routine reply to
// each id targets gives it, then list what listed names; every other run stays silent.
func (h *routineHarness) replying(body string, targets func(*triageRun) []string, listed map[string]string) {
	h.checker.answerWith(func(run *triageRun) (string, string, error) {
		if run.principal != "alice" {
			return silent(run)
		}
		for _, id := range targets(run) {
			arguments, err := json.Marshal(map[string]string{"reply_to_id": id, "body": body})
			if err != nil {
				return "", "", err
			}
			result := run.call(routineTool, string(arguments))
			h.mu.Lock()
			h.results = append(h.results, result)
			h.mu.Unlock()
		}
		return needs(listed)(run)
	})
}

// newOnes answers every new message a run asks about.
func newOnes(run *triageRun) []string { return run.ids() }

// only answers id, in the run that asks about asked.
func only(asked, id string) func(*triageRun) []string {
	return func(run *triageRun) []string {
		if run.asks(asked) {
			return []string{id}
		}
		return nil
	}
}

func (h *routineHarness) said() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.results)
}

// sentCount is how many messages the mailbox sent in all.
func (h *routineHarness) sentCount() int { return len(h.sim.Sent()) }

func (h *routineHarness) expectLogsWithout(texts ...string) {
	h.t.Helper()
	h.expectQuietLogs()
	logs := h.logs.String()
	for _, text := range texts {
		if strings.Contains(logs, text) {
			h.t.Fatalf("logs mention %q:\n%s", text, logs)
		}
	}
}

func TestARoutineReplyIsSentRecordedAndReportedInFull(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	thread, sentID := h.startThread(tableSubject, []string{carol})
	answered := h.answer(thread, sentID, googlesim.Message{})
	h.replying(thanks, newOnes, nil)
	h.poll()

	h.expectNotices(sentNotice{chatID: 101, text: "✉️ Replied for you:\n" +
		"• To carol@partner.example.net:\n" +
		"  Thank you, that works for us.\n" +
		"  Assistant, assistant to Alice Example"})
	runs := h.checker.ran()
	if len(runs) != 1 || !slices.Contains(runs[0].offered, routineTool) ||
		!strings.Contains(runs[0].message, routineTool) || !strings.Contains(runs[0].message, `"Assistant, assistant to Alice Example"`) {
		t.Fatalf("runs = %+v", runs)
	}
	if said := h.said(); len(said) != 1 || !strings.Contains(said[0], "Sent.") {
		t.Fatalf("the tool said %q", said)
	}
	sent := h.sim.Sent()
	if len(sent) != 2 || sent[1].ThreadID != thread {
		t.Fatalf("sent = %+v", sent)
	}
	header, body := sentMessage(t, sent[1].Raw)
	if header.Get("To") != carol || header.Get("Cc") != "" || header.Get("Bcc") != "" || header.Get("Subject") != "Re: "+tableSubject ||
		header.Get("In-Reply-To") != "<"+answered+"@googlesim.example>" || header.Get("Reply-To") != "assistant+a1@example.com" ||
		header.Get("From") != `"Assistant" <assistant@example.com>` || body != thanks {
		t.Fatalf("reply = %v\n%q", header, body)
	}
	if contentType := header.Get("Content-Type"); contentType != "text/plain; charset=UTF-8" {
		t.Fatalf("content type = %q", contentType)
	}

	records := h.recorded.made()
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	stored, found, err := h.store.LoadApproval(h.ctx, records[0].ID)
	if err != nil || !found {
		t.Fatalf("record = %v %v", found, err)
	}
	var kept map[string]string
	if err := json.Unmarshal(stored.Arguments, &kept); err != nil {
		t.Fatal(err)
	}
	if stored.Tool != routineTool || stored.Status != approval.StatusDone || stored.Mode != approval.ModeDirect || len(stored.Undo) != 0 ||
		kept["reply_to_id"] != answered || kept["to"] != carol || kept["subject"] != "Re: "+tableSubject || kept["body"] != thanks {
		t.Fatalf("record = %+v, arguments %v", stored, kept)
	}
	if told := h.notices.PendingNotice("alice"); !strings.Contains(told, `you replied for the user, without asking, to "carol@partner.example.net"`) ||
		!strings.Contains(told, "Thank you, that works for us.") {
		t.Fatalf("turn notice = %q", told)
	}
	if h.pending("alice") != 0 || h.metrics.routine("sent") != 1 {
		t.Fatalf("pending %d, sent %d", h.pending("alice"), h.metrics.routine("sent"))
	}

	h.poll()
	h.expectNotices()
	h.expectLogsWithout(carol, "partner.example.net", "Thank you", tableSubject)
}

// Gmail's Authentication-Results in the form it has for mail from a Gmail address, comments and
// all, verifies the sender.
func TestARoutineReplyTrustsGmailsOwnDMARCPass(t *testing.T) {
	t.Parallel()
	const someone = "someone@gmail.com"
	h := newRoutineHarness(t)
	thread, sentID := h.startThread(tableSubject, []string{someone})
	h.answer(thread, sentID, googlesim.Message{
		From: "Someone <" + someone + ">",
		AuthenticationResults: "mx.google.com; dkim=pass header.i=@gmail.com header.s=20260707 header.b=c5dYF2v7; " +
			"spf=pass (google.com: domain of someone@gmail.com designates 209.85.220.41 as permitted sender) smtp.mailfrom=someone@gmail.com; " +
			"dmarc=pass (p=NONE sp=QUARANTINE dis=NONE) header.from=gmail.com; dara=pass header.i=@gmail.com",
	})
	h.replying(thanks, newOnes, nil)
	h.poll()

	if sent := h.sim.Sent(); len(sent) != 2 || sent[1].ThreadID != thread || h.metrics.routine("sent") != 1 {
		t.Fatalf("sent %+v, routine replies %d", sent, h.metrics.routine("sent"))
	}
}

// A reply the run wrote too long is refused, and the run's next, short one is sent; the message is
// then not listed, since it was answered.
func TestARoutineReplyCanBeWrittenAgainWithinTheRun(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	thread, sentID := h.startThread(tableSubject, []string{carol})
	h.answer(thread, sentID, googlesim.Message{})
	h.checker.answerWith(func(run *triageRun) (string, string, error) {
		for _, body := range []string{strings.Repeat("Thank you. ", 60), thanks} {
			arguments, _ := json.Marshal(map[string]string{"reply_to_id": run.ids()[0], "body": body})
			h.results = append(h.results, run.call(routineTool, string(arguments)))
		}
		return silent(run)
	})
	h.poll()

	if said := h.said(); len(said) != 2 || !strings.Contains(said[0], "at most 600 characters") || !strings.Contains(said[1], "Sent.") {
		t.Fatalf("the tool said %q", said)
	}
	notices := h.chat.take()
	if len(notices) != 1 || strings.Contains(notices[0].text, "needs you") || !strings.Contains(notices[0].text, "Replied for you") {
		t.Fatalf("notices = %+v", notices)
	}
	if h.metrics.routine("invalid") != 1 || h.metrics.routine("sent") != 1 {
		t.Fatalf("invalid %d, sent %d", h.metrics.routine("invalid"), h.metrics.routine("sent"))
	}
}

// Each refused routine reply sends nothing; a refused reply to a new message of the thread lists it
// with the code's own reason, and a refusal of another message lists nothing.
func TestARoutineReplyIsRefusedUnlessEveryRuleHolds(t *testing.T) {
	t.Parallel()
	const (
		notOurs   = "I did not answer it myself: I answer by myself only in a thread I started for you."
		sender    = "I did not answer it myself: its sender is not a verified address I wrote to in this thread."
		automatic = "I did not answer it myself: it looks like automated mail."
		limited   = "I did not answer it myself: the limit of replies I send by myself was reached."
		group     = "I did not answer it myself: I answer by myself only when I wrote to one person in the thread."
		invalid   = "I did not answer it myself: the reply I wrote could not be sent as it was."
	)
	// started opens a thread with Carol and returns it with its Message-ID.
	started := func(h *routineHarness) (string, string) { return h.startThread(tableSubject, []string{carol}) }
	// answered is Carol's reply in a thread the assistant started, with the changes given.
	answered := func(change func(*googlesim.Message)) func(*routineHarness) (string, string) {
		return func(h *routineHarness) (string, string) {
			thread, sentID := started(h)
			message := googlesim.Message{}
			if change != nil {
				change(&message)
			}
			id := h.answer(thread, sentID, message)
			return id, id
		}
	}
	for _, refusal := range []struct {
		name      string
		configure func(*assistantmail.Config)
		// arrange returns the new message whose run replies, and the id the reply names.
		arrange func(*routineHarness) (asked, target string)
		outcome string
		// why is the reason the notice gives the asked message, or "" for no notice.
		why string
		// listedByRun is what the run lists the asked message with itself.
		listedByRun string
	}{
		{name: "another principal's message", outcome: "not_ours", arrange: func(h *routineHarness) (string, string) {
			bobs := h.deliver("assistant+b1@example.com", googlesim.Message{From: carolFrom, Subject: "Hello"})
			asked, _ := answered(nil)(h)
			return asked, bobs
		}},
		{name: "a message of another thread", outcome: "not_ours", arrange: func(h *routineHarness) (string, string) {
			other := h.deliver("assistant+a1@example.com", googlesim.Message{From: carolFrom, Subject: "Another matter"})
			asked, _ := answered(nil)(h)
			return asked, other
		}},
		{name: "spam", outcome: "not_ours", arrange: func(h *routineHarness) (string, string) {
			thread, sentID := started(h)
			spam := h.answer(thread, sentID, googlesim.Message{Labels: []string{"SPAM"}})
			asked := h.answer(thread, sentID, googlesim.Message{})
			return asked, spam
		}},
		{name: "a thread the assistant did not start", outcome: "not_ours", why: notOurs, arrange: func(h *routineHarness) (string, string) {
			first := h.deliver("assistant+a1@example.com", googlesim.Message{From: carolFrom, Subject: tableSubject})
			h.poll()
			h.chat.take()
			sent := h.confirmSend(map[string]any{"to": []string{carol}, "subject": tableSubject, "body": "Yes, Friday works.", "reply_to_id": first})
			thread := sent.ThreadID
			id := h.answer(thread, "<"+sent.ID+"@googlesim.example>", googlesim.Message{})
			return id, id
		}},
		{name: "a sender outside the assistant's recipients", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.From = "Dan Moss <dan@partner.example.net>"
		})},
		{name: "a stranger's forged References", outcome: "sender", why: sender, arrange: func(h *routineHarness) (string, string) {
			_, sentID := started(h)
			forged := h.deliver(mailbox, googlesim.Message{
				From: "Eve <eve@elsewhere.example.org>", Subject: "Re: " + tableSubject, InReplyTo: sentID, References: sentID,
			})
			return forged, forged
		}},
		{name: "a differing Reply-To", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.ReplyTo = "Billing <billing@elsewhere.example.org>"
		})},
		{name: "a failing DMARC check", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = googlesim.AuthenticationResults(carol, "fail")
		})},
		{name: "a failing DMARC check the run lists itself", outcome: "sender", why: "Check who wrote this.", listedByRun: "Check who wrote this.",
			arrange: answered(func(message *googlesim.Message) {
				message.AuthenticationResults = googlesim.AuthenticationResults(carol, "fail")
			})},
		{name: "a domain without DMARC", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = "mx.google.com; dkim=pass header.i=@partner.example.net; spf=pass smtp.mailfrom=" + carol
		})},
		{name: "a passing result below Gmail's own", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = "mx.google.com; spf=pass smtp.mailfrom=" + carol
			message.Extra = []googlesim.Header{{Name: "Authentication-Results", Value: "mx.google.com; dmarc=pass header.from=partner.example.net"}}
		})},
		{name: "a passing result inside another clause", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = "mx.google.com; spf=pass (google.com: dmarc=pass header.from=partner.example.net) smtp.mailfrom=" + carol
		})},
		{name: "a passing result forged in a quoted envelope sender", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = `mx.google.com; spf=pass smtp.mailfrom="x;dmarc=pass header.from=partner.example.net y"@attacker.example.org`
		})},
		{name: "a passing result after a semicolon in a comment", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = "mx.google.com; spf=pass (google.com: x; dmarc=pass header.from=partner.example.net) smtp.mailfrom=" + carol
		})},
		{name: "a passing result hidden by nested and escaped parentheses", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = `mx.google.com; spf=pass (google.com: (x) \) ; dmarc=pass header.from=partner.example.net ) smtp.mailfrom=` + carol
		})},
		{name: "a passing result after a quote inside a comment", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = `mx.google.com; spf=pass (google.com: " x) smtp.mailfrom="y; dmarc=pass header.from=partner.example.net z"@attacker.example.org`
		})},
		{name: "an unterminated quoted string", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = `mx.google.com; dmarc=pass header.from=partner.example.net; spf=pass smtp.mailfrom="x@attacker.example.org`
		})},
		{name: "an unterminated comment", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = "mx.google.com; dmarc=pass header.from=partner.example.net; spf=pass (google.com: x smtp.mailfrom=" + carol
		})},
		{name: "a quoted header.from followed by another domain", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = `mx.google.com; dmarc=pass header.from="attacker.example.org" partner.example.net`
		})},
		{name: "two DMARC results", outcome: "sender", why: sender, arrange: answered(func(message *googlesim.Message) {
			message.AuthenticationResults = "mx.google.com; dmarc=pass header.from=partner.example.net; dmarc=pass header.from=elsewhere.example.org"
		})},
		{name: "a thread written to several people", outcome: "group", why: group, arrange: func(h *routineHarness) (string, string) {
			thread, sentID := h.startThread(tableSubject, []string{carol}, "host@partner.example.net")
			id := h.answer(thread, sentID, googlesim.Message{})
			return id, id
		}},
		{name: "a message without a Message-ID", outcome: "invalid", why: invalid, arrange: answered(func(message *googlesim.Message) {
			message.MessageID = "no-id"
		})},
		{name: "an automatic reply", outcome: "automated", why: automatic, arrange: answered(func(message *googlesim.Message) {
			message.Extra = []googlesim.Header{{Name: "Auto-Submitted", Value: "auto-replied"}}
		})},
		{name: "bulk mail", outcome: "automated", why: automatic, arrange: answered(func(message *googlesim.Message) {
			message.Extra = []googlesim.Header{{Name: "Precedence", Value: "bulk"}}
		})},
		{name: "list mail", outcome: "automated", why: automatic, arrange: answered(func(message *googlesim.Message) {
			message.Extra = []googlesim.Header{{Name: "List-Id", Value: "<news.partner.example.net>"}}
		})},
		{name: "a no-reply sender", outcome: "automated", why: automatic, arrange: func(h *routineHarness) (string, string) {
			thread, sentID := h.startThread(tableSubject, []string{"no-reply@partner.example.net"})
			id := h.answer(thread, sentID, googlesim.Message{From: "Bookings <no-reply@partner.example.net>"})
			return id, id
		}},
		{name: "a second reply in the thread", outcome: "limited", why: limited, arrange: func(h *routineHarness) (string, string) {
			thread, sentID := started(h)
			h.answer(thread, sentID, googlesim.Message{})
			h.replying(thanks, newOnes, nil)
			h.poll()
			if notices := h.chat.take(); len(notices) != 1 || !strings.Contains(notices[0].text, "Replied for you") {
				h.t.Fatalf("first reply's notices = %+v", notices)
			}
			id := h.answer(thread, sentID, googlesim.Message{PlainText: "Thanks again!"})
			return id, id
		}},
		{name: "the day's routine replies", outcome: "limited", why: limited, arrange: func(h *routineHarness) (string, string) {
			for range 3 {
				thread, sentID := started(h)
				h.answer(thread, sentID, googlesim.Message{})
			}
			h.replying(thanks, newOnes, nil)
			h.poll()
			if replied := h.metrics.routine("sent"); replied != 3 {
				h.t.Fatalf("routine replies before the limit = %d", replied)
			}
			h.chat.take()
			asked, _ := answered(nil)(h)
			return asked, asked
		}},
		{name: "the day's sends", outcome: "limited", why: limited, configure: func(cfg *assistantmail.Config) {
			cfg.Mailbox.DailySendLimit = 1
		}, arrange: answered(nil)},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()
			var configure []func(*assistantmail.Config)
			if refusal.configure != nil {
				configure = append(configure, refusal.configure)
			}
			h := newRoutineHarness(t, configure...)
			asked, target := refusal.arrange(h)
			before := h.sentCount()
			listed := map[string]string{}
			if refusal.listedByRun != "" {
				listed[asked] = refusal.listedByRun
			}
			h.replying(thanks, only(asked, target), listed)
			h.poll()

			if sent := h.sentCount(); sent != before {
				t.Fatalf("%d messages sent, want none", sent-before)
			}
			if count := h.metrics.routine(refusal.outcome); count != 1 {
				t.Fatalf("%s refusals = %d", refusal.outcome, count)
			}
			said := h.said()
			if last := said[len(said)-1]; !strings.Contains(last, "Not sent:") {
				t.Fatalf("the tool said %q", last)
			}
			notices := h.chat.take()
			if refusal.why == "" {
				if len(notices) != 0 {
					t.Fatalf("notices = %+v", notices)
				}
				return
			}
			if len(notices) != 1 || !strings.Contains(notices[0].text, "needs you:\n") || strings.Count(notices[0].text, "• ") != 1 ||
				!strings.Contains(notices[0].text, "\n  "+refusal.why+"\n") {
				t.Fatalf("notices = %+v", notices)
			}
			if h.pending("alice") != 0 {
				t.Fatalf("pending = %d", h.pending("alice"))
			}
			h.expectLogsWithout(carol, "partner.example.net", "Thank you")
		})
	}
}

// With routine replies off, the default, a triage run is offered no routine-reply tool, and a call
// of it sends nothing.
func TestRoutineRepliesAreOffUntilTheUserTurnsThemOn(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	if err := h.switches.SetEnabled(h.ctx, "alice", assistantmail.RoutineSwitch, false); err != nil {
		t.Fatal(err)
	}
	thread, sentID := h.startThread(tableSubject, []string{carol})
	h.answer(thread, sentID, googlesim.Message{})
	h.replying(thanks, newOnes, nil)
	h.poll()

	runs := h.checker.ran()
	if len(runs) != 1 || slices.Contains(runs[0].offered, routineTool) || strings.Contains(runs[0].message, routineTool) {
		t.Fatalf("runs = %+v", runs)
	}
	if h.sentCount() != 1 || len(h.recorded.made()) != 0 {
		t.Fatalf("sent %d, records %d", h.sentCount(), len(h.recorded.made()))
	}
	h.expectNotices()

	if enabled, err := h.switches.Enabled(h.ctx, "bob", assistantmail.RoutineSwitch); err != nil || enabled {
		t.Fatalf("bob's routine replies = %v %v, want off by default", enabled, err)
	}
}

// The routine-reply tool is never offered outside a triage run, even with routine replies on: not to
// a chat turn, not to a scheduled run, and not to a run scoped to it without naming it a write.
func TestTheRoutineReplyToolIsOnlyInTriageRuns(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	broker := h.checker.broker
	offered := func(ctx context.Context, all bool) []string {
		t.Helper()
		lookup := broker.ReadOnlyToolsFor
		if all {
			lookup = broker.ToolsFor
		}
		set, err := lookup(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, definition := range set.Definitions {
			names = append(names, definition.Name)
		}
		return names
	}
	scoped := tools.WithScope(h.ctx, tools.Scope{Tools: []string{"assistant_mail_list", routineTool}})
	for name, names := range map[string][]string{
		"chat": offered(h.ctx, true), "scheduled": offered(h.ctx, false), "scoped without the write": offered(scoped, false),
	} {
		if slices.Contains(names, routineTool) || len(names) == 0 {
			t.Fatalf("%s run offers %v", name, names)
		}
	}
	thread, sentID := h.startThread(tableSubject, []string{carol})
	id := h.answer(thread, sentID, googlesim.Message{})
	h.poll()
	if result := h.execute("alice", routineTool, map[string]string{"reply_to_id": id, "body": thanks}); !result.IsError {
		t.Fatalf("an unscoped call = %+v", result)
	}
	if h.sentCount() != 1 {
		t.Fatalf("sent %d", h.sentCount())
	}
}

// A reply whose notice could not be delivered is reported in the next notice that is: a chat that
// refuses the bot settles the mail, but the reply waits, without a notice of its own every poll,
// until new mail brings one.
func TestARoutineReplyIsReportedByTheNextDeliveredNotice(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	thread, sentID := h.startThread(tableSubject, []string{carol})
	h.answer(thread, sentID, googlesim.Message{})
	h.replying(thanks, newOnes, nil)
	h.chat.failWith(blockedError{})
	h.poll()
	if h.pending("alice") != 0 || h.metrics.routine("sent") != 1 {
		t.Fatalf("pending %d, sent %d", h.pending("alice"), h.metrics.routine("sent"))
	}
	h.chat.failWith(errors.New("telegram is down"))
	h.poll()
	h.chat.failWith(nil)
	h.poll()
	h.expectNotices()

	h.checker.answerWith(needs(map[string]string{}))
	order := h.deliver("assistant+a1@example.com", googlesim.Message{Subject: "Delivery slot"})
	h.checker.answerWith(needs(map[string]string{order: "Pick a delivery slot."}))
	h.poll()
	h.expectNotices(sentNotice{chatID: 101, text: "✉️ Replied for you:\n" +
		"• To carol@partner.example.net:\n" +
		"  Thank you, that works for us.\n" +
		"  Assistant, assistant to Alice Example\n" +
		"\n📬 1 email at your assistant address needs you:\n" +
		"• Shop (orders@shop.example.org) · Delivery slot\n" +
		"  Pick a delivery slot.\n" +
		"\nAsk me to read any of them."})
	h.poll()
	h.expectNotices()
}

// A reply whose notice failed for the moment is reported when the notice is sent again.
func TestARoutineReplyWaitsOutAFailedNotice(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	thread, sentID := h.startThread(tableSubject, []string{carol})
	h.answer(thread, sentID, googlesim.Message{})
	h.replying(thanks, newOnes, nil)
	h.chat.failWith(errors.New("telegram is down"))
	h.poll()
	h.chat.failWith(nil)
	h.poll()
	notices := h.chat.take()
	if len(notices) != 1 || !strings.Contains(notices[0].text, "• To carol@partner.example.net:\n  Thank you, that works for us.") {
		t.Fatalf("notices = %+v", notices)
	}
	if runs := len(h.checker.ran()); runs != 1 || len(h.recorded.made()) != 1 {
		t.Fatalf("runs %d, replies %d", runs, len(h.recorded.made()))
	}
}

const unsure = "  It may not have gone out; check the thread in the assistant mailbox."

// A reply whose send timed out may have gone out, so the notice shows it whole as maybe sent, and
// lists the message for the user.
func TestARoutineReplyWhoseSendTimedOutIsReportedAsMaybeSent(t *testing.T) {
	t.Parallel()
	h := newRoutineHarness(t)
	thread, sentID := h.startThread(tableSubject, []string{carol})
	h.answer(thread, sentID, googlesim.Message{})
	h.sim.SlowSends(5 * time.Second)
	h.checker.answerWith(func(run *triageRun) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		arguments, _ := json.Marshal(map[string]string{"reply_to_id": run.ids()[0], "body": thanks})
		h.mu.Lock()
		h.results = append(h.results, run.callWithin(ctx, routineTool, string(arguments)))
		h.mu.Unlock()
		return silent(run)
	})
	h.poll()

	if said := h.said(); len(said) != 1 || !strings.Contains(said[0], "Not sent:") {
		t.Fatalf("the tool said %q", said)
	}
	if h.sentCount() != 2 || h.metrics.routine("failed") != 1 {
		t.Fatalf("sent %d, failed %d", h.sentCount(), h.metrics.routine("failed"))
	}
	stored, _, err := h.store.LoadApproval(h.ctx, h.recorded.made()[0].ID)
	if err != nil || stored.Status != approval.StatusFailed || stored.Reason != assistantmail.ReasonPossiblySent {
		t.Fatalf("record = %+v %v", stored, err)
	}
	notices := h.chat.take()
	if len(notices) != 1 || !strings.HasPrefix(notices[0].text, "✉️ Replied for you:\n"+
		"• To carol@partner.example.net:\n"+unsure+"\n"+
		"  Thank you, that works for us.\n"+
		"  Assistant, assistant to Alice Example\n\n📬 1 email at your assistant address needs you:") ||
		!strings.Contains(notices[0].text, "I tried to answer it myself, but the reply failed") {
		t.Fatalf("notices = %+v", notices)
	}
}

// unfinished stands in for a store whose process stopped right after a reply's send, before its
// record was finished.
type unfinished struct{ approval.Repository }

func (unfinished) FinishDirectApproval(context.Context, string, string, string, string, string, json.RawMessage, time.Time) (bool, error) {
	return false, nil
}

// unmarked stands in for a process that stops after a reply's send, before it marks the thread's
// mail: marking fails while stop is set.
type unmarked struct {
	assistantmail.Repository
	stop *atomic.Bool
}

func (u unmarked) MarkNotified(ctx context.Context, principalID string, ids []string, at time.Time) error {
	if u.stop.Load() {
		return errors.New("the process stopped")
	}
	return u.Repository.MarkNotified(ctx, principalID, ids, at)
}

// After a restart, a reply sent before it, or one whose record never finished, is still reported
// once, and the new triage of its thread lists no refusal for the reply it may not send again.
func TestARoutineReplyOutlivesARestartBeforeItsNotice(t *testing.T) {
	t.Parallel()
	for _, crash := range []struct {
		name       string
		repository func(*sqlite.Store) approval.Repository
		entry      string
	}{
		{"sent", func(store *sqlite.Store) approval.Repository { return store }, "• To carol@partner.example.net:\n"},
		{"record never finished", func(store *sqlite.Store) approval.Repository { return unfinished{store} }, "• To carol@partner.example.net:\n" + unsure + "\n"},
	} {
		t.Run(crash.name, func(t *testing.T) {
			t.Parallel()
			stop := &atomic.Bool{}
			h := newRoutineHarnessOver(t, crash.repository, func(cfg *assistantmail.Config) {
				cfg.Repository = unmarked{Repository: cfg.Repository, stop: stop}
			})
			thread, sentID := h.startThread(tableSubject, []string{carol})
			h.answer(thread, sentID, googlesim.Message{})
			h.replying(thanks, newOnes, nil)
			stop.Store(true)
			h.poll()
			if h.sentCount() != 2 || h.pending("alice") != 1 {
				t.Fatalf("sent %d, pending %d", h.sentCount(), h.pending("alice"))
			}
			h.expectNotices()
			stop.Store(false)

			h.restart()
			approvals, err := approval.NewService(approval.Config{}, h.store, quietApprovals{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if err := approvals.Start(h.ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = approvals.Shutdown(context.Background()) })
			if told := approvals.PendingNotice("alice"); told != "" {
				t.Fatalf("the restart told a conversation of the reply: %q", told)
			}
			h.poll()

			if runs := len(h.checker.ran()); runs != 2 || h.sentCount() != 2 {
				t.Fatalf("runs %d, sent %d", runs, h.sentCount())
			}
			if said := h.said(); len(said) != 2 || !strings.Contains(said[1], "this thread was already answered") {
				t.Fatalf("the tool said %q", said)
			}
			h.expectNotices(sentNotice{chatID: 101, text: "✉️ Replied for you:\n" + crash.entry +
				"  Thank you, that works for us.\n" +
				"  Assistant, assistant to Alice Example"})
			if h.pending("alice") != 0 {
				t.Fatalf("pending = %d", h.pending("alice"))
			}
			h.poll()
			h.expectNotices()
		})
	}
}
