package assistantmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/connectors/gmail"
	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolSend = "assistant_mail_send"

	maximumRecipients   = 5
	maximumSubjectRunes = 200
	// maximumBodyRunes keeps a body short enough that its proposal shows the whole message in one
	// chat message; long headers or many recipients leave less room, which the proposal checks.
	maximumBodyRunes    = 2000
	maximumAddressBytes = 254
	maximumReferences   = 10
	intentNameRunes     = 80

	bodyLimit = "at most about 2,000 characters"
)

// sendDoneModel is the outcome the model reads; only the model needs the message id.
const sendDoneModel = "Sent from the assistant mailbox (message id %s)."

const sendGuidance = "assistant_mail_send proposes an email from this user's assistant address; the user sees and confirms the exact message, so never claim it was sent. " +
	"To answer a message, pass its id as reply_to_id. " +
	"Write to outsiders politely, concisely and businesslike, in the language they wrote in. Sign with your own name, %q, and that you are the assistant to %s, written in that same language (for example \"Nova, assistant to Alice\"). " +
	"Never claim or imply being human, and never invent a place, a body, calls or meetings of your own. " +
	"Asked whether you are an AI, a bot or a real person, say plainly that you are an AI assistant writing for %[2]s."

var sendDefinition = tools.Definition{
	Name:        toolSend,
	Description: "Propose an email from this user's assistant address, new or a reply to one of their messages. Nothing is sent until the user confirms the exact message.",
	Risk:        tools.RiskWrite,
	Activity:    tools.ActivityMail,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"to":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":5,"description":"Plain addresses, such as name@example.com."},` +
		`"cc":{"type":"array","items":{"type":"string"},"maxItems":4,"description":"Copy addresses; at most 5 recipients in all."},` +
		`"subject":{"type":"string","description":"Up to 200 characters."},` +
		`"body":{"type":"string","description":"Plain text, signed, ` + bodyLimit + `."},` +
		`"reply_to_id":{"type":"string","description":"The id from assistant_mail_list of the message this answers."}},` +
		`"required":["to","subject","body"],"additionalProperties":false}`),
}

var (
	// sendableAddress is the only address form a send takes: no display name, no quoting, and none
	// of the characters header syntax or chat markup give a meaning to.
	sendableAddress = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
	// messageIDPattern reads message ids out of a stranger's Message-ID and References headers as
	// printable ASCII without spaces or angle brackets, so nothing else of those headers is copied.
	messageIDPattern = regexp.MustCompile(`<[!-;=?-~]{1,250}>`)
)

// Proposer is the core approval service as the mailbox sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

// ExposureChecker reports whether the calling conversation or run has read content written outside
// the household.
type ExposureChecker interface {
	External(ctx context.Context, principalID string) bool
}

var (
	_ approval.Executor = (*Service)(nil)

	sendTitle        = locale.T("Assistant mailbox · send email", "Почта ассистента · отправка письма")
	replyTitle       = locale.T("Assistant mailbox · reply", "Почта ассистента · ответ")
	markExternal     = locale.T("⚠️ This conversation has read outside content, such as mail or web pages: check that the email says only what you want.", "⚠️ В этом разговоре читался внешний текст, например письма или страницы: проверь, что в письме только то, что ты хочешь.")
	markNewInThread  = locale.T("⚠️ Not in the thread being answered: %s.", "⚠️ Не участвует в переписке, на которую это ответ: %s.")
	markFirstContact = locale.T("⚠️ First email to: %s.", "⚠️ Первое письмо на адрес: %s.")
	markReplyTo      = locale.T("⚠️ The message answered asks for replies to %s, not to its sender %s; %s.", "⚠️ Письмо, на которое это ответ, просит отвечать на %s, а не отправителю %s; %s.")
	matchesReplyTo   = locale.T("%s is its Reply-To", "%s — адрес из Reply-To")
	matchesFrom      = locale.T("%s is its sender", "%s — адрес отправителя")
	matchesNeither   = locale.T("no recipient is either", "ни один получатель не совпадает ни с одним из них")
	unusualAddress   = locale.T("an address with unusual characters", "адрес с необычными символами")

	sendDone        = locale.T("Sent from the assistant mailbox.", "Отправлено из почты ассистента.")
	sendUnknown     = locale.T("The email may have been sent: Gmail did not confirm it. It was not sent again; check the thread in the assistant mailbox before sending it once more.", "Письмо могло уйти: Gmail не подтвердил отправку. Повторно оно не отправлялось; проверь переписку в почте ассистента, прежде чем отправлять ещё раз.")
	sendRefused     = locale.T("Gmail refused the email; nothing was sent.", "Gmail отклонил письмо; ничего не отправлено.")
	sendFailed      = locale.T("The assistant mailbox could not be reached; nothing was sent.", "Почта ассистента недоступна; ничего не отправлено.")
	sendLimit       = locale.T("Not sent: this user has reached the assistant mailbox's limit of %d emails a day.", "Не отправлено: исчерпан дневной лимит почты ассистента — %d писем.")
	sendInvalid     = locale.T("Not sent: %s", "Не отправлено: %s")
	sendUnsupported = locale.T("This action is not supported.", "Это действие не поддерживается.")
	sendNoAddress   = locale.T("Not sent: this user has no address on the assistant mailbox.", "Не отправлено: у этого пользователя нет адреса в почте ассистента.")
)

