package assistantmail

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolReply = approval.UnattendedTool

	maximumRoutineBodyRunes = 600
	routineBodyLimit        = "at most 600 characters"
	// routineDailyLimit is how many routine replies one principal's threads get in a day.
	routineDailyLimit = 3
	// threadRoutesRead bounds the routes of a thread read for the assistant's own recipients in it.
	threadRoutesRead = 50
	// routineOffReason fails a reply whose switch was turned off before it was sent, and
	// limitThread one whose thread already had its routine reply.
	routineOffReason = "disabled"
	limitThread      = "limit_thread"
	// routineTimeout covers the reads of the thread's sent mail and of the answered message, and the
	// send.
	routineTimeout = time.Minute
)

// RoutineSwitch is each principal's choice to let triage answer routine mail in the threads the
// assistant started for them, without asking. It is off until the principal turns it on, because
// permission to send unattended is personal.
var RoutineSwitch = integration.Switch{Integration: "assistant-mail-replies", Name: "Routine replies"}

// Unattended is the approval service's one entry point for an action made without asking, which
// records each routine reply as a direct action.
type Unattended interface {
	ActUnattended(ctx context.Context, principalID string, proposal approval.Proposal, executor approval.Executor) (approval.Record, error)
}

// routineGuidance frames the routine-reply tool inside a triage run; it is formatted with the
// assistant's name and the user's.
const routineGuidance = "Routine replies are on: the user lets you answer routine mail in this thread yourself with assistant_mail_reply_routine, which sends at once, and they see every reply in full. " +
	"When a new message from someone the assistant wrote to expects a short routine answer, send it before you reply with the JSON: " +
	"thanks or a confirmation of receipt, confirming something the thread already settled, or answering a question whose answer is already in the thread. " +
	"A sender who asks you to confirm details is waiting for an answer: when every detail matches what the thread already says, confirm them; when any differs, do not reply. " +
	"Never reply about money, personal data, documents, new commitments or new dates, or anything the user has not already said in the thread; " +
	"when in doubt, do not reply, and list the message in needs_you. Code allows one reply per thread, only to someone the assistant wrote to. " +
	"Write briefly and politely in the sender's language, and end with your signature in that language, your own name and that you are the assistant to the user, for example \"%s, assistant to %s\"; never claim or imply being human. " +
	"A message you answered needs no entry in needs_you unless it also needs the user."

