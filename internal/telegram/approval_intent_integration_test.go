package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/judgment"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const intentWarningText = "This does not look like something you asked for."

type stubOptIn bool

func (s stubOptIn) JudgmentsEnabled(context.Context, string) (bool, error) { return bool(s), nil }

type typeSafeStub struct {
	mu       sync.Mutex
	states   []map[string]string
	noul     float64
	status   int
	server   *httptest.Server
	leakSeen []string
}

var pathWithLetters = regexp.MustCompile(`[A-Za-z]/|/[A-Za-z]`)

func newTypeSafeStub(t *testing.T, noul float64, status int) *typeSafeStub {
	t.Helper()
	stub := &typeSafeStub{noul: noul, status: status}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State map[string]string `json:"state"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		stub.mu.Lock()
		defer stub.mu.Unlock()
		stub.states = append(stub.states, body.State)
		for key, value := range body.State {
			joined := strings.ReplaceAll(value, "\n---\n", "\n")
			if pathWithLetters.MatchString(joined) || strings.Contains(joined, "http") || strings.Contains(joined, "@") {
				stub.leakSeen = append(stub.leakSeen, key+": "+value)
			}
		}
		if stub.status != http.StatusOK {
			w.WriteHeader(stub.status)
			return
		}
		fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"requested":{"type":"noul","noul":%v}}}`, stub.noul)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *typeSafeStub) requests() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.states...)
}

func (s *typeSafeStub) assertNoLeaks(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.leakSeen) > 0 {
		t.Fatalf("unredacted content reached TypeSafe: %v", s.leakSeen)
	}
}

func newIntentChecker(t *testing.T, stub *typeSafeStub, enabled bool) (*judgment.IntentChecker, *turnstate.Registry) {
	t.Helper()
	client, err := judgment.NewClient(judgment.Config{
		APIKey:   "test-key",
		Endpoint: stub.server.URL,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := turnstate.New(time.Now)
	return judgment.NewIntentChecker(client, stubOptIn(enabled), registry, registry), registry
}

// intentProposal runs one Notion append proposal through the real approval delivery and returns
// the message text the user saw together with the summary stored for the record.
func intentProposal(t *testing.T, stub *typeSafeStub, enabled bool, messages ...string) (string, string) {
	t.Helper()
	return intentProposalWith(t, stub, enabled, nil, messages...)
}

func intentProposalWith(t *testing.T, stub *typeSafeStub, enabled bool, prepare func(*turnstate.Registry), messages ...string) (string, string) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: writableNotionSimulator(t)})
	h.provision("alice", "acct-alice-1111")
	h.installNotionToken("alice", aliceNotionToken)
	h.start()
	h.enableNotionWrites(1, 2, 101)

	checker, registry := newIntentChecker(t, stub, enabled)
	for _, message := range messages {
		registry.RecordMessage("alice", message)
	}
	if prepare != nil {
		prepare(registry)
	}
	h.approvals.SetIntentChecker(checker)

	proposal := h.proposeTurn(runtime, 3, "notion_append_blocks", `{"page_id":"page-groceries","paragraphs":["Remember bread"]}`)
	confirmData, _ := approvalButtons(t, proposal.keyboard)
	var summary string
	if err := h.db.QueryRowContext(h.ctx, `SELECT summary FROM approvals WHERE id = ?`, approvalID(confirmData)).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	stub.assertNoLeaks(t)
	return proposal.text, summary
}

func TestIntentCheckWarnsOnUnrequestedAction(t *testing.T) {
	stub := newTypeSafeStub(t, 0.1, http.StatusOK)
	text, summary := intentProposal(t, stub, true, "what is the weather like at https://example.com/forecast?", "mail bob@example.com the file /volume1/docs/tax.pdf")
	if !strings.HasPrefix(text, "⚠️ "+intentWarningText) || !strings.HasPrefix(summary, "⚠️ "+intentWarningText) {
		t.Fatalf("warning missing: text=%q summary=%q", text, summary)
	}
	if !strings.Contains(text, "Notion · append 1 paragraph(s)") || !strings.Contains(text, "Expires in") {
		t.Fatalf("proposal body lost: %q", text)
	}
	if len(stub.requests()) != 1 {
		t.Fatalf("requests = %d, want 1", len(stub.requests()))
	}
}

func TestIntentCheckLeavesRequestedActionAlone(t *testing.T) {
	stub := newTypeSafeStub(t, 0.9, http.StatusOK)
	text, summary := intentProposal(t, stub, true, "add bread to my shopping page")
	if strings.Contains(text, intentWarningText) || strings.Contains(summary, intentWarningText) {
		t.Fatalf("unexpected warning: text=%q summary=%q", text, summary)
	}
	if len(stub.requests()) != 1 {
		t.Fatalf("requests = %d, want 1", len(stub.requests()))
	}
}

func TestIntentCheckFailsOpenOnServerError(t *testing.T) {
	stub := newTypeSafeStub(t, 0.1, http.StatusInternalServerError)
	text, summary := intentProposal(t, stub, true, "hello")
	if strings.Contains(text, intentWarningText) || strings.Contains(summary, intentWarningText) {
		t.Fatalf("unexpected warning: text=%q summary=%q", text, summary)
	}
	if !strings.Contains(text, "Expires in") {
		t.Fatalf("proposal not sent: %q", text)
	}
}

func TestIntentCheckSeesConfirmationWithThePriorRequest(t *testing.T) {
	stub := newTypeSafeStub(t, 0.9, http.StatusOK)
	text, _ := intentProposal(t, stub, true, "add milk to my shopping page", "yes")
	if strings.Contains(text, intentWarningText) {
		t.Fatalf("unexpected warning: %q", text)
	}
	requests := stub.requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	recent := requests[0]["recent_messages"]
	if !strings.Contains(recent, "add milk to my shopping page") || !strings.HasSuffix(recent, "yes") {
		t.Fatalf("recent_messages = %q", recent)
	}
}

func TestIntentCheckSendsNothingWithoutOptIn(t *testing.T) {
	stub := newTypeSafeStub(t, 0.1, http.StatusOK)
	text, _ := intentProposal(t, stub, false, "hello")
	if strings.Contains(text, intentWarningText) || len(stub.requests()) != 0 {
		t.Fatalf("text=%q requests=%d", text, len(stub.requests()))
	}
}

func TestIntentCheckUsesStricterThresholdAfterExposure(t *testing.T) {
	exposed := newTypeSafeStub(t, 0.4, http.StatusOK)
	text, _ := intentProposalWith(t, exposed, true, func(r *turnstate.Registry) { r.MarkExposed("alice") }, "add bread to my shopping page")
	if !strings.HasPrefix(text, "⚠️ "+intentWarningText) {
		t.Fatalf("exposed principal not warned: %q", text)
	}

	clean := newTypeSafeStub(t, 0.4, http.StatusOK)
	text, _ = intentProposal(t, clean, true, "add bread to my shopping page")
	if strings.Contains(text, intentWarningText) {
		t.Fatalf("unexposed principal warned: %q", text)
	}
}

func TestIntentCheckWarnsWithoutRequestWhenSuspicious(t *testing.T) {
	stub := newTypeSafeStub(t, 0.9, http.StatusOK)
	text, _ := intentProposalWith(t, stub, true, func(r *turnstate.Registry) { r.MarkSuspicious("alice") }, "add bread to my shopping page")
	if !strings.HasPrefix(text, "⚠️ "+intentWarningText) {
		t.Fatalf("suspicious principal not warned: %q", text)
	}
	if len(stub.requests()) != 0 {
		t.Fatalf("requests = %d, want 0", len(stub.requests()))
	}
}
