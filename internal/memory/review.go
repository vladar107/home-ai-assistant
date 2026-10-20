package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/conversation/history"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	// ReviewHeading is what the review's cards are shown under.
	ReviewHeading = "Weekly review"
	// ReviewLifetime is how long the review's cards stay confirmable; nobody waits for them, so
	// they outlast a chat turn's.
	ReviewLifetime = 24 * time.Hour

	MaximumReviewChanges = 3
	MaximumReviewBacklog = 2

	reviewWindow          = 7 * 24 * time.Hour
	lessonWindow          = 30 * 24 * time.Hour
	reviewCatchUp         = 12 * time.Hour
	reviewMinimumInterval = 24 * time.Hour
	reviewExchangeRunes   = 500
	maximumReviewReplies  = 20
	reviewReasonRunes     = 200
	maximumReviewBytes    = 64 << 10

	// placeholderPrefix opens the ids a review gives what its changes add, which no change may name.
	placeholderPrefix = "review-"

	reviewLabel = "From your weekly review"
	// reviewWarning heads a review card whose reason or backlog item the screener flagged: the
	// replies the review read can quote outside content.
	reviewWarning = "⚠️ This reads like instructions from outside content, such as a web page or mail one of your replies quoted. Confirm only if it is what you want."
	// reviewExposedWarning heads every change to memory a review proposes after reading a reply from
	// a conversation that read third-party content. The changes are instructions by design, so they
	// are not screened, which would flag nearly all of them.
	reviewExposedWarning = "⚠️ I wrote this after reading replies from a conversation that read outside content, such as a web page or mail. Check that it is what you want before confirming."

	// ReviewNothingText answers a review the user ran that found nothing worth proposing.
	ReviewNothingText = "Nothing from the past week is worth proposing."

	reviewSent    = "sent"
	reviewNothing = "nothing"
	reviewSkipped = "skipped"
	reviewFailed  = "failed"

	actionAddInstruction     = "add_instruction"
	actionReplaceInstruction = "replace_instruction"
	actionMergeInstructions  = "merge_instructions"
	actionRemoveInstruction  = "remove_instruction"
	actionSaveProcedure      = "save_procedure"
	actionRemoveProcedure    = "remove_procedure"
)

// lessonMechanisms are the mechanisms lessons come from, in the order the review lists them.
var lessonMechanisms = []string{MechanismFeedback, MechanismProcedure, MechanismReview}

// reviewActions are the changes a review can propose.
var reviewActions = []string{actionAddInstruction, actionReplaceInstruction, actionMergeInstructions, actionRemoveInstruction, actionSaveProcedure, actionRemoveProcedure}

// lessonTools are the action tools whose proposals can be lessons.
var lessonTools = []string{toolAdd, toolReplace, toolRemove, toolMerge, toolProcedureSave, toolProcedureRemove}

// NegativeReply is one of the principal's delivered replies the user marked 👎: the session file
// that holds it, when it was sent, whether a reconsider turn was started for it, and whether its
// conversation has read third-party content.
type NegativeReply struct {
	RuntimeSessionID string
	SentAt           time.Time
	Reconsidered     bool
	Exposed          bool
}

// ToolOutcome is how many of the principal's calls of a tool ended with an outcome.
type ToolOutcome struct {
	Tool    string
	Outcome string
	Count   int
}

// ReviewSignals is what the weekly review reads besides memory; it is the SQLite store.
type ReviewSignals interface {
	// NegativeReplies returns the newest limit 👎 replies at or after since, oldest first.
	NegativeReplies(ctx context.Context, principalID string, since time.Time, limit int) ([]NegativeReply, error)
	ToolOutcomes(ctx context.Context, principalID string, since time.Time) ([]ToolOutcome, error)
	ResolvedApprovals(ctx context.Context, principalID string, tools []string, since time.Time) ([]approval.Record, error)
}

