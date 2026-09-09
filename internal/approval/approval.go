// Package approval is the core approval state machine for confirmed actions: a connector proposes
// an exact action during a turn, the bot shows it to the same user with Confirm and Reject
// buttons, and the core executes a confirmed proposal exactly once through the connector.
package approval

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"
	StatusRejected  = "rejected"
	StatusExpired   = "expired"
	StatusExecuted  = "executed"
	StatusFailed    = "failed"

	ReasonReplaced       = "replaced"
	ReasonRestart        = "restart"
	ReasonInterrupted    = "interrupted"
	ReasonTimeout        = "timeout"
	ReasonNoExecutor     = "no_executor"
	ReasonExecutorFailed = "executor_failed"

	ActionConfirm = "confirm"
	ActionReject  = "reject"

	DefaultLifetime      = 10 * time.Minute
	DefaultSweepInterval = 30 * time.Second
	maximumSummaryRunes  = 3000
	executionTimeout     = 45 * time.Second
	idBytes              = 8

	ProposedText = "Proposed and awaiting the user's confirmation; nothing has been written yet. The user sees the exact proposal with Confirm and Reject buttons after your reply. Do not claim it was done."
)

// Proposal is what a connector asks the user to confirm: the exact tool call and its rendering.
type Proposal struct {
	Tool      string
	Arguments json.RawMessage
	Summary   string
}

// Record is the durable approval. Arguments hold the canonical form whose hash is stored.
type Record struct {
	ID          string
	PrincipalID string
	Tool        string
	Arguments   json.RawMessage
	Hash        string
	Summary     string
	Status      string
	Reason      string
	Outcome     string
	ChatID      int64
	MessageID   int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
	FinishedAt  time.Time
}

// Outcome is the content-free result line of an executed action, such as a created page URL.
type Outcome struct {
	Text string
}

// ActionError marks a failed execution with a short reason word and the sentence shown to the user.
type ActionError struct {
	Reason string
	Text   string
}

func (e *ActionError) Error() string { return "action failed: " + e.Reason }

// Executor is implemented by a connector for the tools it proposes. It runs only confirmed
// records and must be safe against a second call for the same record never happening.
type Executor interface {
	ActionTools() []string
	ExecuteApproved(context.Context, Record) (Outcome, error)
}

type Repository interface {
	// CreateApproval stores a pending record after expiring the principal's previous pending one
	// with ReasonReplaced; the replaced record is returned when there was one.
	CreateApproval(context.Context, Record) (Record, bool, error)
	LoadApproval(context.Context, string) (Record, bool, error)
	// PendingApproval returns the principal's pending record, if any.
	PendingApproval(context.Context, string) (Record, bool, error)
	// ConfirmApproval moves a pending, unexpired record of the principal to confirmed.
	ConfirmApproval(ctx context.Context, id, principalID string, now time.Time) (bool, error)
	// FinishApproval moves a record to a terminal status with a reason and outcome.
	FinishApproval(ctx context.Context, id, from, status, reason, outcome string) (bool, error)
	SetApprovalMessage(ctx context.Context, id string, chatID, messageID int64) error
	// ExpireApprovals moves every pending record past its expiry, or every pending and confirmed
	// record when all is set, to expired or failed, returning the changed records.
	ExpireApprovals(ctx context.Context, now time.Time, all bool) ([]Record, error)
	// UndeliveredApprovals returns pending records whose chat is known but whose message never
	// reached Telegram.
	UndeliveredApprovals(context.Context) ([]Record, error)
}

// Deliverer sends and updates the proposal message in the user's chat.
type Deliverer interface {
	SendProposal(ctx context.Context, chatID int64, text, confirmData, rejectData string) (int64, error)
	EditText(ctx context.Context, chatID, messageID int64, text string) error
}

type Config struct {
	Lifetime      time.Duration
	SweepInterval time.Duration
}

type Service struct {
	repository    Repository
	deliverer     Deliverer
	logger        *slog.Logger
	lifetime      time.Duration
	sweepInterval time.Duration
	now           func() time.Time

	mu        sync.Mutex
	executors map[string]Executor
	notices   map[string]string
	cancel    context.CancelFunc
	done      chan struct{}
}

