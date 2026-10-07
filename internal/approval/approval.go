// Package approval is the core approval state machine for confirmed actions: connectors propose
// exact actions during a turn, the bot shows the turn's proposals to the same user as one bundle
// with Confirm and Reject buttons, and the core executes each confirmed proposal exactly once
// through its connector.
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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
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
	// ReasonWithdrawn marks a rejected record the model withdrew itself.
	ReasonWithdrawn = "withdrawn"

	ActionConfirm = "confirm"
	ActionReject  = "reject"
	ActionPreview = "preview"

	DefaultLifetime      = 10 * time.Minute
	DefaultSweepInterval = 30 * time.Second
	maximumSummaryRunes  = 3000
	intentCheckTimeout   = 3 * time.Second
	IntentWarning        = "⚠️ This does not look like something you asked for. Check it carefully before confirming."
	executionTimeout     = 45 * time.Second
	idBytes              = 8

	maximumBundleItems = 10
	// bundleSummaryUnits is the summary budget of one bundle message; it leaves room in Telegram's
	// 4096-unit message for the header, the expiry line, and a cut outcome line per item.
	bundleSummaryUnits = 2500
	bundleOutcomeRunes = 120

	ProposedText = "Proposed and awaiting the user's confirmation; nothing has been written yet. After your reply the user sees this turn's proposals together with one Confirm and one Reject button. Do not claim it was done."
)

// ErrBundleFull is returned for a proposal that no longer fits the bundle waiting for confirmation.
var ErrBundleFull = errors.New("the batch of changes waiting for the user's confirmation is full; nothing was added. Tell the user to confirm or reject it first, then propose the rest")

// ErrSummaryTooLong is returned for a proposal whose rendering cannot fit one message. It is
// refused instead of cut, because the confirmation must show the exact action.
var ErrSummaryTooLong = errors.New("the proposal is too long to show the user in full for confirmation; nothing was added. Propose a shorter one")

// Refused reports whether the error is a proposal refusal whose text tells the model what to do next.
func Refused(err error) bool {
	return errors.Is(err, ErrBundleFull) || errors.Is(err, ErrSummaryTooLong)
}

// Proposal is what a connector asks the user to confirm: the exact tool call and its rendering.
type Proposal struct {
	Tool      string
	Arguments json.RawMessage
	Summary   string
	Intent    Intent
}

// Intent is a short description of a proposed action for the intent check, which sends it to an
// external service after redaction. It must never carry bodies, IDs, URLs or paths: only a verb,
// the kind of target, and its human-readable name.
type Intent struct {
	Verb       string
	TargetKind string
	TargetName string
}

// IntentChecker judges whether a proposed action looks like something the user asked for.
type IntentChecker interface {
	// Mismatch reports true when the action does not look requested.
	Mismatch(ctx context.Context, principalID, tool string, intent Intent) (bool, error)
}