type sendArguments struct {
	To        []string `json:"to"`
	Cc        []string `json:"cc,omitempty"`
	Subject   string   `json:"subject"`
	Body      string   `json:"body"`
	ReplyToID string   `json:"reply_to_id,omitempty"`
}

func (a sendArguments) recipients() []string {
	return append(slices.Clone(a.To), a.Cc...)
}

// refusal is a send its checks refuse, with the reason for the model.
type refusal struct{ text string }

func (r refusal) Error() string { return r.text }

// replied is what a reply needs of the message it answers.
type replied struct {
	threadID   string
	subject    string
	messageID  string
	references []string
	from       []string
	replyTo    []string
	// participants are the lowercase addresses of the message's From, To, Cc and Reply-To.
	participants map[string]bool
}

var repliedHeaders = []string{"From", "To", "Cc", "Reply-To", "Subject", "Message-ID", "References"}

// proposeSend checks a send and records it for the user's confirmation; nothing reaches Gmail but
// the read of the message a reply answers.
func (s *Service) proposeSend(ctx context.Context, principalID string, raw json.RawMessage) tools.Result {
	if s.proposer == nil {
		return tools.Result{Text: "Sending from the assistant mailbox is not available.", IsError: true}
	}
	var arguments sendArguments
	if err := tools.DecodeArguments(raw, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}
	}
	if err := s.validateSend(&arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}
	}
	var thread *replied
	if arguments.ReplyToID != "" {
		var err error
		if thread, err = s.replied(ctx, principalID, arguments.ReplyToID); err != nil {
			return s.sendToolFailure(ctx, principalID, err)
		}
		arguments.Subject = replySubject(arguments.Subject, thread.subject)
	}
	summary, err := s.sendSummary(ctx, principalID, arguments, thread)
	if err != nil {
		return s.sendToolFailure(ctx, principalID, err)
	}
	if !approval.SummaryFits(summary) {
		return tools.Result{Text: "The email is too long to show the user in full for confirmation: the body can hold " + bodyLimit + ", fewer with a long subject or many recipients. Shorten it.", IsError: true}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return tools.Result{Text: "The email could not be proposed.", IsError: true}
	}
	intent := approval.Intent{Verb: "send", TargetKind: "email", TargetName: clip.Runes(arguments.Subject, intentNameRunes)}
	if thread != nil {
		// A reply's subject is the thread's, which a stranger wrote.
		intent = approval.Intent{Verb: "reply to", TargetKind: "email"}
	}
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolSend, Arguments: encoded, Summary: summary, Intent: intent}); err != nil {
		if approval.Refused(err) {
			return tools.Result{Text: err.Error(), IsError: true}
		}
		s.logger.WarnContext(ctx, "assistant_mail.proposal.failed", "principal_id", principalID, "error", err)
		return tools.Result{Text: "The email could not be proposed.", IsError: true}
	}
	return tools.Result{Text: approval.ProposedText}
}

func (s *Service) sendToolFailure(ctx context.Context, principalID string, err error) tools.Result {
	var refused refusal
	if errors.As(err, &refused) {
		return tools.Result{Text: refused.text, IsError: true}
	}
	return s.toolFailure(ctx, principalID, toolSend, err)
}