func NewService(cfg Config, repository Repository, deliverer Deliverer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("approval repository is required")
	}
	if deliverer == nil {
		return nil, fmt.Errorf("approval deliverer is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("approval logger is required")
	}
	lifetime := cfg.Lifetime
	if lifetime <= 0 {
		lifetime = DefaultLifetime
	}
	sweepInterval := cfg.SweepInterval
	if sweepInterval <= 0 {
		sweepInterval = DefaultSweepInterval
	}
	return &Service{
		repository:    repository,
		deliverer:     deliverer,
		logger:        logger,
		lifetime:      lifetime,
		sweepInterval: sweepInterval,
		now:           time.Now,
		executors:     make(map[string]Executor),
		notices:       make(map[string]string),
	}, nil
}

// SetLifetime changes how long new proposals stay confirmable.
func (s *Service) SetLifetime(lifetime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lifetime > 0 {
		s.lifetime = lifetime
	}
}

// RegisterExecutor binds a connector to the tools it can execute once confirmed.
func (s *Service) RegisterExecutor(executor Executor) error {
	if executor == nil {
		return fmt.Errorf("register approval executor: executor is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tool := range executor.ActionTools() {
		if _, exists := s.executors[tool]; exists {
			return fmt.Errorf("register approval executor: tool %q is already bound", tool)
		}
		s.executors[tool] = executor
	}
	return nil
}

// Start expires everything left unresolved by a previous process and begins the expiry sweep.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return fmt.Errorf("approval service is already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel, s.done = cancel, done
	s.mu.Unlock()

	expired, err := s.repository.ExpireApprovals(ctx, s.now(), true)
	if err != nil {
		cancel()
		s.mu.Lock()
		s.cancel, s.done = nil, nil
		s.mu.Unlock()
		return fmt.Errorf("expire approvals after restart: %w", err)
	}
	for _, record := range expired {
		s.report(ctx, record)
	}
	go s.sweep(runCtx, done)
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown approval service: %w", ctx.Err())
	}
}

func (s *Service) sweep(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		expired, err := s.repository.ExpireApprovals(ctx, s.now(), false)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.WarnContext(ctx, "approval.sweep.failed")
			}
			continue
		}
		for _, record := range expired {
			s.report(ctx, record)
		}
		s.resendUndelivered(ctx)
	}
}

// Propose records an exact action for the principal, replacing any pending proposal.
func (s *Service) Propose(ctx context.Context, principalID string, proposal Proposal) (Record, error) {
	if principalID == "" || proposal.Tool == "" || strings.TrimSpace(proposal.Summary) == "" {
		return Record{}, fmt.Errorf("approval proposal is incomplete")
	}
	s.mu.Lock()
	_, executable := s.executors[proposal.Tool]
	s.mu.Unlock()
	if !executable {
		return Record{}, fmt.Errorf("approval proposal names a tool without an executor")
	}
	canonical, err := canonicalJSON(proposal.Arguments)
	if err != nil {
		return Record{}, fmt.Errorf("canonicalize approval arguments: %w", err)
	}
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	digest := sha256.Sum256(canonical)
	now := s.now()
	s.mu.Lock()
	lifetime := s.lifetime
	s.mu.Unlock()
	record := Record{
		ID:          id,
		PrincipalID: principalID,
		Tool:        proposal.Tool,
		Arguments:   canonical,
		Hash:        hex.EncodeToString(digest[:]),
		Summary:     truncateRunes(strings.TrimSpace(proposal.Summary), maximumSummaryRunes),
		Status:      StatusPending,
		CreatedAt:   now,
		ExpiresAt:   now.Add(lifetime),
	}
	replaced, hadPrevious, err := s.repository.CreateApproval(ctx, record)
	if err != nil {
		return Record{}, fmt.Errorf("store approval: %w", err)
	}
	s.logger.InfoContext(ctx, "approval.proposed", "principal_id", principalID, "approval_id", id, "tool", proposal.Tool, "replaced", hadPrevious)
	if hadPrevious {
		s.report(ctx, replaced)
	}
	return record, nil
}

// AfterDelivery sends the principal's pending proposal to the chat once the turn's reply is out.
func (s *Service) AfterDelivery(ctx context.Context, principalID string, chatID int64) error {
	record, found, err := s.repository.PendingApproval(ctx, principalID)
	if err != nil {
		return err
	}
	if !found || record.MessageID != 0 {
		return nil
	}
	return s.send(ctx, record, chatID)
}