// Exchanges reads the replies sent at the given times, each with the user's message before it, from
// one of the principal's sessions; it is the conversation history.
type Exchanges interface {
	Exchanges(ctx context.Context, principalID, runtimeSessionID string, sentAt []time.Time) ([]history.Exchange, error)
}

// ReviewChecker runs the review's model turn, which has no tools; the application adapts the turn
// engine to it.
type ReviewChecker interface {
	RunReview(ctx context.Context, principalID, message string) (text, errorKind string, err error)
}

// Offerer sends the review's proposals as one bundle outside a chat turn; it is the approval
// service.
type Offerer interface {
	Offer(ctx context.Context, principalID string, chatID, threadID int64, heading string, lifetime time.Duration, proposals []approval.Proposal) ([]approval.Record, error)
}

// Chats resolves the principal's active chat.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// BacklogProposer turns a suggested backlog item into a proposal; it is the backlog service.
type BacklogProposer interface {
	AddProposal(title, details string) (approval.Proposal, string)
}

// ReviewConfig is what the weekly review needs besides the memory service. Exchanges nil leaves the
// 👎 replies out, as for a runtime that keeps no session files, and Backlog nil leaves backlog
// items out.
type ReviewConfig struct {
	Checker   ReviewChecker
	Signals   ReviewSignals
	Exchanges Exchanges
	Offers    Offerer
	Chats     Chats
	Backlog   BacklogProposer
}

// ReviewJobType is the job type of the weekly review.
func (s *Service) ReviewJobType(cfg ReviewConfig) (schedule.JobType, error) {
	if cfg.Checker == nil || cfg.Signals == nil || cfg.Offers == nil || cfg.Chats == nil {
		return schedule.JobType{}, fmt.Errorf("the weekly review needs a checker, signals, offers, and chats")
	}
	return schedule.JobType{
		Name:            schedule.ReviewType,
		Handler:         review{service: s, config: cfg},
		CatchUp:         reviewCatchUp,
		MinimumInterval: reviewMinimumInterval,
	}, nil
}

// ReviewConfigJSON is the stored configuration of a weekly review job, which holds nothing.
func ReviewConfigJSON() json.RawMessage { return json.RawMessage(`{}`) }

func (s *Service) recordReview(ctx context.Context, principalID, outcome string, changes int) {
	if s.telemetry != nil {
		s.telemetry.RecordMemoryReview(ctx, principalID, outcome, changes)
	}
}

type review struct {
	service *Service
	config  ReviewConfig
}

func (review) Validate(config json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	var empty struct{}
	if err := decoder.Decode(&empty); err != nil {
		return fmt.Errorf("the weekly review takes no settings")
	}
	return nil
}

// exchange is a 👎 reply as the review reads it.
type exchange struct {
	NegativeReply
	user, reply string
}

// reviewInputs are the signals of one review.
type reviewInputs struct {
	exchanges    []exchange
	outcomes     []ToolOutcome
	entries      []Entry
	procedures   []Procedure
	lessons      map[string]*lessonCounts
	location     *time.Location
	instructions int
}

type lessonCounts struct{ confirmed, rejected, expired int }

