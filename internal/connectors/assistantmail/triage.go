package assistantmail

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// Checker runs one triage of new mail: a check nobody watches, on the principal's own
// subscription, offered only the scope's tools. The application adapts the turn engine's
// RunConfined to it. A failed run comes back as its error kind; an error means ctx ended.
type Checker interface {
	RunConfined(ctx context.Context, principalID, message string, scope tools.Scope) (text, errorKind string, err error)
}

const (
	// triageThreads bounds the threads one triage of a principal runs the model for; the rest wait
	// for the next poll.
	triageThreads = 10
	// triageBudget is the tool calls one thread's run may make.
	triageBudget = 12
	// maximumTriageBytes bounds the reply that is read.
	maximumTriageBytes = 16 << 10
	// maximumWhyRunes caps the reason a message needs the user, which the model writes from it.
	maximumWhyRunes = 200
	// maximumDailyRuns caps one principal's triage runs in any 24 hours, so a flood of threads cannot
	// spend their subscription; past it, new mail is listed untriaged.
	maximumDailyRuns = 30
	runWindow        = 24 * time.Hour
)

// webText finds a link or a domain-like token, which a reason written from a stranger's mail must
// not carry into the notice.
var webText = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://|\bwww\.|\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)

// Triage outcomes, as counted per thread run.
const (
	triageSilent   = "silent"
	triageNotified = "notified"
	triageFallback = "fallback"
	triageFailed   = "failed"
	// triageCapped counts the threads listed untriaged past maximumDailyRuns.
	triageCapped = "capped"
)

// verdict is what became of one new message.
type verdict uint8

const (
	// verdictSilent needs nothing from the user.
	verdictSilent verdict = iota
	// verdictNeeds needs the user, and a notice lists it with its reason.
	verdictNeeds
	// verdictUnsorted could not be triaged, and a notice lists it as it arrived.
	verdictUnsorted
)

type decision struct {
	verdict verdict
	why     string
	forced  bool
	// refusal is the code's reason for a message whose routine reply it refused.
	refusal locale.Text
}

// triageState is what one principal's triage keeps between polls. triageMu guards running; the rest
// is touched only by the principal's one running triage.
type triageState struct {
	running bool
	// decided holds the decisions on mail not yet settled, so a notice that failed is sent again
	// without running the model again.
	decided map[string]decision
	// failures counts the polls in a row whose notice failed to deliver.
	failures int
	// headerFailures counts, per message, the polls in a row whose read of its headers failed.
	headerFailures map[string]int
	// runs are when the model ran for the principal in the last runWindow.
	runs []time.Time
	// repliesHeld is set once a notice that carried only routine replies was given up, or the
	// principal had no chat, so they wait for the next notice of new mail instead of being tried
	// again every poll.
	repliesHeld bool
}

// allowRun reports whether another run fits maximumDailyRuns at now, and counts it when it does.
func (state *triageState) allowRun(now time.Time) bool {
	state.runs = slices.DeleteFunc(state.runs, func(at time.Time) bool { return !at.After(now.Add(-runWindow)) })
	if len(state.runs) >= maximumDailyRuns {
		return false
	}
	state.runs = append(state.runs, now)
	return true
}

// readIDs keeps the ids a triage run read in full, which only those reads write.
type readIDs struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (r *readIDs) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids[id] = true
}

func (r *readIDs) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ids[id]
}

// startTriage starts, off the poll's goroutine, the triage of each principal with the switch on whose
// last one has finished; mail that arrives while one runs waits for a later poll. A triage runs on
// ctx, so the loop's end cancels it.
func (s *Service) startTriage(ctx context.Context) {
	for _, principal := range s.principals() {
		if !s.observed[principal].Enabled {
			continue
		}
		s.triageMu.Lock()
		state := s.triages[principal]
		if state == nil {
			state = &triageState{decided: make(map[string]decision), headerFailures: make(map[string]int)}
			s.triages[principal] = state
		}
		if state.running {
			s.triageMu.Unlock()
			continue
		}
		state.running = true
		s.triageWG.Add(1)
		s.triageMu.Unlock()
		go func() {
			defer s.triageWG.Done()
			s.triage(ctx, principal, state)
			s.triageMu.Lock()
			state.running = false
			s.triageMu.Unlock()
		}()
	}
}