func (s *Service) send(ctx context.Context, record Record, chatID int64) error {
	messageID, err := s.deliverer.SendProposal(ctx, chatID, proposalText(record), callbackData(ActionConfirm, record.ID), callbackData(ActionReject, record.ID))
	if err != nil {
		// The chat is remembered so the sweep can retry until the proposal expires.
		_ = s.repository.SetApprovalMessage(ctx, record.ID, chatID, 0)
		s.logger.WarnContext(ctx, "approval.proposal.undeliverable", "principal_id", record.PrincipalID, "approval_id", record.ID, "error", err)
		return err
	}
	if err := s.repository.SetApprovalMessage(ctx, record.ID, chatID, messageID); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "approval.proposal.sent", "principal_id", record.PrincipalID, "approval_id", record.ID)
	return nil
}

func (s *Service) resendUndelivered(ctx context.Context) {
	undelivered, err := s.repository.UndeliveredApprovals(ctx)
	if err != nil {
		return
	}
	for _, record := range undelivered {
		_ = s.send(ctx, record, record.ChatID)
	}
}

// Press handles a button press by the pressing principal and returns the text the proposal
// message should show now. A press on another principal's proposal is ignored.
func (s *Service) Press(ctx context.Context, principalID, action, approvalID string) (string, bool, error) {
	record, found, err := s.repository.LoadApproval(ctx, approvalID)
	if err != nil {
		return "", false, err
	}
	if !found || record.PrincipalID != principalID {
		return "", false, nil
	}
	if record.Status != StatusPending {
		return finalText(record), true, nil
	}
	if !s.now().Before(record.ExpiresAt) {
		if _, err := s.repository.FinishApproval(ctx, record.ID, StatusPending, StatusExpired, ReasonTimeout, ""); err != nil {
			return "", false, err
		}
		record.Status, record.Reason = StatusExpired, ReasonTimeout
		s.logger.InfoContext(ctx, "approval.resolved", "principal_id", principalID, "approval_id", record.ID, "status", record.Status, "reason", record.Reason)
		return finalText(record), true, nil
	}
	switch action {
	case ActionReject:
		if _, err := s.repository.FinishApproval(ctx, record.ID, StatusPending, StatusRejected, "", ""); err != nil {
			return "", false, err
		}
		record.Status = StatusRejected
		s.logger.InfoContext(ctx, "approval.resolved", "principal_id", principalID, "approval_id", record.ID, "status", record.Status)
		return finalText(record), true, nil
	case ActionConfirm:
		confirmed, err := s.repository.ConfirmApproval(ctx, record.ID, principalID, s.now())
		if err != nil {
			return "", false, err
		}
		if !confirmed {
			current, _, err := s.repository.LoadApproval(ctx, approvalID)
			if err != nil {
				return "", false, err
			}
			return finalText(current), true, nil
		}
		record.Status = StatusConfirmed
		return s.execute(ctx, record), true, nil
	default:
		return "", false, nil
	}
}

func (s *Service) execute(ctx context.Context, record Record) string {
	s.mu.Lock()
	executor, bound := s.executors[record.Tool]
	s.mu.Unlock()
	status, reason, outcome := StatusFailed, ReasonNoExecutor, ""
	if bound {
		executionCtx, cancel := context.WithTimeout(ctx, executionTimeout)
		result, err := executor.ExecuteApproved(executionCtx, record)
		cancel()
		var actionErr *ActionError
		switch {
		case err == nil:
			status, reason, outcome = StatusExecuted, "", result.Text
		case errors.As(err, &actionErr):
			reason, outcome = actionErr.Reason, actionErr.Text
		case errors.Is(err, context.DeadlineExceeded):
			reason = ReasonTimeout
		case errors.Is(err, context.Canceled):
			reason = ReasonInterrupted
		default:
			reason = ReasonExecutorFailed
		}
	}
	if _, err := s.repository.FinishApproval(context.WithoutCancel(ctx), record.ID, StatusConfirmed, status, reason, outcome); err != nil {
		s.logger.ErrorContext(ctx, "approval.finish_failed", "principal_id", record.PrincipalID, "approval_id", record.ID, "status", status)
	}
	record.Status, record.Reason, record.Outcome = status, reason, outcome
	s.rememberNotice(record)
	s.logger.InfoContext(ctx, "approval.resolved", "principal_id", record.PrincipalID, "approval_id", record.ID, "status", status, "reason", reason)
	return finalText(record)
}