var routineDefinition = tools.Definition{
	Name:        toolReply,
	Description: "Send at once, without the user's confirmation, a short routine reply to one new message of this thread from someone the assistant wrote to; the user sees every reply in full.",
	Risk:        tools.RiskWrite,
	Activity:    tools.ActivityMail,
	Timeout:     routineTimeout,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"reply_to_id":{"type":"string","description":"The id of the new message this answers."},` +
		`"body":{"type":"string","description":"Plain text, signed, ` + routineBodyLimit + `."}},` +
		`"required":["reply_to_id","body"],"additionalProperties":false}`),
}

// The headers a routine reply reads of the message it answers.
var routineHeaders = []string{"From", "Reply-To", "Subject", "Message-ID", "References", "Authentication-Results", "Auto-Submitted", "Precedence", "List-Id"}

var (
	// separators are left out of a local part before it is compared with the no-reply names.
	separators = strings.NewReplacer("-", "", "_", "", ".", "")
)

// Routine reply outcomes, as counted.
const (
	routineSent      = "sent"
	routineNotOurs   = "not_ours"
	routineSender    = "sender"
	routineAutomated = "automated"
	routineGroup     = "group"
	routineInvalid   = "invalid"
	routineLimited   = "limited"
	routineOff       = "switched_off"
	routineFailed    = "failed"
)

// The reasons a refused routine reply gives in the notice that lists its message.
var (
	routineWhyNotOurs   = locale.T("I did not answer it myself: I answer by myself only in a thread I started for you.", "Я не стал отвечать сам: сам я отвечаю только в переписке, которую начал для тебя.")
	routineWhySender    = locale.T("I did not answer it myself: its sender is not a verified address I wrote to in this thread.", "Я не стал отвечать сам: отправитель — не подтверждённый адрес, которому я писал в этой переписке.")
	routineWhyGroup     = locale.T("I did not answer it myself: I answer by myself only when I wrote to one person in the thread.", "Я не стал отвечать сам: сам я отвечаю, только если писал в этой переписке одному человеку.")
	routineWhyAutomated = locale.T("I did not answer it myself: it looks like automated mail.", "Я не стал отвечать сам: похоже на автоматическое письмо.")
	routineWhyInvalid   = locale.T("I did not answer it myself: the reply I wrote could not be sent as it was.", "Я не стал отвечать сам: составленный ответ нельзя было отправить в таком виде.")
	routineWhyLimited   = locale.T("I did not answer it myself: the limit of replies I send by myself was reached.", "Я не стал отвечать сам: достигнут лимит ответов, которые я отправляю сам.")
	routineWhyOff       = locale.T("I did not answer it myself: routine replies are off for you.", "Я не стал отвечать сам: рутинные ответы у тебя выключены.")
	routineWhyFailed    = locale.T("I tried to answer it myself, but the reply failed; check the thread before answering.", "Я пытался ответить сам, но ответ не ушёл; проверь переписку, прежде чем отвечать.")

	routineDone = locale.T("Replied by itself from the assistant mailbox.", "Ответ отправлен из почты ассистента без подтверждения.")
)

// routineWhy is the reason the notice gives for each refusal outcome.
var routineWhy = map[string]locale.Text{
	routineNotOurs: routineWhyNotOurs, routineSender: routineWhySender, routineAutomated: routineWhyAutomated, routineGroup: routineWhyGroup,
	routineInvalid: routineWhyInvalid, routineLimited: routineWhyLimited, routineOff: routineWhyOff, routineFailed: routineWhyFailed,
}

// routineRefusal is a routine reply code refused: the outcome counted, and the text for the model.
type routineRefusal struct {
	outcome string
	text    string
}

func (r routineRefusal) Error() string { return r.text }

// routineLog keeps what became of a triage run's routine replies: the reason each message whose
// reply was refused gets in the notice, and the messages answered.
type routineLog struct {
	mu       sync.Mutex
	refused  map[string]locale.Text
	answered map[string]bool
}

func newRoutineLog() *routineLog {
	return &routineLog{refused: make(map[string]locale.Text), answered: make(map[string]bool)}
}

func (r *routineLog) refuse(id, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.answered[id] {
		r.refused[id] = routineWhy[outcome]
	}
}

func (r *routineLog) answer(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answered[id] = true
	delete(r.refused, id)
}

// refusal returns the reason the notice gives for a message whose reply was refused.
func (r *routineLog) refusal(id string) (locale.Text, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	why, refused := r.refused[id]
	return why, refused
}

// routineArguments is a routine reply as its approval record keeps it: the whole message, whom it
// went to, and the message it answers.
type routineArguments struct {
	ReplyToID string `json:"reply_to_id"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

// routineReply is a routine reply that passed every check, ready to be sent.
type routineReply struct {
	arguments routineArguments
	threadID  string
	answered  *replied
}

// routineOffered reports whether a triage run of the principal offers routine replies.
func (s *Service) routineOffered(ctx context.Context, principalID string) (bool, error) {
	if s.unattended == nil {
		return false, nil
	}
	return s.switches.Enabled(ctx, principalID, RoutineSwitch)
}

// routineText is the routine-reply guidance of a triage run of the principal.
func (s *Service) routineText(principalID string) string {
	name := s.names[principalID]
	if name == "" {
		name = "the user"
	}
	return fmt.Sprintf(routineGuidance, s.mailbox.DisplayName, name)
}