// WaitTriage waits for the triages the polls so far started, for a caller that polls by hand.
func (s *Service) WaitTriage(ctx context.Context) error {
	triaged := make(chan struct{})
	go func() {
		s.triageWG.Wait()
		close(triaged)
	}()
	select {
	case <-triaged:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// triage settles the principal's new mail and counts how its notice went.
func (s *Service) triage(ctx context.Context, principalID string, state *triageState) {
	outcome, err := s.triageTo(ctx, principalID, state)
	if ctx.Err() != nil {
		return
	}
	if outcome != "" {
		s.telemetry.RecordAssistantMailNotice(ctx, outcome)
	}
	if err != nil {
		s.logger.WarnContext(ctx, "assistant_mail.notice.failed", "principal_id", principalID, "outcome", outcome, "error", err)
	}
}

// triageTo triages each thread with new mail not yet decided, settles the mail that needs nothing,
// and sends one notice of the rest. It returns the notice's outcome, or "" when there was nothing to
// tell. Spam is settled unseen. Mail of a principal without an active chat, or whose chat refuses
// the bot, is settled without a notice, and so is mail whose notice failed abandonAfter polls in a
// row.
func (s *Service) triageTo(ctx context.Context, principalID string, state *triageState) (string, error) {
	pending, err := s.repository.PendingNotices(ctx, principalID, pendingRead)
	if err != nil {
		return noticeFailed, err
	}
	state.keepOnly(pending)
	if len(pending) == 0 {
		replies, err := s.repository.UnreportedRoutineReplies(ctx, principalID)
		if err != nil {
			return noticeFailed, err
		}
		if len(replies) == 0 || state.repliesHeld {
			state.failures = 0
			return "", nil
		}
	}
	chatID, found, err := s.chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return noticeFailed, err
	}
	if !found {
		clear(state.decided)
		state.failures, state.repliesHeld = 0, true
		return s.settle(ctx, principalID, routeIDs(pending), noticeNoChat)
	}
	ctx, err = locale.ForPrincipal(ctx, s.languages, principalID)
	if err != nil {
		s.logger.WarnContext(ctx, "assistant_mail.language.load_failed", "principal_id", principalID, "error", err)
	}
	var spam []string
	var threads [][]Route
	threadIndex := make(map[string]int)
	for _, route := range pending {
		if route.Spam {
			spam = append(spam, route.GmailID)
			continue
		}
		if _, decided := state.decided[route.GmailID]; decided {
			continue
		}
		key := cmp.Or(route.ThreadID, route.GmailID)
		index, found := threadIndex[key]
		if !found {
			index = len(threads)
			threadIndex[key] = index
			threads = append(threads, nil)
		}
		threads[index] = append(threads[index], route)
	}
	if len(spam) > 0 {
		if outcome, err := s.settle(ctx, principalID, spam, ""); err != nil {
			return outcome, err
		}
	}
	if s.checker == nil {
		for _, routes := range threads {
			decideAll(state, routeIDs(routes), verdictUnsorted)
		}
		threads = nil
	}
	for _, routes := range threads[:min(len(threads), triageThreads)] {
		if err := s.decide(ctx, principalID, routes, state); err != nil {
			return noticeFailed, err
		}
	}
	notice, silent, err := s.buildNotice(ctx, principalID, pending, state)
	if err != nil {
		return noticeFailed, err
	}
	if outcome, err := s.settleDecided(ctx, principalID, silent, "", state); err != nil {
		return outcome, err
	}
	if notice.listed() == 0 {
		return s.settleDecided(ctx, principalID, notice.covered, "", state)
	}
	// Triage can take minutes, so the switch and the chat are read again: mail of a switch turned
	// off meanwhile is settled, as turning it on again would settle it unannounced.
	enabled, err := s.switches.Enabled(ctx, principalID, Switch)
	if err != nil {
		return noticeFailed, err
	}
	if !enabled {
		state.failures = 0
		return s.settleDecided(ctx, principalID, notice.covered, noticeSwitchedOff, state)
	}
	if chatID, found, err = s.chats.ResolveActiveChat(ctx, principalID); err != nil {
		return noticeFailed, err
	}
	if !found {
		state.failures, state.repliesHeld = 0, true
		return s.settleDecided(ctx, principalID, notice.covered, noticeNoChat, state)
	}
	if err := s.sender.SendPlainReply(ctx, chatID, 0, s.noticeText(ctx, &notice)); err != nil {
		if ctx.Err() != nil {
			return noticeFailed, err
		}
		outcome := noticeBlocked
		if !isPermanent(err) {
			if state.failures++; state.failures < abandonAfter {
				return noticeFailed, err
			}
			outcome = noticeAbandoned
		}
		state.failures = 0
		state.repliesHeld = len(notice.covered) == 0
		s.logger.WarnContext(ctx, "assistant_mail.notice.dropped", "principal_id", principalID, "outcome", outcome, "error", err)
		return s.settleDecided(ctx, principalID, notice.covered, outcome, state)
	}
	state.failures, state.repliesHeld = 0, false
	if s.notices != nil {
		s.notices.keep(principalID, s.turnNotice(notice))
	}
	// The notice is in the chat, so marking its mail and its replies outlives a shutdown that has
	// begun; otherwise the next poll announces them again.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeStoreTimeout)
	defer cancel()
	if err := s.repository.MarkRoutineRepliesReported(storeCtx, principalID, notice.replyIDs(), s.now()); err != nil {
		s.logger.WarnContext(ctx, "assistant_mail.routine.unmarked", "principal_id", principalID, "error", err)
	}
	return s.settleDecided(storeCtx, principalID, notice.covered, noticeSent, state)
}

// buildNotice reads the headers of the decided mail a notice lists, oldest first, and returns the
// notice and the ids of the mail that needs nothing. A message gone from the mailbox is settled
// unlisted, and past noticeListed messages the rest are only counted. The notice also carries every
// routine reply not yet reported, from its record.
func (s *Service) buildNotice(ctx context.Context, principalID string, pending []Route, state *triageState) (pendingNotice, []string, error) {
	notice := newPendingNotice()
	replies, err := s.repository.UnreportedRoutineReplies(ctx, principalID)
	if err != nil {
		return pendingNotice{}, nil, err
	}
	for _, reply := range replies {
		var arguments routineArguments
		if err := json.Unmarshal(reply.Arguments, &arguments); err != nil {
			s.logger.WarnContext(ctx, "assistant_mail.routine.unreadable", "principal_id", principalID)
			continue
		}
		if len(notice.replied.entries) == noticeListed {
			notice.replied.more++
			continue
		}
		notice.replied.entries = append(notice.replied.entries, noticeEntry{
			shown: shown{address: cleanAddress(arguments.To), verified: true}, at: reply.StartedAt, body: arguments.Body, sendID: reply.SendID, uncertain: reply.Uncertain,
		})
	}
	var silent []string
	for _, route := range pending {
		decided, found := state.decided[route.GmailID]
		if route.Spam || !found {
			continue
		}
		if decided.verdict == verdictSilent {
			silent = append(silent, route.GmailID)
			continue
		}
		notice.covered = append(notice.covered, route.GmailID)
		section := &notice.unsorted
		if decided.verdict == verdictNeeds {
			section = &notice.needs
		}
		if len(notice.needs.entries)+len(notice.unsorted.entries) == noticeListed {
			section.more++
			continue
		}
		message, found, err := s.triageHeaders(ctx, principalID, route.GmailID, state)
		if err != nil {
			return pendingNotice{}, nil, err
		}
		if found {
			section.entries = append(section.entries, noticeEntry{
				shown: shownMessage(message), at: route.InternalDate, why: decided.why, forced: decided.forced, refusal: decided.refusal,
			})
		}
	}
	return notice, silent, nil
}

// triageHeaders reads the headers of one message, reporting false for a message gone from the
// mailbox. A message whose headers failed abandonAfter polls in a row is given up as gone, so it
// cannot hold back the principal's other mail.
func (s *Service) triageHeaders(ctx context.Context, principalID, id string, state *triageState) (gmail.Message, bool, error) {
	message, err := s.headers(ctx, id)
	switch {
	case err == nil:
		delete(state.headerFailures, id)
		return message, true, nil
	case isGone(err):
		delete(state.headerFailures, id)
		return gmail.Message{}, false, nil
	case ctx.Err() != nil:
		return gmail.Message{}, false, err
	}
	if state.headerFailures[id]++; state.headerFailures[id] < abandonAfter {
		return gmail.Message{}, false, err
	}
	delete(state.headerFailures, id)
	s.telemetry.RecordAssistantMailNotice(ctx, noticeAbandoned)
	s.logger.WarnContext(ctx, "assistant_mail.message.abandoned", "principal_id", principalID, "error", err)
	return gmail.Message{}, false, nil
}

// decide triages one thread's new messages in one model run that sees only the thread, and that may
// send a routine reply while the principal has routine replies on. A message whose sender is not
// verified needs the user whatever the run says, a message whose routine reply code refused needs
// them with the code's reason unless the run listed it itself, and one the run never read is listed
// as it arrived; when the run fails, its reply cannot be read, or the principal's runs reached
// maximumDailyRuns, every message is listed as it arrived.
func (s *Service) decide(ctx context.Context, principalID string, routes []Route, state *triageState) error {
	var ids []string
	unverified := make(map[string]bool)
	for _, route := range routes {
		message, found, err := s.triageHeaders(ctx, principalID, route.GmailID, state)
		if err != nil {
			return err
		}
		if !found {
			state.decided[route.GmailID] = decision{verdict: verdictSilent}
			continue
		}
		ids = append(ids, route.GmailID)
		unverified[route.GmailID] = !shownMessage(message).verified
	}
	threadID := routes[0].ThreadID
	if len(ids) == 0 {
		return nil
	}
	if threadID == "" {
		decideAll(state, ids, verdictUnsorted)
		return nil
	}
	if !state.allowRun(s.now()) {
		s.logger.WarnContext(ctx, "assistant_mail.triage.capped", "principal_id", principalID)
		s.telemetry.RecordAssistantMailTriage(ctx, triageCapped)
		decideAll(state, ids, verdictUnsorted)
		return nil
	}
	read, routine := &readIDs{ids: make(map[string]bool)}, newRoutineLog()
	scope := tools.Scope{Tools: []string{toolList, toolRead}, Bind: func(ctx context.Context) context.Context {
		return withThread(ctx, principalID, threadID, read, routine)
	}}
	offered, err := s.routineOffered(ctx, principalID)
	if err != nil {
		return err
	}
	guidance := ""
	if offered {
		scope.Tools, scope.Writes = append(scope.Tools, toolReply), []string{toolReply}
		guidance = s.routineText(principalID)
	}
	text, errorKind, err := s.checker.RunConfined(tools.WithCallBudget(ctx, triageBudget), principalID, prompt.MailTriage(ids, guidance), scope)
	if err != nil && ctx.Err() != nil {
		return err
	}
	needs, readable := decodeTriage(text)
	switch {
	case err != nil || errorKind != "":
		s.logger.WarnContext(ctx, "assistant_mail.triage.failed", "principal_id", principalID, "error_kind", errorKind, "error", err)
		s.telemetry.RecordAssistantMailTriage(ctx, triageFailed)
		decideAll(state, ids, verdictUnsorted)
		return nil
	case !readable:
		s.logger.WarnContext(ctx, "assistant_mail.triage.unreadable", "principal_id", principalID)
		s.telemetry.RecordAssistantMailTriage(ctx, triageFallback)
		decideAll(state, ids, verdictUnsorted)
		return nil
	}
	outcome := triageSilent
	for _, id := range ids {
		why, listed := needs[id]
		refusal, refused := routine.refusal(id)
		switch {
		case listed && read.has(id):
			state.decided[id] = decision{verdict: verdictNeeds, why: why, refusal: refusal}
		case refused:
			state.decided[id] = decision{verdict: verdictNeeds, refusal: refusal}
		case !read.has(id):
			// Mail the run never read, such as one past its call budget, is never settled on its word.
			state.decided[id] = decision{verdict: verdictUnsorted}
		case unverified[id]:
			state.decided[id] = decision{verdict: verdictNeeds, forced: true}
		default:
			state.decided[id] = decision{verdict: verdictSilent}
			continue
		}
		outcome = triageNotified
	}
	s.telemetry.RecordAssistantMailTriage(ctx, outcome)
	return nil
}

func decideAll(state *triageState, ids []string, verdict verdict) {
	for _, id := range ids {
		state.decided[id] = decision{verdict: verdict}
	}
}

// triageReply is the run's answer; a reply without needs_you cannot be read.
type triageReply struct {
	NeedsYou *[]struct {
		ID  string `json:"id"`
		Why string `json:"why"`
	} `json:"needs_you"`
}

// decodeTriage reads the run's strict JSON reply into the reason of each message it lists, cleaned
// as a subject is but allowed a longer line. Code, not the model, decides which ids count.
func decodeTriage(text string) (map[string]string, bool) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimPrefix(text, "json")
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	if text == "" || len(text) > maximumTriageBytes {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	var reply triageReply
	if err := decoder.Decode(&reply); err != nil || reply.NeedsYou == nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	needs := make(map[string]string, len(*reply.NeedsYou))
	for _, item := range *reply.NeedsYou {
		if id := strings.TrimSpace(item.ID); id != "" {
			if _, seen := needs[id]; !seen {
				needs[id] = reason(item.Why)
			}
		}
	}
	return needs, true
}

// reason cleans the run's reason as a subject is, allowing a longer line. A reason that carries a
// link or a domain is dropped for the code's own, so a stranger's mail cannot point the user
// anywhere through the notice.
func reason(why string) string {
	if webText.MatchString(why) {
		return ""
	}
	return cleanLine(why, maximumWhyRunes)
}

// settleDecided settles the routes and forgets their decisions once they are marked.
func (s *Service) settleDecided(ctx context.Context, principalID string, ids []string, outcome string, state *triageState) (string, error) {
	if len(ids) == 0 {
		return outcome, nil
	}
	outcome, err := s.settle(ctx, principalID, ids, outcome)
	if err == nil {
		for _, id := range ids {
			delete(state.decided, id)
		}
	}
	return outcome, err
}

// keepOnly forgets the decisions on mail no longer pending, such as mail a switch turned on again
// settled.
func (state *triageState) keepOnly(pending []Route) {
	waiting := make(map[string]bool, len(pending))
	for _, route := range pending {
		waiting[route.GmailID] = true
	}
	for id := range state.decided {
		if !waiting[id] {
			delete(state.decided, id)
		}
	}
	for id := range state.headerFailures {
		if !waiting[id] {
			delete(state.headerFailures, id)
		}
	}
}