// report edits the proposal message of a record resolved without a press, when it has one.
func (s *Service) report(ctx context.Context, record Record) {
	s.rememberNotice(record)
	s.logger.InfoContext(ctx, "approval.resolved", "principal_id", record.PrincipalID, "approval_id", record.ID, "status", record.Status, "reason", record.Reason)
	if record.ChatID == 0 || record.MessageID == 0 {
		return
	}
	if err := s.deliverer.EditText(ctx, record.ChatID, record.MessageID, finalText(record)); err != nil {
		s.logger.WarnContext(ctx, "approval.report.failed", "principal_id", record.PrincipalID, "approval_id", record.ID, "error", err)
	}
}

// rememberNotice keeps what became of a resolved proposal until the principal's next turn, so the
// model answers from the outcome it never saw instead of assuming its proposal is still waiting.
// Only the latest resolution is kept, and a proposal the model itself replaced needs no notice.
func (s *Service) rememberNotice(record Record) {
	if record.Reason == ReasonReplaced {
		return
	}
	notice := noticeText(record)
	if notice == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices[record.PrincipalID] = notice
}

// PendingNotice takes the principal's unreported proposal resolution, if there is one.
func (s *Service) PendingNotice(principalID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	notice := s.notices[principalID]
	delete(s.notices, principalID)
	return notice
}

func noticeText(record Record) string {
	switch record.Status {
	case StatusExecuted:
		return strings.TrimSpace("The user confirmed your " + record.Tool + " proposal and it was executed. " + record.Outcome)
	case StatusRejected:
		return "The user rejected your " + record.Tool + " proposal; nothing was written."
	case StatusExpired:
		return "Your " + record.Tool + " proposal expired without an answer; nothing was written."
	case StatusFailed:
		if record.Outcome != "" {
			return "Your " + record.Tool + " proposal was confirmed but failed: " + record.Outcome
		}
		return "Your " + record.Tool + " proposal was confirmed but failed; check the target before proposing it again."
	default:
		return ""
	}
}

// ParseCallback splits proposal button data into its action and approval ID.
func ParseCallback(data string) (action, id string, ok bool) {
	rest, found := strings.CutPrefix(data, "approval:")
	if !found {
		return "", "", false
	}
	action, id, found = strings.Cut(rest, ":")
	if !found || (action != ActionConfirm && action != ActionReject) || len(id) != idBytes*2 {
		return "", "", false
	}
	return action, id, true
}

func callbackData(action, id string) string {
	return "approval:" + action + ":" + id
}

func proposalText(record Record) string {
	return record.Summary + "\n\nExpires in " + fmtMinutes(record.ExpiresAt.Sub(record.CreatedAt)) + "."
}

func finalText(record Record) string {
	switch record.Status {
	case StatusPending:
		return proposalText(record)
	case StatusConfirmed:
		return record.Summary + "\n\nConfirmed; executing."
	case StatusExecuted:
		return record.Summary + "\n\nDone. " + record.Outcome
	case StatusRejected:
		return record.Summary + "\n\nRejected; nothing was written."
	case StatusExpired:
		switch record.Reason {
		case ReasonReplaced:
			return record.Summary + "\n\nReplaced by a newer proposal; nothing was written."
		case ReasonRestart:
			return record.Summary + "\n\nExpired because the assistant restarted; nothing was written."
		default:
			return record.Summary + "\n\nExpired; nothing was written."
		}
	case StatusFailed:
		switch record.Reason {
		case ReasonInterrupted:
			return record.Summary + "\n\nThe assistant restarted while executing this; check Notion before trying again."
		case ReasonTimeout:
			return record.Summary + "\n\nExecution timed out; check the target before trying again."
		case ReasonNoExecutor, ReasonExecutorFailed:
			return record.Summary + "\n\nExecution failed before anything was written."
		default:
			if record.Outcome != "" {
				return record.Summary + "\n\nFailed. " + record.Outcome
			}
			return record.Summary + "\n\nFailed."
		}
	default:
		return record.Summary
	}
}

func fmtMinutes(duration time.Duration) string {
	minutes := int((duration + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		return "less than a minute"
	}
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// canonicalJSON re-encodes arguments with sorted object keys and no insignificant whitespace so
// equal actions hash equally and the stored form is exactly what will be executed.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	if err := writeCanonical(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeCanonical(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			if err := writeCanonical(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeCanonical(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	}
	return nil
}

func newID() (string, error) {
	var value [idBytes]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate approval identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