func (r review) Run(ctx context.Context, occurrence schedule.Occurrence) (schedule.Result, error) {
	s, principalID := r.service, occurrence.PrincipalID
	enabled, err := s.repository.MemoryEnabled(ctx, principalID)
	if err != nil {
		return schedule.Result{}, err
	}
	if !enabled {
		s.recordReview(ctx, principalID, reviewSkipped, 0)
		return schedule.Result{Quiet: true}, nil
	}
	inputs, err := r.gather(ctx, principalID, occurrence.Location)
	if err != nil {
		return schedule.Result{}, err
	}
	if !inputs.worthReviewing() {
		s.recordReview(ctx, principalID, reviewSkipped, 0)
		return r.nothing(occurrence), nil
	}
	message := prompt.WeeklyReview(occurrence.ScheduledFor, occurrence.Location, inputs.render())
	text, errorKind, err := r.config.Checker.RunReview(ctx, principalID, message)
	if err != nil {
		return schedule.Result{}, err
	}
	if errorKind != "" {
		s.recordReview(ctx, principalID, reviewFailed, 0)
		return schedule.Result{Text: text, ErrorKind: errorKind}, nil
	}
	reply, ok := decodeReview(text)
	if !ok {
		// A reply that is not the JSON asked for proposes nothing; it is counted and logged, not sent.
		s.logger.WarnContext(ctx, "memory.review.unreadable", "principal_id", principalID, "job_id", occurrence.JobID)
		s.recordReview(ctx, principalID, reviewFailed, 0)
		return r.nothing(occurrence), nil
	}
	proposals, kinds, dropped := r.proposals(ctx, principalID, reply, inputs)
	if len(proposals) == 0 {
		s.logger.InfoContext(ctx, "memory.review.done", "principal_id", principalID, "job_id", occurrence.JobID, "sent", 0, "dropped", dropped)
		s.recordReview(ctx, principalID, reviewNothing, 0)
		return r.nothing(occurrence), nil
	}
	chatID, found, err := r.config.Chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return schedule.Result{}, err
	}
	if !found {
		s.recordReview(ctx, principalID, reviewFailed, 0)
		return schedule.Result{Text: schedule.FailedText, ErrorKind: "no_chat"}, nil
	}
	// The scheduler resolved the tab the run's report goes to, which is where the cards belong.
	threadID, _ := thread.Delivery(ctx)
	records, err := r.config.Offers.Offer(ctx, principalID, chatID, threadID, ReviewHeading, ReviewLifetime, proposals)
	if records == nil {
		s.logger.WarnContext(ctx, "memory.review.offer_failed", "principal_id", principalID, "job_id", occurrence.JobID, "error", err)
		s.recordReview(ctx, principalID, reviewFailed, 0)
		return schedule.Result{Text: schedule.FailedText, ErrorKind: "offer"}, nil
	}
	if err != nil {
		// The cards are stored, and the approval sweep sends them until they expire.
		s.logger.WarnContext(ctx, "memory.review.undelivered", "principal_id", principalID, "job_id", occurrence.JobID, "error", err)
	}
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		s.record(ctx, principalID, kind, outcomeProposed)
		s.recordLesson(ctx, principalID, MechanismReview, outcomeProposed)
	}
	s.logger.InfoContext(ctx, "memory.review.done", "principal_id", principalID, "job_id", occurrence.JobID, "sent", len(proposals), "dropped", dropped)
	s.recordReview(ctx, principalID, reviewSent, len(proposals))
	// A review the user ran says what it sent, where the scheduler would otherwise add that there is
	// nothing to report.
	if occurrence.Manual {
		return schedule.Result{Text: sentText(len(proposals))}, nil
	}
	return schedule.Result{Quiet: true}, nil
}

func (review) nothing(occurrence schedule.Occurrence) schedule.Result {
	if occurrence.Manual {
		return schedule.Result{Text: ReviewNothingText}
	}
	return schedule.Result{Quiet: true}
}

func sentText(count int) string {
	if count == 1 {
		return "Your weekly review is above: 1 proposal to confirm or reject."
	}
	return fmt.Sprintf("Your weekly review is above: %d proposals to confirm or reject.", count)
}