// validateSend checks the arguments a send takes, at the proposal and again before it runs, and
// puts them in the form that is shown and sent.
func (s *Service) validateSend(arguments *sendArguments) error {
	if len(arguments.To) == 0 {
		return refusal{"to needs at least one address"}
	}
	if len(arguments.To)+len(arguments.Cc) > maximumRecipients {
		return refusal{fmt.Sprintf("an email takes at most %d recipients in to and cc together", maximumRecipients)}
	}
	seen := make(map[string]bool, maximumRecipients)
	for _, list := range [][]string{arguments.To, arguments.Cc} {
		for index, value := range list {
			address, err := s.checkAddress(value)
			if err != nil {
				return err
			}
			lower := strings.ToLower(address)
			if seen[lower] {
				return refusal{fmt.Sprintf("%s is listed twice; name each recipient once", address)}
			}
			seen[lower] = true
			list[index] = address
		}
	}
	arguments.Subject = strings.TrimSpace(arguments.Subject)
	if !utf8.ValidString(arguments.Subject) || strings.ContainsFunc(arguments.Subject, isControl) {
		return refusal{"the subject must be one line of text without control characters"}
	}
	if n := utf8.RuneCountInString(arguments.Subject); n < 1 || n > maximumSubjectRunes {
		return refusal{fmt.Sprintf("the subject must be 1 to %d characters", maximumSubjectRunes)}
	}
	arguments.Body = strings.ReplaceAll(arguments.Body, "\r\n", "\n")
	if !utf8.ValidString(arguments.Body) || strings.ContainsFunc(arguments.Body, func(r rune) bool { return r != '\n' && r != '\t' && isControl(r) }) {
		return refusal{"the body must be plain UTF-8 text without control characters"}
	}
	if strings.TrimSpace(arguments.Body) == "" || utf8.RuneCountInString(arguments.Body) > maximumBodyRunes {
		return refusal{"the body must hold text, " + bodyLimit}
	}
	arguments.ReplyToID = strings.TrimSpace(arguments.ReplyToID)
	return nil
}

func isControl(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' '
}

// checkAddress returns the plain address a recipient names, refusing a display name, anything a
// header could be smuggled in with, and the assistant mailbox's own address.
func (s *Service) checkAddress(value string) (string, error) {
	if strings.ContainsFunc(value, isControl) {
		return "", refusal{"an address must not hold line breaks or control characters"}
	}
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || (value != parsed.Address && value != "<"+parsed.Address+">") ||
		len(parsed.Address) > maximumAddressBytes || !sendableAddress.MatchString(parsed.Address) {
		return "", refusal{fmt.Sprintf("%q is not a plain address such as name@example.com", clip.Runes(value, 80))}
	}
	if s.own(parsed.Address) {
		return "", refusal{"the assistant mailbox cannot send to its own address"}
	}
	return parsed.Address, nil
}

// own reports whether the address reaches the assistant mailbox, with or without a tag.
func (s *Service) own(address string) bool {
	address = strings.ToLower(address)
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return false
	}
	local, _, _ := strings.Cut(address[:at], "+")
	return gmailBase(local, address[at+1:]) == s.base
}

// replied reads the message a reply answers, which must be one of the principal's own routed
// messages and not spam.
func (s *Service) replied(ctx context.Context, principalID, id string) (*replied, error) {
	if id == "" || len(id) > 64 || strings.ContainsAny(id, "/?#") {
		return nil, refusal{notFoundText}
	}
	route, found, err := s.repository.Route(ctx, principalID, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, refusal{notFoundText}
	}
	if route.Spam {
		return nil, refusal{"That message is in spam, so it cannot be answered from here."}
	}
	var message gmail.Message
	err = s.call(ctx, func(access string) error {
		var err error
		message, err = s.gmail.MessageHeaders(ctx, access, id, repliedHeaders)
		return err
	})
	if isGone(err) {
		return nil, refusal{goneText}
	}
	if err != nil {
		return nil, err
	}
	thread := &replied{threadID: message.ThreadID, participants: make(map[string]bool)}
	if thread.threadID == "" {
		thread.threadID = route.ThreadID
	}
	if subjects := message.HeaderValues("Subject"); len(subjects) > 0 {
		thread.subject = subjects[0]
		if decoded, err := addressParser.WordDecoder.DecodeHeader(subjects[0]); err == nil {
			thread.subject = decoded
		}
	}
	for _, value := range message.HeaderValues("Message-ID") {
		if id := messageIDPattern.FindString(value); id != "" {
			thread.messageID = id
			break
		}
	}
	for _, value := range message.HeaderValues("References") {
		thread.references = append(thread.references, messageIDPattern.FindAllString(value, -1)...)
	}
	if over := len(thread.references) - (maximumReferences - 1); over > 0 {
		thread.references = thread.references[over:]
	}
	read := func(name string) []string {
		var found []string
		for _, value := range message.HeaderValues(name) {
			for _, address := range addresses(value) {
				lower := strings.ToLower(address)
				found = append(found, lower)
				thread.participants[lower] = true
			}
		}
		return found
	}
	thread.from, thread.replyTo = read("From"), read("Reply-To")
	read("To")
	read("Cc")
	return thread, nil
}