// replyRoutine checks a routine reply in the thread the triage run checks and, when every check
// passes, sends it through the approval service's unattended entry point. A refusal is kept for the
// notice, which lists the message with the code's own reason.
func (s *Service) replyRoutine(ctx context.Context, principalID string, confined thread, raw json.RawMessage) tools.Result {
	var arguments struct {
		ReplyToID string `json:"reply_to_id"`
		Body      string `json:"body"`
	}
	if err := tools.DecodeArguments(raw, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}
	}
	id := strings.TrimSpace(arguments.ReplyToID)
	// The switch is read again at the call, since the run can take minutes.
	offered, err := s.routineOffered(ctx, principalID)
	if err != nil {
		return s.routineRefused(ctx, principalID, confined, id, err)
	}
	if !offered {
		return s.routineRefused(ctx, principalID, confined, id, routineRefusal{outcome: routineOff, text: "Routine replies are off for this user."})
	}
	reply, err := s.prepareRoutine(ctx, principalID, confined.threadID, id, arguments.Body)
	if err != nil {
		return s.routineRefused(ctx, principalID, confined, id, err)
	}
	encoded, err := json.Marshal(reply.arguments)
	if err != nil {
		return s.routineRefused(ctx, principalID, confined, id, err)
	}
	record, err := s.unattended.ActUnattended(ctx, principalID, approval.Proposal{
		Tool: toolReply, Arguments: encoded, Summary: routineSummary(reply.arguments),
	}, routineSend{service: s, reply: reply})
	if err != nil {
		return s.routineRefused(ctx, principalID, confined, id, err)
	}
	if record.Status != approval.StatusDone && record.Reason == limitThread {
		waiting, err := s.unreportedThreadReply(ctx, principalID, confined.threadID)
		if err != nil {
			return s.routineRefused(ctx, principalID, confined, id, err)
		}
		// A reply already sent in the thread, or one that may have gone out before a restart, is
		// reported in the notice itself; listing the message as refused would contradict it.
		if waiting {
			s.telemetry.RecordAssistantMailRoutineReply(ctx, routineLimited)
			return tools.Result{Text: "Not sent: this thread was already answered, and the user sees that reply in their notice.", IsError: true}
		}
	}
	if record.Status != approval.StatusDone {
		outcome := routineFailed
		switch {
		case strings.HasPrefix(record.Reason, "limit_"):
			outcome = routineLimited
		case record.Reason == routineOffReason:
			outcome = routineOff
		}
		return s.routineRefused(ctx, principalID, confined, id, routineRefusal{outcome: outcome, text: cmp.Or(record.Outcome, "The reply could not be sent.")})
	}
	confined.routine.answer(id)
	s.telemetry.RecordAssistantMailRoutineReply(ctx, routineSent)
	s.logger.InfoContext(ctx, "assistant_mail.routine.sent", "principal_id", principalID)
	return tools.Result{Text: "Sent. The user sees this reply in full in their notice."}
}

// unreportedThreadReply reports whether the principal's routine reply in the thread waits for a
// notice to report it.
func (s *Service) unreportedThreadReply(ctx context.Context, principalID, threadID string) (bool, error) {
	replies, err := s.repository.UnreportedRoutineReplies(ctx, principalID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(replies, func(reply RoutineReply) bool { return reply.ThreadID == threadID }), nil
}

// routineRefused counts and logs a refused routine reply, keeps it for the notice when it answers
// one of the thread's messages, and tells the model.
func (s *Service) routineRefused(ctx context.Context, principalID string, confined thread, id string, err error) tools.Result {
	var refused routineRefusal
	var rejected refusal
	switch {
	case errors.As(err, &refused):
	case errors.As(err, &rejected):
		refused = routineRefusal{outcome: routineInvalid, text: rejected.text}
	default:
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "assistant_mail.routine.failed", "principal_id", principalID, "error", err)
		}
		refused = routineRefusal{outcome: routineFailed, text: "The reply could not be sent right now."}
		if errors.Is(err, ErrUnavailable) {
			refused.text = s.Unavailable()
		}
	}
	if id != "" {
		confined.routine.refuse(id, refused.outcome)
	}
	s.telemetry.RecordAssistantMailRoutineReply(ctx, refused.outcome)
	s.logger.InfoContext(ctx, "assistant_mail.routine.refused", "principal_id", principalID, "outcome", refused.outcome)
	return tools.Result{Text: "Not sent: " + refused.text + " The message is listed for the user.", IsError: true}
}