// Record is the durable approval. Arguments hold the canonical form whose hash is stored. Records
// proposed in one turn share a bundle, named by its first record's ID and ordered by position.
type Record struct {
	ID          string
	BundleID    string
	Position    int
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
	StartedAt   time.Time
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

// Previewer is implemented by an executor whose proposals can be tried once before the user
// confirms them, without any of their effects.
type Previewer interface {
	// Previewable reports whether the pending record has something to preview.
	Previewable(context.Context, Record) bool
	// Preview runs the record's proposed action once without saving anything. An error means the
	// preview was interrupted and nothing should be shown.
	Preview(context.Context, Record) (Preview, error)
}

// Preview is what one previewed proposal produced, under the name the user knows it by.
type Preview struct {
	Name string
	Text string
}

type Repository interface {
	// CreateApproval stores a pending record. A record whose bundle is its own starts a new bundle
	// after expiring every pending record of the principal with ReasonReplaced, and those are
	// returned; any other record joins its bundle.
	CreateApproval(context.Context, Record) ([]Record, error)
	LoadApproval(context.Context, string) (Record, bool, error)
	// LoadBundle returns a bundle's records in position order.
	LoadBundle(ctx context.Context, bundleID string) ([]Record, error)
	// PendingApprovals returns the principal's pending records in position order.
	PendingApprovals(context.Context, string) ([]Record, error)
	// ConfirmBundle moves the pending, unexpired records of the principal's bundle to confirmed,
	// reporting whether any moved.
	ConfirmBundle(ctx context.Context, bundleID, principalID string, now time.Time) (bool, error)
	// StartApproval marks a confirmed record as executing, so a restart can tell it from one that
	// never ran.
	StartApproval(ctx context.Context, id string, now time.Time) error
	// FinishApproval moves a record to a terminal status with a reason and outcome.
	FinishApproval(ctx context.Context, id, from, status, reason, outcome string) (bool, error)
	// SetApprovalMessage records the chat and the message that shows the records; a zero message ID
	// marks them as not delivered yet.
	SetApprovalMessage(ctx context.Context, ids []string, chatID, messageID int64) error
	// SetApprovalSummary replaces the summary of a pending record.
	SetApprovalSummary(ctx context.Context, id, summary string) error
	// ExpireApprovals moves every pending record past its expiry, or every pending and confirmed
	// record when all is set, to expired, or to failed for a confirmed record that had started,
	// returning the changed records.
	ExpireApprovals(ctx context.Context, now time.Time, all bool) ([]Record, error)
	// UndeliveredApprovals returns pending records whose chat is known but whose message never
	// reached Telegram.
	UndeliveredApprovals(context.Context) ([]Record, error)
}

// Deliverer sends and updates the proposal messages in the user's chat.
type Deliverer interface {
	// SendText sends a message without buttons, for the parts of a bundle before the last.
	SendText(ctx context.Context, chatID int64, text string) (int64, error)
	SendProposal(ctx context.Context, chatID int64, text string, buttons Buttons) (int64, error)
	EditText(ctx context.Context, chatID, messageID int64, text string) error
	// EditProposal replaces the text of the message with the buttons, keeping the given buttons.
	EditProposal(ctx context.Context, chatID, messageID int64, text string, buttons Buttons) error
	// SendPreview posts a preview's output as a reply to the proposal message.
	SendPreview(ctx context.Context, chatID, replyTo int64, text string) error
}

// Buttons is the callback data of a bundle's buttons; an empty Preview leaves that button out.
type Buttons struct {
	Confirm string
	Reject  string
	Preview string
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

	mu sync.Mutex
	// proposing serializes each principal's proposals, so parallel tool calls in one turn join the
	// same bundle. Bundles never span principals, so one principal's slow send holds no one else up.
	proposing map[string]*sync.Mutex
	executors map[string]Executor
	checker   IntentChecker
	telemetry Telemetry
	intents   map[string]Intent
	notices   map[string]string
	cancel    context.CancelFunc
	done      chan struct{}
	// runCtx is the context previews run in while the service is started.
	runCtx context.Context
	// previewing holds the bundles with a preview running, and pressed the answer given to each
	// Preview press, so a press Telegram delivers again starts nothing new.
	previewing map[string]bool
	pressed    map[string]previewAnswer
	previews   sync.WaitGroup
}

type previewAnswer struct {
	text string
	at   time.Time
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
		intents:       make(map[string]Intent),
		proposing:     make(map[string]*sync.Mutex),
		previewing:    make(map[string]bool),
		pressed:       make(map[string]previewAnswer),
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

// Telemetry records the approval lifecycle.
type Telemetry interface {
	RecordApproval(ctx context.Context, principal, tool, status string, warned bool)
}

// SetTelemetry sets where the approval lifecycle is recorded, at wiring time before proposals.
func (s *Service) SetTelemetry(telemetry Telemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.telemetry = telemetry
}

// recordStatus counts one lifecycle step. The warning is read from the stored summary so it
// survives a restart.
func (s *Service) recordStatus(ctx context.Context, record Record, status string) {
	s.mu.Lock()
	telemetry := s.telemetry
	s.mu.Unlock()
	if telemetry == nil {
		return
	}
	telemetry.RecordApproval(ctx, record.PrincipalID, record.Tool, status, strings.HasPrefix(record.Summary, IntentWarning))
}

// SetIntentChecker enables the intent check on proposals delivered from now on.
func (s *Service) SetIntentChecker(checker IntentChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checker = checker
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
	s.cancel, s.done, s.runCtx = cancel, done, runCtx
	s.mu.Unlock()

	expired, err := s.repository.ExpireApprovals(ctx, s.now(), true)
	if err != nil {
		cancel()
		s.mu.Lock()
		s.cancel, s.done, s.runCtx = nil, nil, nil
		s.mu.Unlock()
		return fmt.Errorf("expire approvals after restart: %w", err)
	}
	s.report(ctx, expired)
	go s.sweep(runCtx, done)
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done, s.runCtx = nil, nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	finished := make(chan struct{})
	go func() {
		<-done
		s.previews.Wait()
		close(finished)
	}()
	select {
	case <-finished:
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
		s.report(ctx, expired)
		s.resendUndelivered(ctx)
	}
}

// Propose records an exact action for the principal. It joins the bundle of this turn's
// undelivered proposals, or starts a new bundle that replaces any pending one.
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
	proposing := s.proposalLock(principalID)
	proposing.Lock()
	record, replaced, err := s.store(ctx, principalID, proposal.Tool, canonical, hex.EncodeToString(digest[:]), proposal.Summary, id)
	proposing.Unlock()
	if err != nil || record.ID != id {
		return record, err
	}
	if proposal.Intent != (Intent{}) {
		s.mu.Lock()
		s.intents[id] = proposal.Intent
		s.mu.Unlock()
	}
	s.logger.InfoContext(ctx, "approval.proposed", "principal_id", principalID, "approval_id", id, "bundle_id", record.BundleID, "tool", proposal.Tool, "replaced", len(replaced) > 0)
	s.recordStatus(ctx, Record{PrincipalID: principalID, Tool: proposal.Tool}, "proposed")
	s.report(ctx, replaced)
	return record, nil
}

func (s *Service) proposalLock(principalID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, found := s.proposing[principalID]
	if !found {
		lock = &sync.Mutex{}
		s.proposing[principalID] = lock
	}
	return lock
}

// store joins the record to the turn's bundle or starts a new one, returning the records it
// replaced. An identical proposal already in the bundle is returned instead of a new record.
func (s *Service) store(ctx context.Context, principalID, tool string, canonical json.RawMessage, hash, summary, id string) (Record, []Record, error) {
	if utf16Units(strings.TrimSpace(summary))+utf16Units(IntentWarning)+2 > bundleSummaryUnits {
		return Record{}, nil, ErrSummaryTooLong
	}
	pending, err := s.repository.PendingApprovals(ctx, principalID)
	if err != nil {
		return Record{}, nil, fmt.Errorf("load pending approvals: %w", err)
	}
	now := s.now()
	s.mu.Lock()
	lifetime := s.lifetime
	s.mu.Unlock()
	record := Record{
		ID:          id,
		BundleID:    id,
		PrincipalID: principalID,
		Tool:        tool,
		Arguments:   canonical,
		Hash:        hash,
		Summary:     strings.TrimSpace(summary),
		Status:      StatusPending,
		CreatedAt:   now,
		ExpiresAt:   now.Add(lifetime),
	}
	if joinable(pending, now, lifetime) {
		for _, item := range pending {
			if item.Tool == record.Tool && item.Hash == record.Hash {
				return item, nil, nil
			}
		}
		if len(pending) >= maximumBundleItems {
			return Record{}, nil, ErrBundleFull
		}
		// The position follows every record of the bundle, withdrawn ones included, which pending
		// leaves out.
		first := pending[0]
		bundle, err := s.repository.LoadBundle(ctx, first.BundleID)
		if err != nil {
			return Record{}, nil, fmt.Errorf("load approval bundle: %w", err)
		}
		record.BundleID, record.Position = first.BundleID, bundle[len(bundle)-1].Position+1
		record.CreatedAt, record.ExpiresAt = first.CreatedAt, first.ExpiresAt
	}
	replaced, err := s.repository.CreateApproval(ctx, record)
	if err != nil {
		return Record{}, nil, fmt.Errorf("store approval: %w", err)
	}
	return record, replaced, nil
}

// joinable reports whether the pending records are one bundle that this turn is still building:
// not yet sent, and young enough that a turn whose reply never went out does not pass its
// proposals, or a deadline about to lapse, to a later turn.
func joinable(pending []Record, now time.Time, lifetime time.Duration) bool {
	if len(pending) == 0 {
		return false
	}
	first := pending[0]
	for _, item := range pending {
		if item.BundleID != first.BundleID || item.ChatID != 0 {
			return false
		}
	}
	return now.Sub(first.CreatedAt) < lifetime/2 && now.Before(first.ExpiresAt)
}

// AfterDelivery sends the principal's undelivered bundle to the chat once the turn's reply is out.
func (s *Service) AfterDelivery(ctx context.Context, principalID string, chatID int64) error {
	bundle, err := s.repository.PendingApprovals(ctx, principalID)
	if err != nil {
		return err
	}
	if len(bundle) == 0 || bundle[len(bundle)-1].MessageID != 0 {
		return nil
	}
	s.checkIntents(ctx, bundle)
	// Holding proposals off until the message is recorded keeps a late proposal from joining a
	// bundle the user has already been shown without it.
	proposing := s.proposalLock(principalID)
	proposing.Lock()
	defer proposing.Unlock()
	current, err := s.repository.PendingApprovals(ctx, principalID)
	if err != nil {
		return err
	}
	if len(current) == 0 || current[0].BundleID != bundle[0].BundleID || current[len(current)-1].MessageID != 0 {
		return nil
	}
	return s.send(ctx, current, chatID)
}

// checkIntents runs the intent check on every item of the bundle at once.
func (s *Service) checkIntents(ctx context.Context, bundle []Record) {
	var group sync.WaitGroup
	for _, record := range bundle {
		group.Add(1)
		go func() {
			defer group.Done()
			s.checkIntent(ctx, record)
		}()
	}
	group.Wait()
}

// checkIntent prepends a warning to a proposal that does not look requested. The intent is used
// once, so a proposal resent after a failed delivery keeps the summary already stored.
func (s *Service) checkIntent(ctx context.Context, record Record) {
	s.mu.Lock()
	checker := s.checker
	intent, hasIntent := s.intents[record.ID]
	delete(s.intents, record.ID)
	s.mu.Unlock()
	if checker == nil || !hasIntent {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, intentCheckTimeout)
	defer cancel()
	mismatch, err := checker.Mismatch(checkCtx, record.PrincipalID, record.Tool, intent)
	if err != nil {
		s.logger.WarnContext(ctx, "approval.intent_check.failed", "principal_id", record.PrincipalID, "approval_id", record.ID)
		return
	}
	if !mismatch {
		return
	}
	summary := truncateRunes(IntentWarning+"\n\n"+record.Summary, maximumSummaryRunes)
	if err := s.repository.SetApprovalSummary(ctx, record.ID, summary); err != nil {
		s.logger.WarnContext(ctx, "approval.intent_warning.unsaved", "principal_id", record.PrincipalID, "approval_id", record.ID)
	}
}

func (s *Service) dropIntents(bundle []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range bundle {
		delete(s.intents, record.ID)
	}
}

// send delivers the records of the bundle that no message shows yet, in as many messages as their
// summaries need. Only the last message has the buttons, and each record is in exactly one message.
func (s *Service) send(ctx context.Context, bundle []Record, chatID int64) error {
	first := bundle[0]
	start := slices.IndexFunc(bundle, func(record Record) bool { return record.MessageID == 0 })
	if start < 0 {
		return nil
	}
	ranges := messageRanges(bundle, start)
	for index, span := range ranges {
		text := messageText(bundle, span[0], span[1])
		var messageID int64
		var err error
		if index == len(ranges)-1 {
			messageID, err = s.deliverer.SendProposal(ctx, chatID, text, s.buttons(ctx, bundle))
		} else {
			messageID, err = s.deliverer.SendText(ctx, chatID, text)
		}
		if err != nil {
			// The chat is remembered so the sweep can retry the rest until the proposal expires.
			_ = s.repository.SetApprovalMessage(ctx, recordIDs(bundle[span[0]:]), chatID, 0)
			s.logger.WarnContext(ctx, "approval.proposal.undeliverable", "principal_id", first.PrincipalID, "bundle_id", first.BundleID, "error", err)
			return err
		}
		if err := s.repository.SetApprovalMessage(ctx, recordIDs(bundle[span[0]:span[1]]), chatID, messageID); err != nil {
			return err
		}
	}
	s.logger.InfoContext(ctx, "approval.proposal.sent", "principal_id", first.PrincipalID, "bundle_id", first.BundleID, "items", len(bundle), "messages", len(ranges))
	return nil
}

func recordIDs(records []Record) []string {
	ids := make([]string, len(records))
	for index, record := range records {
		ids[index] = record.ID
	}
	return ids
}

func (s *Service) resendUndelivered(ctx context.Context) {
	undelivered, err := s.repository.UndeliveredApprovals(ctx)
	if err != nil {
		return
	}
	for _, group := range groupBundles(undelivered) {
		bundle, err := s.repository.LoadBundle(ctx, group[0].BundleID)
		bundle = shown(bundle)
		if err != nil || len(bundle) == 0 {
			continue
		}
		_ = s.send(ctx, bundle, group[0].ChatID)
	}
}

// Pressed is what a button press did.
type Pressed struct {
	// Text is what the proposal message should show now.
	Text string
	// Owned is false for a press on another principal's proposal, which is ignored.
	Owned bool
	// Ran reports that this press confirmed the bundle and ran its actions, so the model has an
	// outcome to continue from.
	Ran bool
}

// Press handles a Confirm or Reject press by the pressing principal.
func (s *Service) Press(ctx context.Context, principalID, action, approvalID string) (Pressed, error) {
	record, found, err := s.repository.LoadApproval(ctx, approvalID)
	if err != nil {
		return Pressed{}, err
	}
	if !found || record.PrincipalID != principalID {
		return Pressed{}, nil
	}
	bundle, err := s.repository.LoadBundle(ctx, record.BundleID)
	if err != nil {
		return Pressed{}, err
	}
	s.dropIntents(bundle)
	bundle = shown(bundle)
	if len(bundle) == 0 {
		return Pressed{Owned: true}, nil
	}
	pending := slices.IndexFunc(bundle, func(item Record) bool { return item.Status == StatusPending })
	if pending < 0 {
		return Pressed{Text: bundleText(bundle), Owned: true}, nil
	}
	if !s.now().Before(bundle[pending].ExpiresAt) {
		if err := s.finishPending(ctx, bundle, StatusExpired, ReasonTimeout); err != nil {
			return Pressed{}, err
		}
		return Pressed{Text: bundleText(bundle), Owned: true}, nil
	}
	switch action {
	case ActionReject:
		if err := s.finishPending(ctx, bundle, StatusRejected, ""); err != nil {
			return Pressed{}, err
		}
		return Pressed{Text: bundleText(bundle), Owned: true}, nil
	case ActionConfirm:
		confirmed, err := s.repository.ConfirmBundle(ctx, record.BundleID, principalID, s.now())
		if err != nil {
			return Pressed{}, err
		}
		if !confirmed {
			current, err := s.repository.LoadBundle(ctx, record.BundleID)
			if err != nil {
				return Pressed{}, err
			}
			return Pressed{Text: bundleText(shown(current)), Owned: true}, nil
		}
		for index := range bundle {
			if bundle[index].Status == StatusPending {
				bundle[index].Status = StatusConfirmed
				bundle[index] = s.execute(ctx, bundle[index])
			}
		}
		s.rememberNotice(bundle)
		return Pressed{Text: bundleText(bundle), Owned: true, Ran: true}, nil
	default:
		return Pressed{}, nil
	}
}

// finishPending moves the bundle's pending records to a terminal status, in place.
func (s *Service) finishPending(ctx context.Context, bundle []Record, status, reason string) error {
	for index := range bundle {
		record := &bundle[index]
		if record.Status != StatusPending {
			continue
		}
		finished, err := s.repository.FinishApproval(ctx, record.ID, StatusPending, status, reason, "")
		if err != nil {
			return err
		}
		record.Status, record.Reason = status, reason
		if finished {
			s.recordStatus(ctx, *record, record.Status)
		}
		s.logger.InfoContext(ctx, "approval.resolved", "principal_id", record.PrincipalID, "approval_id", record.ID, "bundle_id", record.BundleID, "status", record.Status, "reason", record.Reason)
	}
	return nil
}

func (s *Service) execute(ctx context.Context, record Record) Record {
	s.mu.Lock()
	executor, bound := s.executors[record.Tool]
	s.mu.Unlock()
	status, reason, outcome := StatusFailed, ReasonNoExecutor, ""
	if bound {
		if err := s.repository.StartApproval(ctx, record.ID, s.now()); err != nil {
			// Without the mark a restart would report the action as never run, so it is not run.
			s.logger.ErrorContext(ctx, "approval.start_failed", "principal_id", record.PrincipalID, "approval_id", record.ID)
			bound, reason = false, ReasonExecutorFailed
		}
	}
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
	s.recordStatus(ctx, record, status)
	s.logger.InfoContext(ctx, "approval.resolved", "principal_id", record.PrincipalID, "approval_id", record.ID, "bundle_id", record.BundleID, "status", status, "reason", reason)
	return record
}

// report records records resolved without a press and edits each of their bundles' messages.
func (s *Service) report(ctx context.Context, records []Record) {
	s.dropIntents(records)
	for _, record := range records {
		status := record.Status
		if record.Reason == ReasonReplaced {
			status = "replaced"
		}
		s.recordStatus(ctx, record, status)
		s.logger.InfoContext(ctx, "approval.resolved", "principal_id", record.PrincipalID, "approval_id", record.ID, "bundle_id", record.BundleID, "status", record.Status, "reason", record.Reason)
	}
	for _, changed := range groupBundles(records) {
		bundle, err := s.repository.LoadBundle(ctx, changed[0].BundleID)
		bundle = shown(bundle)
		if err != nil || len(bundle) == 0 {
			bundle = changed
		}
		s.rememberNotice(bundle)
		last := bundle[len(bundle)-1]
		if last.ChatID == 0 || last.MessageID == 0 {
			continue
		}
		if err := s.deliverer.EditText(ctx, last.ChatID, last.MessageID, bundleText(bundle)); err != nil {
			s.logger.WarnContext(ctx, "approval.report.failed", "principal_id", last.PrincipalID, "bundle_id", last.BundleID, "error", err)
		}
	}
}

// groupBundles splits records into bundles, keeping their order.
func groupBundles(records []Record) [][]Record {
	var bundles [][]Record
	index := make(map[string]int)
	for _, record := range records {
		position, seen := index[record.BundleID]
		if !seen {
			position = len(bundles)
			index[record.BundleID] = position
			bundles = append(bundles, nil)
		}
		bundles[position] = append(bundles[position], record)
	}
	return bundles
}

// rememberNotice keeps what became of a resolved bundle until the principal's next turn, so the
// model answers from the outcome it never saw instead of assuming its proposals are still waiting.
// Only the latest resolution is kept, and proposals the model itself replaced need no notice.
func (s *Service) rememberNotice(bundle []Record) {
	var notices []string
	for _, record := range bundle {
		if record.Reason == ReasonReplaced || record.Reason == ReasonWithdrawn {
			continue
		}
		if notice := noticeText(record); notice != "" {
			notices = append(notices, notice)
		}
	}
	if len(notices) == 0 {
		return
	}
	notice := notices[0]
	if len(notices) > 1 {
		notice = fmt.Sprintf("Your %d proposals were shown and resolved together. ", len(notices))
		for index, text := range notices {
			notice += fmt.Sprintf("(%d) %s ", index+1, text)
		}
		notice = strings.TrimSpace(notice)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices[bundle[0].PrincipalID] = notice
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
	if !found || (action != ActionConfirm && action != ActionReject && action != ActionPreview) || len(id) != idBytes*2 {
		return "", "", false
	}
	return action, id, true
}

func callbackData(action, id string) string {
	return "approval:" + action + ":" + id
}

// messageRanges splits the records from start on into the messages that carry them: runs of
// records whose summaries fit the per-message budget, each record whole in one message.
func messageRanges(bundle []Record, start int) [][2]int {
	var ranges [][2]int
	from, units := start, 0
	for index := start; index < len(bundle); index++ {
		cost := utf16Units(bundle[index].Summary) + 2
		if index > from && units+cost > bundleSummaryUnits {
			ranges = append(ranges, [2]int{from, index})
			from, units = index, 0
		}
		units += cost
	}
	return append(ranges, [2]int{from, len(bundle)})
}

// shown leaves out the records withdrawn before any message showed them, so a bundle is numbered
// and rendered as the user saw it, or will see it.
func shown(bundle []Record) []Record {
	visible := make([]Record, 0, len(bundle))
	for _, record := range bundle {
		if record.Reason != ReasonWithdrawn || record.MessageID != 0 {
			visible = append(visible, record)
		}
	}
	return visible
}

// bundleText renders the message that carries the bundle's buttons, which is the last one.
func bundleText(bundle []Record) string {
	last := bundle[len(bundle)-1]
	from := len(bundle) - 1
	for last.MessageID != 0 && from > 0 && bundle[from-1].MessageID == last.MessageID {
		from--
	}
	return messageText(bundle, from, len(bundle))
}

// messageText renders the records from to to of a bundle: a single proposal as itself, and more as
// a numbered list with each item's result under it, or one shared result at the end of the last
// message when they all ended the same way.
func messageText(bundle []Record, from, to int) string {
	if len(bundle) == 1 {
		return finalText(bundle[0])
	}
	shared := statusLine(bundle[0], bundleOutcomeRunes)
	for _, record := range bundle[1:] {
		if statusLine(record, bundleOutcomeRunes) != shared {
			shared = ""
			break
		}
	}
	var builder strings.Builder
	switch {
	case from == 0:
		fmt.Fprintf(&builder, "%d changes, confirmed or rejected together:", len(bundle))
	case to-from == 1:
		fmt.Fprintf(&builder, "Change %d of %d, confirmed or rejected together:", to, len(bundle))
	default:
		fmt.Fprintf(&builder, "Changes %d-%d of %d, confirmed or rejected together:", from+1, to, len(bundle))
	}
	for index := from; index < to; index++ {
		fmt.Fprintf(&builder, "\n\n%d. %s", index+1, bundle[index].Summary)
		if shared == "" {
			builder.WriteString("\n" + statusLine(bundle[index], bundleOutcomeRunes))
		}
	}
	if shared != "" && to == len(bundle) {
		builder.WriteString("\n\n" + shared)
	}
	return builder.String()
}

func finalText(record Record) string {
	line := statusLine(record, maximumSummaryRunes)
	if line == "" {
		return record.Summary
	}
	return record.Summary + "\n\n" + line
}

// statusLine says where a record stands, with its outcome cut to limit runes.
func statusLine(record Record, limit int) string {
	outcome := truncateRunes(record.Outcome, limit)
	switch record.Status {
	case StatusPending:
		return "Expires in " + fmtMinutes(record.ExpiresAt.Sub(record.CreatedAt)) + "."
	case StatusConfirmed:
		return "Confirmed; executing."
	case StatusExecuted:
		return "Done. " + outcome
	case StatusRejected:
		if record.Reason == ReasonWithdrawn {
			return "Withdrawn by the assistant; nothing was written."
		}
		return "Rejected; nothing was written."
	case StatusExpired:
		switch record.Reason {
		case ReasonReplaced:
			return "Replaced by a newer proposal; nothing was written."
		case ReasonRestart:
			return "Expired because the assistant restarted; nothing was written."
		default:
			return "Expired; nothing was written."
		}
	case StatusFailed:
		switch record.Reason {
		case ReasonInterrupted:
			return "The assistant restarted while executing this; check whether it took effect before trying again."
		case ReasonTimeout:
			return "Execution timed out; check the target before trying again."
		case ReasonNoExecutor, ReasonExecutorFailed:
			return "Execution failed before anything was written."
		default:
			if outcome != "" {
				return "Failed. " + outcome
			}
			return "Failed."
		}
	default:
		return ""
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

func utf16Units(text string) int {
	units := 0
	for _, r := range text {
		units += utf16.RuneLen(r)
	}
	return units
}