// replySubject is the subject a reply goes out with: Gmail files a sent message in a thread only
// when its subject matches the thread's, so a subject that does not becomes the thread's own.
func replySubject(subject, original string) string {
	if original == "" || !utf8.ValidString(original) || strings.ContainsFunc(original, isControl) {
		return subject
	}
	candidate := "Re: " + baseSubject(original)
	if baseSubject(subject) == baseSubject(original) {
		if baseSubject(subject) != subject {
			return subject
		}
		candidate = "Re: " + subject
	}
	if utf8.RuneCountInString(candidate) > maximumSubjectRunes || baseSubject(original) == "" {
		return subject
	}
	return candidate
}

// baseSubject is the subject without its Re: and Fwd: prefixes.
func baseSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	for {
		lower := strings.ToLower(subject)
		trimmed := false
		for _, prefix := range []string{"re:", "fwd:", "fw:"} {
			if strings.HasPrefix(lower, prefix) {
				subject, trimmed = strings.TrimSpace(subject[len(prefix):]), true
				break
			}
		}
		if !trimmed {
			return subject
		}
	}
}

// sendSummary is what the user confirms: the marks that apply, and the message exactly as it will
// be sent, in a code block so nothing in it is read as formatting.
func (s *Service) sendSummary(ctx context.Context, principalID string, arguments sendArguments, thread *replied) (string, error) {
	tagged, _ := s.address(principalID)
	var builder strings.Builder
	if thread != nil {
		builder.WriteString(replyTitle.Format(ctx))
	} else {
		builder.WriteString(sendTitle.Format(ctx))
	}
	if s.exposure != nil && s.exposure.External(ctx, principalID) {
		builder.WriteString("\n" + markExternal.Format(ctx))
	}
	recipients := arguments.recipients()
	if thread != nil {
		var outside []string
		for _, recipient := range recipients {
			if !thread.participants[strings.ToLower(recipient)] {
				outside = append(outside, recipient)
			}
		}
		if len(outside) > 0 {
			builder.WriteString("\n" + markNewInThread.Format(ctx, strings.Join(outside, ", ")))
		}
		if mark := replyToMark(ctx, thread, recipients); mark != "" {
			builder.WriteString("\n" + mark)
		}
	} else {
		known, err := s.repository.KnownContacts(ctx, principalID, recipients)
		if err != nil {
			return "", err
		}
		var first []string
		for _, recipient := range recipients {
			if !slices.Contains(known, strings.ToLower(recipient)) {
				first = append(first, recipient)
			}
		}
		if len(first) > 0 {
			builder.WriteString("\n" + markFirstContact.Format(ctx, strings.Join(first, ", ")))
		}
	}
	var message strings.Builder
	fmt.Fprintf(&message, "From: %s <%s>\nReply-To: %s\nTo: %s\n", s.mailbox.DisplayName, s.mailbox.Address, tagged, strings.Join(arguments.To, ", "))
	if len(arguments.Cc) > 0 {
		fmt.Fprintf(&message, "Cc: %s\n", strings.Join(arguments.Cc, ", "))
	}
	fmt.Fprintf(&message, "Subject: %s\n\n%s", arguments.Subject, arguments.Body)
	// A fence longer than any run of backticks in the message is never closed by the message itself.
	fence := strings.Repeat("`", max(3, longestRun(message.String(), '`')+1))
	builder.WriteString("\n\n" + fence + "\n" + message.String() + "\n" + fence)
	return builder.String(), nil
}