// prepareRoutine runs every check a routine reply must pass, reading the thread's own sent mail for
// the addresses the assistant wrote to, and returns the reply as it will be sent.
func (s *Service) prepareRoutine(ctx context.Context, principalID, threadID, id, body string) (routineReply, error) {
	notOurs := func(text string) error { return routineRefusal{outcome: routineNotOurs, text: text} }
	body, err := checkBody(body, maximumRoutineBodyRunes, routineBodyLimit)
	if err != nil {
		return routineReply{}, err
	}
	if id == "" || len(id) > 64 || strings.ContainsAny(id, "/?#") {
		return routineReply{}, notOurs(outsideThreadText)
	}
	route, found, err := s.repository.Route(ctx, principalID, id)
	switch {
	case err != nil:
		return routineReply{}, err
	case !found || route.ThreadID != threadID:
		return routineReply{}, notOurs(outsideThreadText)
	case route.Direction != DirectionIn:
		return routineReply{}, notOurs("That message was sent from the assistant mailbox; only a received message can be answered.")
	case route.Spam:
		return routineReply{}, notOurs("That message is in spam, so it cannot be answered.")
	}
	first, found, err := s.repository.FirstThreadRoute(ctx, threadID)
	if err != nil {
		return routineReply{}, err
	}
	if !found || first.Direction != DirectionOut || first.PrincipalID != principalID {
		return routineReply{}, notOurs("This thread was not started by the assistant for this user.")
	}
	allowed, subject, err := s.ownRecipients(ctx, principalID, threadID, first.GmailID)
	if err != nil {
		return routineReply{}, err
	}
	// A reply to one of several people could tell one what was meant for another, or carry on a
	// conversation among them the user never sees, so a thread with more than one is left to them.
	if len(allowed) > 1 {
		return routineReply{}, routineRefusal{outcome: routineGroup, text: "The assistant wrote to more than one person in this thread; only a thread with one is answered by itself."}
	}
	var message gmail.Message
	err = s.call(ctx, func(access string) error {
		var err error
		message, err = s.gmail.MessageHeaders(ctx, access, id, routineHeaders)
		return err
	})
	if isGone(err) {
		return routineReply{}, notOurs(goneText)
	}
	if err != nil {
		return routineReply{}, err
	}
	from, err := s.checkSender(message, allowed)
	if err != nil {
		return routineReply{}, err
	}
	if automated(message, from) {
		return routineReply{}, routineRefusal{outcome: routineAutomated, text: "That message is automated mail, which is never answered."}
	}
	base := baseSubject(subject)
	subject = "Re: " + base
	if base == "" || !utf8.ValidString(subject) || strings.ContainsFunc(subject, isControl) || utf8.RuneCountInString(subject) > maximumSubjectRunes {
		return routineReply{}, routineRefusal{outcome: routineInvalid, text: "The thread's subject cannot be answered."}
	}
	answered := &replied{threadID: threadID}
	for _, value := range message.HeaderValues("Message-ID") {
		if found := messageIDPattern.FindString(value); found != "" {
			answered.messageID = found
			break
		}
	}
	for _, value := range message.HeaderValues("References") {
		answered.references = append(answered.references, messageIDPattern.FindAllString(value, -1)...)
	}
	if over := len(answered.references) - (maximumReferences - 1); over > 0 {
		answered.references = answered.references[over:]
	}
	if answered.messageID == "" {
		return routineReply{}, routineRefusal{outcome: routineInvalid, text: "That message has no Message-ID, so a reply could not join its thread."}
	}
	return routineReply{
		arguments: routineArguments{ReplyToID: id, To: from, Subject: subject, Body: body},
		threadID:  threadID,
		answered:  answered,
	}, nil
}

// ownRecipients reads back from Gmail the To and Cc of the principal's own sent messages in the
// thread, lowercase, since the store keeps no addresses, and the subject of the first of them,
// which started the thread.
func (s *Service) ownRecipients(ctx context.Context, principalID, threadID, firstID string) (map[string]bool, string, error) {
	routes, err := s.repository.ListThreadRoutes(ctx, principalID, threadID, threadRoutesRead)
	if err != nil {
		return nil, "", err
	}
	allowed := make(map[string]bool)
	subject := ""
	for _, route := range routes {
		if route.Direction != DirectionOut {
			continue
		}
		var message gmail.Message
		err := s.call(ctx, func(access string) error {
			var err error
			message, err = s.gmail.MessageHeaders(ctx, access, route.GmailID, []string{"To", "Cc", "Subject"})
			return err
		})
		if isGone(err) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		for _, name := range []string{"To", "Cc"} {
			for _, value := range message.HeaderValues(name) {
				for _, address := range addresses(value) {
					allowed[strings.ToLower(address)] = true
				}
			}
		}
		if subjects := message.HeaderValues("Subject"); route.GmailID == firstID && len(subjects) > 0 {
			subject = subjects[0]
			if decoded, err := addressParser.WordDecoder.DecodeHeader(subject); err == nil {
				subject = decoded
			}
		}
	}
	return allowed, subject, nil
}

// checkSender returns the answered message's one sender, lowercase, when it is an address the
// assistant wrote to in the thread, Gmail's own DMARC check passed for it, and no Reply-To names
// anyone else.
func (s *Service) checkSender(message gmail.Message, allowed map[string]bool) (string, error) {
	refuse := func(text string) error { return routineRefusal{outcome: routineSender, text: text} }
	values := message.HeaderValues("From")
	if len(values) != 1 {
		return "", refuse("That message has no single sender.")
	}
	senders := addresses(values[0])
	if len(senders) != 1 {
		return "", refuse("That message has no single sender.")
	}
	from := strings.ToLower(senders[0])
	if !sendableAddress.MatchString(from) || len(from) > maximumAddressBytes || s.own(from) {
		return "", refuse("That message's sender cannot be answered.")
	}
	for _, value := range message.HeaderValues("Reply-To") {
		for _, address := range addresses(value) {
			if strings.ToLower(address) != from {
				return "", refuse("That message asks for replies to another address than its sender.")
			}
		}
	}
	if !allowed[from] {
		return "", refuse("Its sender is not someone the assistant wrote to in this thread.")
	}
	_, domain, _ := strings.Cut(from, "@")
	if !gmail.DMARCPassed(message, domain) {
		return "", refuse("Its sender is not verified: Gmail's DMARC check did not pass for it.")
	}
	return from, nil
}