// gather reads the week's signals. A reply its session file does not show is left out rather than
// guessed, and so is one whose file cannot be read.
func (r review) gather(ctx context.Context, principalID string, location *time.Location) (reviewInputs, error) {
	s := r.service
	now := s.now()
	inputs := reviewInputs{location: location, lessons: make(map[string]*lessonCounts)}
	if inputs.location == nil {
		inputs.location = time.UTC
	}
	if r.config.Exchanges != nil {
		replies, err := r.config.Signals.NegativeReplies(ctx, principalID, now.Add(-reviewWindow), maximumReviewReplies)
		if err != nil {
			return reviewInputs{}, err
		}
		// Each session's files are read once, for all of its replies.
		found := make(map[int]history.Exchange, len(replies))
		var sessions []string
		times := make(map[string][]time.Time)
		indexes := make(map[string][]int)
		for index, reply := range replies {
			if _, seen := times[reply.RuntimeSessionID]; !seen {
				sessions = append(sessions, reply.RuntimeSessionID)
			}
			times[reply.RuntimeSessionID] = append(times[reply.RuntimeSessionID], reply.SentAt)
			indexes[reply.RuntimeSessionID] = append(indexes[reply.RuntimeSessionID], index)
		}
		for _, session := range sessions {
			exchanges, err := r.config.Exchanges.Exchanges(ctx, principalID, session, times[session])
			if err != nil {
				s.logger.WarnContext(ctx, "memory.review.exchange_unreadable", "principal_id", principalID, "error", err)
				continue
			}
			for position, index := range indexes[session] {
				found[index] = exchanges[position]
			}
		}
		for index, reply := range replies {
			if read := found[index]; read.Found {
				inputs.exchanges = append(inputs.exchanges, exchange{NegativeReply: reply, user: read.User, reply: read.Reply})
			}
		}
	}
	var err error
	if inputs.outcomes, err = r.config.Signals.ToolOutcomes(ctx, principalID, now.Add(-reviewWindow)); err != nil {
		return reviewInputs{}, err
	}
	if inputs.entries, err = s.repository.ListMemory(ctx, principalID); err != nil {
		return reviewInputs{}, err
	}
	for _, entry := range inputs.entries {
		if entry.Kind == KindInstruction {
			inputs.instructions++
		}
	}
	if inputs.procedures, err = s.repository.ListProcedures(ctx, principalID); err != nil {
		return reviewInputs{}, err
	}
	resolved, err := r.config.Signals.ResolvedApprovals(ctx, principalID, lessonTools, now.Add(-lessonWindow))
	if err != nil {
		return reviewInputs{}, err
	}
	for _, record := range resolved {
		mechanism := lessonMechanism(record)
		if mechanism == "" {
			continue
		}
		counts := inputs.lessons[mechanism]
		if counts == nil {
			counts = &lessonCounts{}
			inputs.lessons[mechanism] = counts
		}
		switch {
		case record.Status == approval.StatusExecuted || record.Status == approval.StatusFailed:
			counts.confirmed++
		case record.Status == approval.StatusRejected && record.Reason != approval.ReasonWithdrawn:
			counts.rejected++
		case record.Status == approval.StatusExpired:
			counts.expired++
		}
	}
	return inputs, nil
}

// lessonMechanism is the mechanism a resolved proposal came from, or "" for one that was no lesson.
func lessonMechanism(record approval.Record) string {
	var marks struct {
		Learned bool `json:"learned"`
		Review  bool `json:"review"`
	}
	if json.Unmarshal(record.Arguments, &marks) != nil {
		return ""
	}
	switch {
	case marks.Review || record.Tool == toolMerge || record.Tool == toolProcedureRemove:
		return MechanismReview
	case record.Tool == toolProcedureSave:
		return MechanismProcedure
	case marks.Learned:
		return MechanismFeedback
	default:
		return ""
	}
}

// exposed reports whether a 👎 reply the review reads came from a conversation that read
// third-party content.
func (inputs reviewInputs) exposed() bool {
	return slices.ContainsFunc(inputs.exchanges, func(exchange exchange) bool { return exchange.Exposed })
}

// worthReviewing is false for a week that leaves the review nothing to work from: no 👎 reply, no
// failed tool call, nothing to merge, and no procedure to prune.
func (inputs reviewInputs) worthReviewing() bool {
	failed := slices.ContainsFunc(inputs.outcomes, func(outcome ToolOutcome) bool { return outcome.Outcome != "success" })
	return len(inputs.exchanges) > 0 || failed || inputs.instructions >= 2 || len(inputs.procedures) > 0
}