// replyToMark says, when the message answered has a Reply-To other than its sender, which of the
// two each recipient is.
func replyToMark(ctx context.Context, thread *replied, recipients []string) string {
	if len(thread.replyTo) == 0 || len(thread.from) == 0 || slices.Equal(sortedCopy(thread.replyTo), sortedCopy(thread.from)) {
		return ""
	}
	var parts []string
	for _, recipient := range recipients {
		lower := strings.ToLower(recipient)
		switch {
		case slices.Contains(thread.replyTo, lower):
			parts = append(parts, matchesReplyTo.Format(ctx, recipient))
		case slices.Contains(thread.from, lower):
			parts = append(parts, matchesFrom.Format(ctx, recipient))
		}
	}
	matched := strings.Join(parts, ", ")
	if matched == "" {
		matched = matchesNeither.Format(ctx)
	}
	return markReplyTo.Format(ctx, shownAddress(ctx, thread.replyTo[0]), shownAddress(ctx, thread.from[0]), matched)
}

// shownAddress shows a stranger's address in a mark only when nothing in it could be read as
// formatting.
func shownAddress(ctx context.Context, address string) string {
	if sendableAddress.MatchString(address) {
		return address
	}
	return unusualAddress.Format(ctx)
}

func sortedCopy(values []string) []string {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

func longestRun(text string, r byte) int {
	longest, current := 0, 0
	for index := 0; index < len(text); index++ {
		if text[index] == r {
			current++
			longest = max(longest, current)
		} else {
			current = 0
		}
	}
	return longest
}

func (s *Service) ActionTools() []string {
	return []string{toolSend}
}

// ExecuteApproved checks a confirmed send again, counts it against the principal's daily limit, and
// sends it once. A send whose outcome Gmail leaves unknown is reported as possibly sent and never
// repeated, since Gmail's send has no idempotency key.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	if record.Tool != toolSend {
		return approval.Outcome{}, sendFailure(ctx, "unknown_tool", sendUnsupported)
	}
	tagged, found := s.address(record.PrincipalID)
	if !found {
		return approval.Outcome{}, sendFailure(ctx, "no_address", sendNoAddress)
	}
	if !s.Available() {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: s.Unavailable()}
	}
	var arguments sendArguments
	if err := tools.DecodeArguments(record.Arguments, &arguments); err != nil {
		return approval.Outcome{}, sendFailure(ctx, "invalid", sendInvalid, err.Error())
	}
	if err := s.validateSend(&arguments); err != nil {
		return approval.Outcome{}, sendFailure(ctx, "invalid", sendInvalid, err.Error())
	}
	var thread *replied
	if arguments.ReplyToID != "" {
		var err error
		if thread, err = s.replied(ctx, record.PrincipalID, arguments.ReplyToID); err != nil {
			return approval.Outcome{}, s.executionFailure(ctx, record.PrincipalID, err)
		}
	}
	now := s.now()
	day := now.In(s.location)
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, s.location)
	allowed, err := s.repository.StartSend(ctx, record.PrincipalID, record.ID, dayStart, dayStart.AddDate(0, 0, 1), s.mailbox.DailySendLimit, now)
	if err != nil {
		return approval.Outcome{}, s.executionFailure(ctx, record.PrincipalID, err)
	}
	if !allowed {
		s.logger.InfoContext(ctx, "assistant_mail.send.limited", "principal_id", record.PrincipalID)
		return approval.Outcome{}, sendFailure(ctx, "limit", sendLimit, s.mailbox.DailySendLimit)
	}
	raw := s.compose(arguments, tagged, thread, now)
	threadID := ""
	if thread != nil && thread.messageID != "" {
		threadID = thread.threadID
	}
	var sent gmail.MessageRef
	err = s.call(ctx, func(access string) error {
		var err error
		sent, err = s.gmail.Send(ctx, access, raw, threadID)
		return err
	})
	if err != nil {
		// An approval deadline that ran out before the request left fails the same way; possibly
		// sent is the safe report, since this cannot tell it from a request cut off in flight.
		if gmailErr, ok := google.ProductError(err, "Gmail"); ok && gmailErr.Class == google.ErrorUnavailable {
			s.logger.WarnContext(ctx, "assistant_mail.send.unknown", "principal_id", record.PrincipalID, "error", err)
			return approval.Outcome{}, sendFailure(ctx, "possibly_sent", sendUnknown)
		}
		if _, ok := google.ProductError(err, "Gmail"); ok {
			s.logger.WarnContext(ctx, "assistant_mail.send.refused", "principal_id", record.PrincipalID, "error", err)
			return approval.Outcome{}, sendFailure(ctx, "refused", sendRefused)
		}
		return approval.Outcome{}, s.executionFailure(ctx, record.PrincipalID, err)
	}
	// The message went out; failing to record it only costs reply routing and a first-contact mark.
	recorded := context.WithoutCancel(ctx)
	if err := s.repository.RecordSent(recorded, sent.ID, sent.ThreadID, record.PrincipalID, now, now); err != nil {
		s.logger.ErrorContext(ctx, "assistant_mail.sent.record_failed", "principal_id", record.PrincipalID, "error", err)
	}
	if err := s.repository.RecordContacts(recorded, record.PrincipalID, arguments.recipients(), now); err != nil {
		s.logger.WarnContext(ctx, "assistant_mail.contacts.record_failed", "principal_id", record.PrincipalID, "error", err)
	}
	s.logger.InfoContext(ctx, "assistant_mail.sent", "principal_id", record.PrincipalID, "reply", thread != nil)
	return approval.Outcome{Text: fmt.Sprintf(sendDoneModel, sent.ID), UserText: sendDone.Format(ctx)}, nil
}