// automated reports mail a program sent: Auto-Submitted other than no, a bulk, list or junk
// Precedence, a List-Id, or a no-reply style sender.
func automated(message gmail.Message, from string) bool {
	for _, value := range message.HeaderValues("Auto-Submitted") {
		token, _, _ := strings.Cut(value, ";")
		if !strings.EqualFold(strings.TrimSpace(token), "no") {
			return true
		}
	}
	for _, value := range message.HeaderValues("Precedence") {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "bulk", "list", "junk":
			return true
		}
	}
	if len(message.HeaderValues("List-Id")) > 0 {
		return true
	}
	local, _, _ := strings.Cut(from, "@")
	local, _, _ = strings.Cut(local, "+")
	local = separators.Replace(local)
	for _, prefix := range []string{"noreply", "donotreply", "mailerdaemon", "postmaster", "bounce"} {
		if strings.HasPrefix(local, prefix) {
			return true
		}
	}
	return false
}

// routineSummary is the record's summary: the message as it was sent.
func routineSummary(arguments routineArguments) string {
	return fmt.Sprintf("Routine reply, sent without asking\nTo: %s\nSubject: %s\n\n%s", arguments.To, arguments.Subject, arguments.Body)
}

// routineSend sends one checked routine reply for the approval service's unattended entry point,
// counting it against its limits atomically with its start.
type routineSend struct {
	service *Service
	reply   routineReply
}

func (r routineSend) ActionTools() []string { return []string{toolReply} }

func (r routineSend) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	s := r.service
	tagged, found := s.address(record.PrincipalID)
	if !found {
		return approval.Outcome{}, sendFailure(ctx, "no_address", sendNoAddress)
	}
	if !s.Available() {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: s.Unavailable()}
	}
	// Both switches are read again at the send, since the run that chose to reply can take minutes.
	for _, sw := range []integration.Switch{Switch, RoutineSwitch} {
		enabled, err := s.switches.Enabled(ctx, record.PrincipalID, sw)
		if err != nil {
			return approval.Outcome{}, s.executionFailure(ctx, record.PrincipalID, err)
		}
		if !enabled {
			return approval.Outcome{}, &approval.ActionError{Reason: routineOffReason, Text: sw.Name + " is off for this user."}
		}
	}
	now := s.now()
	day := now.In(s.location)
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, s.location)
	verdict, err := s.repository.StartRoutineReply(ctx, RoutineStart{
		PrincipalID: record.PrincipalID, ThreadID: r.reply.threadID, SendID: record.ID,
		DayStart: dayStart, DayEnd: dayStart.AddDate(0, 0, 1), DailyLimit: routineDailyLimit, SendLimit: s.mailbox.DailySendLimit, At: now,
	})
	if err != nil {
		return approval.Outcome{}, s.executionFailure(ctx, record.PrincipalID, err)
	}
	switch verdict {
	case RoutineThreadReplied:
		return approval.Outcome{}, &approval.ActionError{Reason: limitThread, Text: "This thread already had its one routine reply."}
	case RoutineDailyLimit:
		return approval.Outcome{}, &approval.ActionError{Reason: "limit_day", Text: fmt.Sprintf("This user already had %d routine replies today.", routineDailyLimit)}
	case RoutineSendLimit:
		return approval.Outcome{}, &approval.ActionError{Reason: "limit_send", Text: fmt.Sprintf("This user reached the assistant mailbox's limit of %d emails a day.", s.mailbox.DailySendLimit)}
	}
	arguments := sendArguments{To: []string{r.reply.arguments.To}, Subject: r.reply.arguments.Subject, Body: r.reply.arguments.Body}
	raw := s.compose(arguments, tagged, r.reply.answered, now)
	sent, err := s.deliver(ctx, record.PrincipalID, raw, r.reply.threadID)
	if err != nil {
		return approval.Outcome{}, err
	}
	// The message went out; failing to record it only costs reply routing.
	if err := s.repository.RecordSent(context.WithoutCancel(ctx), sent.ID, sent.ThreadID, record.PrincipalID, now, now); err != nil {
		s.logger.ErrorContext(ctx, "assistant_mail.sent.record_failed", "principal_id", record.PrincipalID, "error", err)
	}
	return approval.Outcome{Text: fmt.Sprintf(sendDoneModel, sent.ID), UserText: routineDone.Format(ctx)}, nil
}
