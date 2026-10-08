// Package searchcheck decides, for each query the model's web search is about to send out, whether
// it may run. A search cannot be stopped once it starts and the provider's search can open an
// address the query names, so after a conversation or run has read text from outside the household
// a query that could carry data out waits for the user's confirmation.
package searchcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	// Tool names the approval of a search; the search itself runs inside the runtime.
	Tool = "web_search"

	// approvalLifetime is how long a confirmed query may still be searched, once.
	approvalLifetime = 30 * time.Minute
	maximumApproved  = 16
	judgeTimeout     = 3 * time.Second

	ReasonClean      = "clean"
	ReasonConfirmed  = "confirmed"
	ReasonSuspicious = "suspicious"
	ReasonPattern    = "pattern"
	ReasonOffRequest = "off_request"
	ReasonChecked    = "checked"
	ReasonUnrecorded = "unrecorded"

	approvedOutcome = "Approved; the search runs now."
)

// Exposure reports whether the calling run or conversation has read text from outside the
// household.
type Exposure interface {
	External(ctx context.Context, principalID string) bool
}

// Proposer is the approval service as the check sees it.
type Proposer interface {
	Propose(ctx context.Context, principalID string, proposal approval.Proposal) (approval.Record, error)
}

// Judge is the optional TypeSafe part of the check, for principals who opted in.
type Judge interface {
	// Suspicious reports whether screening flagged recent content.
	Suspicious(ctx context.Context, principalID string) bool
	// OffRequest reports whether the query does not look like part of what the user asked for.
	OffRequest(ctx context.Context, principalID, query string) (bool, error)
}

type Telemetry interface {
	RecordSearchCheck(ctx context.Context, principal, verdict, reason string)
}

type approvedSearch struct {
	query string
	at    time.Time
}

type Service struct {
	exposure  Exposure
	proposer  Proposer
	logger    *slog.Logger
	now       func() time.Time
	judge     Judge
	telemetry Telemetry

	mu       sync.Mutex
	approved map[string][]approvedSearch
}

var _ approval.Executor = (*Service)(nil)