// executionFailure reports a send that failed before anything reached Gmail's send.
func (s *Service) executionFailure(ctx context.Context, principalID string, err error) error {
	var refused refusal
	switch {
	case errors.As(err, &refused):
		return sendFailure(ctx, "invalid", sendInvalid, refused.text)
	case errors.Is(err, ErrUnavailable):
		return &approval.ActionError{Reason: "unavailable", Text: s.Unavailable()}
	}
	s.logger.WarnContext(ctx, "assistant_mail.send.failed", "principal_id", principalID, "error", err)
	return sendFailure(ctx, "failed", sendFailed)
}

func sendFailure(ctx context.Context, reason string, text locale.Text, args ...any) error {
	return &approval.ActionError{Reason: reason, Text: text.FormatIn(locale.English, args...), UserText: text.Format(ctx, args...)}
}

// compose builds the RFC 5322 message: plain UTF-8 text, quoted-printable, with CRLF line ends. A
// reply points at the message it answers, so Gmail and the other side thread it.
func (s *Service) compose(arguments sendArguments, replyTo string, thread *replied, at time.Time) []byte {
	var message bytes.Buffer
	header := func(name, value string) {
		message.WriteString(name + ": " + value + "\r\n")
	}
	header("From", (&mail.Address{Name: s.mailbox.DisplayName, Address: s.mailbox.Address}).String())
	header("Reply-To", replyTo)
	header("To", strings.Join(arguments.To, ",\r\n "))
	if len(arguments.Cc) > 0 {
		header("Cc", strings.Join(arguments.Cc, ",\r\n "))
	}
	header("Subject", encodeHeader(arguments.Subject))
	header("Date", at.In(s.location).Format(time.RFC1123Z))
	if thread != nil && thread.messageID != "" {
		header("In-Reply-To", thread.messageID)
		header("References", strings.Join(append(slices.Clone(thread.references), thread.messageID), "\r\n "))
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	message.WriteString("\r\n")
	body := quotedprintable.NewWriter(&message)
	_, _ = body.Write([]byte(arguments.Body))
	_ = body.Close()
	return message.Bytes()
}

// encodeHeader writes a header value as it is when it is plain ASCII, and otherwise as RFC 2047
// encoded words, one per folded line, so no line grows past the limit.
func encodeHeader(value string) string {
	ascii := true
	for _, r := range value {
		if r < ' ' || r > '~' {
			ascii = false
			break
		}
	}
	if ascii {
		return value
	}
	return strings.ReplaceAll(mime.BEncoding.Encode("utf-8", value), "?= =?", "?=\r\n =?")
}