// render writes the signals for the review's message. Times are in the job's zone.
func (inputs reviewInputs) render() string {
	const day = "Mon 2 Jan 2006"
	var sections []string

	if len(inputs.exchanges) == 0 {
		sections = append(sections, "Replies the user marked 👎 in the past 7 days: none.")
	} else {
		lines := []string{"Replies the user marked 👎 in the past 7 days:"}
		for index, exchange := range inputs.exchanges {
			reconsidered := "no"
			if exchange.Reconsidered {
				reconsidered = "yes"
			}
			lines = append(lines, fmt.Sprintf("%d. Sent %s; looked at again after the 👎: %s.", index+1, exchange.SentAt.In(inputs.location).Format(day+" 15:04"), reconsidered))
			if exchange.user != "" {
				lines = append(lines, "The user wrote: "+clip.Runes(exchange.user, reviewExchangeRunes))
			}
			lines = append(lines, "The reply: "+clip.Runes(exchange.reply, reviewExchangeRunes))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}

	if len(inputs.outcomes) == 0 {
		sections = append(sections, "Tool calls in the past 7 days: none.")
	} else {
		lines := []string{"Tool calls in the past 7 days, by tool and outcome:"}
		var tool string
		var counts []string
		flush := func() {
			if tool != "" {
				lines = append(lines, "- "+tool+": "+strings.Join(counts, ", "))
			}
		}
		for _, outcome := range inputs.outcomes {
			if outcome.Tool != tool {
				flush()
				tool, counts = outcome.Tool, nil
			}
			counts = append(counts, outcome.Outcome+" "+strconv.Itoa(outcome.Count))
		}
		flush()
		sections = append(sections, strings.Join(lines, "\n"))
	}

	var instructions []string
	for _, entry := range inputs.entries {
		if entry.Kind != KindInstruction {
			continue
		}
		line := "- [" + entry.ID + "] " + entry.Text
		if entry.Source == SourceLearned {
			line += " (learned)"
		}
		instructions = append(instructions, line)
	}
	if len(instructions) == 0 {
		sections = append(sections, "Instructions the user keeps: none.")
	} else {
		sections = append(sections, "Instructions the user keeps:\n"+strings.Join(instructions, "\n"))
	}

	if len(inputs.procedures) == 0 {
		sections = append(sections, "Procedures the user keeps: none.")
	} else {
		lines := []string{"Procedures the user keeps:"}
		for _, procedure := range inputs.procedures {
			read := "never read"
			if !procedure.ReadAt.IsZero() {
				read = "last read " + procedure.ReadAt.In(inputs.location).Format(day)
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s (kept since %s, %s)\n  Steps: %s", procedure.ID, procedure.Title,
				procedure.CreatedAt.In(inputs.location).Format(day), read, strings.ReplaceAll(procedure.Steps, "\n", " / ")))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}

	names := map[string]string{MechanismFeedback: "from a 👎", MechanismProcedure: "procedures offered after a chat", MechanismReview: "from the weekly review"}
	var lessons []string
	for _, mechanism := range lessonMechanisms {
		if counts := inputs.lessons[mechanism]; counts != nil {
			lessons = append(lessons, fmt.Sprintf("- %s: %d confirmed, %d rejected, %d expired", names[mechanism], counts.confirmed, counts.rejected, counts.expired))
		}
	}
	if len(lessons) == 0 {
		sections = append(sections, "Lessons proposed in the past 30 days: none.")
	} else {
		sections = append(sections, "Lessons proposed in the past 30 days:\n"+strings.Join(lessons, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

type reviewReply struct {
	Changes []reviewChange  `json:"changes"`
	Backlog []reviewBacklog `json:"backlog"`
}

type reviewChange struct {
	Action string   `json:"action"`
	Reason string   `json:"reason"`
	ID     string   `json:"id"`
	IDs    []string `json:"ids"`
	Text   string   `json:"text"`
	Title  string   `json:"title"`
	Steps  string   `json:"steps"`
}

type reviewBacklog struct {
	Title   string `json:"title"`
	Details string `json:"details"`
}

// decodeReview reads the model's reply, tolerating a code fence around it and nothing else.
func decodeReview(text string) (reviewReply, bool) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimPrefix(text, "json")
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	if text == "" || len(text) > maximumReviewBytes {
		return reviewReply{}, false
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	var reply reviewReply
	if err := decoder.Decode(&reply); err != nil {
		return reviewReply{}, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return reviewReply{}, false
	}
	return reply, true
}

// reviewMemory is the memory a review's changes are judged against, with the changes accepted so far
// applied, so that together they still fit the caps and touch each entry once.
type reviewMemory struct {
	entries    []Entry
	procedures []Procedure
	touched    map[string]bool
	added      int
}

// proposals validates the reply's changes against the principal's memory and turns the first valid
// ones into labelled proposals, then the backlog items; an item too long to show whole is dropped.
// Only the reasons and the backlog items are screened. kinds names each proposal's memory kind, ""
// for a backlog item, and dropped counts what was left out.
func (r review) proposals(ctx context.Context, principalID string, reply reviewReply, inputs reviewInputs) ([]approval.Proposal, []string, int) {
	memory := &reviewMemory{entries: slices.Clone(inputs.entries), procedures: slices.Clone(inputs.procedures), touched: make(map[string]bool)}
	var proposals []approval.Proposal
	var kinds []string
	dropped := 0
	for _, change := range reply.Changes {
		if len(proposals) == MaximumReviewChanges {
			dropped++
			continue
		}
		proposal, kind, reason := memory.accept(change)
		if reason != "" {
			action := change.Action
			if !slices.Contains(reviewActions, action) {
				action = "unknown"
			}
			r.service.logger.InfoContext(ctx, "memory.review.dropped", "principal_id", principalID, "action", action, "reason", reason)
			dropped++
			continue
		}
		proposal = r.label(ctx, principalID, proposal, change.Reason, "")
		if inputs.exposed() {
			proposal.Summary = reviewExposedWarning + "\n" + proposal.Summary
		}
		if !approval.SummaryFits(proposal.Summary) {
			r.service.logger.InfoContext(ctx, "memory.review.dropped", "principal_id", principalID, "action", change.Action, "reason", "too_long")
			dropped++
			continue
		}
		proposals = append(proposals, proposal)
		kinds = append(kinds, kind)
	}
	backlogged := 0
	for _, item := range reply.Backlog {
		if r.config.Backlog == nil || backlogged == MaximumReviewBacklog {
			dropped++
			continue
		}
		proposal, reason := r.config.Backlog.AddProposal(item.Title, item.Details)
		if reason != "" {
			r.service.logger.InfoContext(ctx, "memory.review.dropped", "principal_id", principalID, "action", "backlog", "reason", reason)
			dropped++
			continue
		}
		proposal = r.label(ctx, principalID, proposal, "", item.Title+"\n"+item.Details)
		if !approval.SummaryFits(proposal.Summary) {
			r.service.logger.InfoContext(ctx, "memory.review.dropped", "principal_id", principalID, "action", "backlog", "reason", "too_long")
			dropped++
			continue
		}
		proposals = append(proposals, proposal)
		kinds = append(kinds, "")
		backlogged++
	}
	return proposals, kinds, dropped
}

// label puts where a proposal came from and why above it, and the warning above both when the
// screener flags the reason or the given prose, such as a backlog item.
func (r review) label(ctx context.Context, principalID string, proposal approval.Proposal, reason, screened string) approval.Proposal {
	reason = clip.Runes(normalize(reason), reviewReasonRunes)
	label := reviewLabel + ":"
	if reason != "" {
		label += " " + reason
		screened = strings.TrimSpace(screened + "\n" + reason)
	}
	proposal.Summary = label + "\n" + proposal.Summary
	if r.service.screener != nil && screened != "" {
		screenCtx, cancel := context.WithTimeout(ctx, screenTimeout)
		flagged, err := r.service.screener.Screen(screenCtx, principalID, proposal.Tool, screened)
		cancel()
		if err == nil && flagged {
			proposal.Summary = reviewWarning + "\n" + proposal.Summary
		}
	}
	return proposal
}

// accept judges one change and, when it is valid, applies it to the memory and returns its proposal
// and its memory kind; otherwise it says why the change is dropped.
func (m *reviewMemory) accept(change reviewChange) (approval.Proposal, string, string) {
	switch change.Action {
	case actionAddInstruction:
		text := normalize(change.Text)
		if reason := m.instructionText(text, nil); reason != "" {
			return approval.Proposal{}, "", reason
		}
		m.added++
		m.entries = append(m.entries, Entry{ID: placeholderPrefix + strconv.Itoa(m.added), Kind: KindInstruction, Text: text})
		arguments, err := json.Marshal(proposedAdd{addArguments: addArguments{Kind: KindInstruction, Text: text}, Review: true})
		return approval.Proposal{
			Tool: toolAdd, Arguments: arguments,
			Summary: fmt.Sprintf("Remember this instruction: %q\nIt will apply to every later conversation, scheduled run, and background task until you delete it in /memory.", text),
		}, KindInstruction, errorText(err)
	case actionReplaceInstruction:
		text := normalize(change.Text)
		entry, reason := m.instruction(change.ID)
		if reason == "" {
			reason = m.instructionText(text, []string{entry.ID})
		}
		if reason != "" {
			return approval.Proposal{}, "", reason
		}
		m.replace(entry.ID, text)
		arguments, err := json.Marshal(proposedReplace{replaceArguments: replaceArguments{ID: entry.ID, Text: text}, Review: true, Previous: entry.Text})
		return approval.Proposal{
			Tool: toolReplace, Arguments: arguments,
			Summary: fmt.Sprintf("Change the remembered instruction\nfrom: %q\nto: %q", entry.Text, text),
		}, KindInstruction, errorText(err)
	case actionMergeInstructions:
		text := normalize(change.Text)
		var merged []Entry
		for _, id := range change.IDs {
			entry, reason := m.instruction(strings.TrimSpace(id))
			if reason != "" {
				return approval.Proposal{}, "", reason
			}
			if slices.ContainsFunc(merged, func(other Entry) bool { return other.ID == entry.ID }) {
				return approval.Proposal{}, "", "an instruction is named twice"
			}
			merged = append(merged, entry)
		}
		if len(merged) < 2 {
			return approval.Proposal{}, "", "a merge needs two or more instructions"
		}
		ids := make([]string, 0, len(merged))
		texts := make([]string, 0, len(merged))
		lines := make([]string, 0, len(merged))
		for _, entry := range merged {
			ids, texts, lines = append(ids, entry.ID), append(texts, entry.Text), append(lines, fmt.Sprintf("- %q", entry.Text))
		}
		if reason := m.instructionText(text, ids); reason != "" {
			return approval.Proposal{}, "", reason
		}
		m.replace(ids[0], text)
		for _, id := range ids[1:] {
			m.remove(id)
		}
		arguments, err := json.Marshal(proposedMerge{IDs: ids, Texts: texts, Text: text})
		return approval.Proposal{
			Tool: toolMerge, Arguments: arguments,
			Summary: fmt.Sprintf("Merge these instructions into one:\n%s\ninto: %q", strings.Join(lines, "\n"), text),
		}, KindInstruction, errorText(err)
	case actionRemoveInstruction:
		entry, reason := m.instruction(change.ID)
		if reason != "" {
			return approval.Proposal{}, "", reason
		}
		m.remove(entry.ID)
		arguments, err := json.Marshal(proposedRemove{ID: entry.ID, Text: entry.Text, Review: true})
		return approval.Proposal{
			Tool: toolRemove, Arguments: arguments,
			Summary: fmt.Sprintf("Forget the remembered instruction: %q", entry.Text),
		}, KindInstruction, errorText(err)
	case actionSaveProcedure:
		saved := proposedProcedure{ID: strings.TrimSpace(change.ID), Title: normalize(change.Title), Steps: normalizeSteps(change.Steps), Review: true}
		if reason := validProcedure(saved.Title, saved.Steps); reason != "" {
			return approval.Proposal{}, "", reason
		}
		if strings.HasPrefix(saved.ID, placeholderPrefix) {
			return approval.Proposal{}, "", "no kept procedure has that id"
		}
		if saved.ID != "" && m.touched[saved.ID] {
			return approval.Proposal{}, "", "another change touches that procedure"
		}
		replaced, rejected := checkProcedure(m.procedures, saved)
		if rejected.outcome != "" {
			return approval.Proposal{}, "", rejected.text
		}
		saved.Updated = replaced.UpdatedAt
		summary := "Keep this procedure: " + saved.Title
		if saved.ID != "" {
			summary = fmt.Sprintf("Replace the procedure %q with this one: %s", replaced.Title, saved.Title)
			m.touched[saved.ID] = true
			for index := range m.procedures {
				if m.procedures[index].ID == saved.ID {
					m.procedures[index].Title = saved.Title
				}
			}
		} else {
			m.added++
			m.procedures = append(m.procedures, Procedure{ID: placeholderPrefix + strconv.Itoa(m.added), Title: saved.Title})
		}
		arguments, err := json.Marshal(saved)
		return approval.Proposal{
			Tool: toolProcedureSave, Arguments: arguments,
			Summary: summary + "\n" + saved.Steps + "\nI will read it before similar requests until you delete it in /memory.",
		}, KindProcedure, errorText(err)
	case actionRemoveProcedure:
		id := strings.TrimSpace(change.ID)
		index := slices.IndexFunc(m.procedures, func(procedure Procedure) bool { return procedure.ID == id })
		if index < 0 || strings.HasPrefix(id, placeholderPrefix) {
			return approval.Proposal{}, "", "no kept procedure has that id"
		}
		if m.touched[id] {
			return approval.Proposal{}, "", "another change touches that procedure"
		}
		procedure := m.procedures[index]
		m.touched[id] = true
		m.procedures = slices.Delete(m.procedures, index, index+1)
		arguments, err := json.Marshal(proposedProcedureRemoval{ID: procedure.ID, Title: procedure.Title, Updated: procedure.UpdatedAt})
		return approval.Proposal{
			Tool: toolProcedureRemove, Arguments: arguments,
			Summary: fmt.Sprintf("Forget the procedure %q.", procedure.Title),
		}, KindProcedure, errorText(err)
	default:
		return approval.Proposal{}, "", "unknown action"
	}
}

// instruction finds a kept instruction no earlier change touched.
func (m *reviewMemory) instruction(id string) (Entry, string) {
	id = strings.TrimSpace(id)
	entry, found := find(m.entries, id)
	if !found || entry.Kind != KindInstruction || strings.HasPrefix(id, placeholderPrefix) {
		return Entry{}, "no kept instruction has that id"
	}
	if m.touched[id] {
		return Entry{}, "another change touches that instruction"
	}
	return entry, ""
}

// instructionText judges an instruction's new text as replacing the given entries, whose old texts
// no longer count, and marks them touched when it passes.
func (m *reviewMemory) instructionText(text string, replacing []string) string {
	if reason := validText(text); reason != "" {
		return reason
	}
	kept := slices.DeleteFunc(slices.Clone(m.entries), func(entry Entry) bool { return slices.Contains(replacing, entry.ID) })
	if rejected := check(kept, KindInstruction, text, ""); rejected.outcome != "" {
		return rejected.outcome
	}
	for _, id := range replacing {
		m.touched[id] = true
	}
	return ""
}

func (m *reviewMemory) replace(id, text string) {
	for index := range m.entries {
		if m.entries[index].ID == id {
			m.entries[index].Text = text
		}
	}
}

func (m *reviewMemory) remove(id string) {
	m.touched[id] = true
	m.entries = slices.DeleteFunc(m.entries, func(entry Entry) bool { return entry.ID == id })
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