// NewService returns a check that allows every search of a run that has read nothing from outside
// the household. A nil now means time.Now.
func NewService(exposure Exposure, proposer Proposer, logger *slog.Logger, now func() time.Time) (*Service, error) {
	if exposure == nil {
		return nil, fmt.Errorf("search check exposure is required")
	}
	if proposer == nil {
		return nil, fmt.Errorf("search check proposer is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("search check logger is required")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{exposure: exposure, proposer: proposer, logger: logger, now: now, approved: make(map[string][]approvedSearch)}, nil
}

// SetJudge adds the TypeSafe checks; without them only the deterministic check applies.
func (s *Service) SetJudge(judge Judge) { s.judge = judge }

func (s *Service) SetTelemetry(telemetry Telemetry) { s.telemetry = telemetry }

// Check decides one query for the principal under the context of the run that makes it. A run with
// nobody to ask, such as a scheduled job or a background task, is interactive false, and a search
// that would wait for confirmation is denied instead.
func (s *Service) Check(ctx context.Context, principalID, query string, interactive bool) agentruntime.SearchDecision {
	query = normalize(query)
	decision := s.decide(ctx, principalID, query, interactive)
	if s.telemetry != nil {
		s.telemetry.RecordSearchCheck(ctx, principalID, string(decision.Verdict), decision.Reason)
	}
	return decision
}

func (s *Service) decide(ctx context.Context, principalID, query string, interactive bool) agentruntime.SearchDecision {
	if !s.exposure.External(ctx, principalID) {
		return allowed(ReasonClean)
	}
	if s.useApproval(principalID, query) {
		return allowed(ReasonConfirmed)
	}
	reason := s.concern(ctx, principalID, query)
	if reason == "" {
		return allowed(ReasonChecked)
	}
	phrase := reasonPhrases[reason]
	if !interactive {
		return agentruntime.SearchDecision{
			Verdict: agentruntime.SearchDeny,
			Reason:  reason,
			Text:    "The search was not run: " + phrase + ", and nobody is available to confirm it in this run. Continue without it, and do not reword the query to get around this.",
		}
	}
	arguments, err := json.Marshal(searchArguments{Query: query})
	if err == nil {
		_, err = s.proposer.Propose(ctx, principalID, approval.Proposal{
			Tool:      Tool,
			Arguments: arguments,
			Summary:   "Search the web for: " + codeSpan(query) + "\nNeeds your confirmation because " + phrase + ".",
			Intent:    approval.Intent{Verb: "search", TargetKind: "web query", TargetName: query},
		})
	}
	if err != nil {
		if approval.Refused(err) {
			return agentruntime.SearchDecision{Verdict: agentruntime.SearchDeny, Reason: reason, Text: "The search was not run. " + err.Error()}
		}
		s.logger.WarnContext(ctx, "search.proposal.failed", "principal_id", principalID, "error", err)
		return agentruntime.SearchDecision{Verdict: agentruntime.SearchDeny, Reason: ReasonUnrecorded, Text: "The search was not run, and asking the user to confirm it failed."}
	}
	return agentruntime.SearchDecision{
		Verdict: agentruntime.SearchConfirm,
		Reason:  reason,
		Text: "The search was not run: " + phrase + ". The user is asked to confirm it, and after your reply they see \"Search the web for: " + query + "\" with Confirm and Reject buttons. " +
			"Do not claim any results, do not answer from search, and do not reword the query to get around this. If the user confirms, search again with exactly the same query.",
	}
}

// concern names why the query must wait for the user, or returns "" when it may run.
func (s *Service) concern(ctx context.Context, principalID, query string) string {
	if s.judge != nil && s.judge.Suspicious(ctx, principalID) {
		return ReasonSuspicious
	}
	if judgment.CarriesData(query) {
		return ReasonPattern
	}
	if s.judge != nil {
		judgeCtx, cancel := context.WithTimeout(ctx, judgeTimeout)
		off, err := s.judge.OffRequest(judgeCtx, principalID, query)
		cancel()
		if err == nil && off {
			return ReasonOffRequest
		}
	}
	return ""
}

var reasonPhrases = map[string]string{
	ReasonSuspicious: "this conversation read content that tried to instruct the assistant",
	ReasonPattern:    "it contains a web address or other identifier",
	ReasonOffRequest: "it does not look like part of your request",
}

func allowed(reason string) agentruntime.SearchDecision {
	return agentruntime.SearchDecision{Verdict: agentruntime.SearchAllow, Reason: reason}
}

type searchArguments struct {
	Query string `json:"query"`
}

// codeSpan shows the query as code, so the chat prints it verbatim instead of turning a link or
// emphasis in it into formatting that hides the address. A fence longer than any backtick run in
// the query keeps the query from ending the span early.
func codeSpan(query string) string {
	longest, run := 0, 0
	for _, r := range query {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	if longest == 0 {
		return "`" + query + "`"
	}
	fence := strings.Repeat("`", longest+1)
	return fence + " " + query + " " + fence
}

// normalize trims the query and collapses its whitespace, which is how a confirmed query is
// matched with the one searched later, beside ignoring case.
func normalize(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func approvalKey(query string) string {
	return strings.ToLower(normalize(query))
}

func (s *Service) ActionTools() []string { return []string{Tool} }

// ExecuteApproved records the confirmation; the search itself runs when the model asks for it
// again with the same query.
func (s *Service) ExecuteApproved(_ context.Context, record approval.Record) (approval.Outcome, error) {
	var confirmed searchArguments
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil || strings.TrimSpace(confirmed.Query) == "" {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed search could not be read; nothing was searched."}
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.liveLocked(record.PrincipalID, now)
	if len(entries) >= maximumApproved {
		entries = entries[1:]
	}
	s.approved[record.PrincipalID] = append(entries, approvedSearch{query: approvalKey(confirmed.Query), at: now})
	return approval.Outcome{Text: approvedOutcome}, nil
}

// useApproval spends the principal's confirmation of this query, if there is a live one.
func (s *Service) useApproval(principalID, query string) bool {
	key := approvalKey(query)
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.liveLocked(principalID, s.now())
	for index, entry := range entries {
		if entry.query == key {
			s.approved[principalID] = append(entries[:index:index], entries[index+1:]...)
			return true
		}
	}
	return false
}

func (s *Service) liveLocked(principalID string, now time.Time) []approvedSearch {
	live := s.approved[principalID][:0:0]
	for _, entry := range s.approved[principalID] {
		if now.Sub(entry.at) < approvalLifetime {
			live = append(live, entry)
		}
	}
	if len(live) == 0 {
		delete(s.approved, principalID)
		return nil
	}
	s.approved[principalID] = live
	return live
}
