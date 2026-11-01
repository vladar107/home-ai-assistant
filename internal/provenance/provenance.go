// Package provenance answers "why?" about one of the assistant's messages: it finds the trace the
// turn that delivered the message kept, renders in code which tools ran with what outcome, what
// each looked at, whether outside or private content was involved, and which model answered, and
// adds the model's short explanation from one extra turn without tools on the owner's own
// subscription. The code-rendered part is the record; the explanation is a reconstruction.
package provenance

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/conversation/history"
	"github.com/vladar107/home-ai-assistant/internal/localtime"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/turn"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const (
	// The replies the user reads.
	NoRecordReply = "I no longer have the record of that reply."
	UsageReply    = "Reply /why to one of my messages to see what it was based on."
	CaveatLine    = "The list is the record of what ran; the explanation is my reconstruction of it, not my actual reasoning."
	FailedLine    = "I could not write an explanation just now; the list above is the record."
	BusyReply     = "I am still working out the last one you asked about; ask again once it has arrived."

	explainTimeout = 2 * time.Minute
	// maximumMessageRunes bounds how much of the asked-about message the explanation reads.
	maximumMessageRunes = 3000
	sendTimeout         = 15 * time.Second
	// matchRunes is how much of a reply's start tells it apart from another.
	matchRunes = 100
	// maximumShownCalls bounds the calls an answer lists, which the explanation turn reads too.
	maximumShownCalls = 20
)

// Store is what an explanation reads.
type Store interface {
	// TurnTraceByMessage returns the trace of the turn that delivered the principal's message.
	TurnTraceByMessage(ctx context.Context, principalID string, chatID, messageID int64) (turn.Trace, bool, error)
	// ReplySession returns the runtime session of a delivered chat reply and when it was sent.
	ReplySession(ctx context.Context, principalID string, messageID int64) (string, time.Time, bool, error)
}

// Recent is the memory of each tab's latest exchange, which still holds the user's last two
// messages before a recent reply. A request's own text is cleared once its turn is done.
type Recent interface {
	RecentExchange(principalID string, threadID int64) []turnstate.Line
}

// History reads what the runtime recorded of a conversation: the user's message before a reply.
type History interface {
	Exchanges(ctx context.Context, principalID, runtimeSessionID string, sentAt []time.Time) ([]history.Exchange, error)
}

// Explainer runs the explanation's turn on the principal's own subscription.
type Explainer interface {
	RunExplain(ctx context.Context, principalID, message string) (turn.ScheduledReply, error)
}

// Sender sends the answer as a reply to the user's question.
type Sender interface {
	SendReply(ctx context.Context, chatID, replyToMessageID int64, text string) error
}

// Request is one "why?": the user's message asking it, and the message it is about.
type Request struct {
	PrincipalID string
	ChatID      int64
	ThreadID    int64
	// MessageID is the user's question, which the answer replies to.
	MessageID int64
	// RepliedID and RepliedText are the message asked about.
	RepliedID   int64
	RepliedText string
}

// Config is what the service reads and runs on. Store, Explainer, Sender, and Logger are required;
// without Recent and History an explanation goes without the user's messages, and without a
// Location times are shown in UTC.
type Config struct {
	Store     Store
	Recent    Recent
	History   History
	Explainer Explainer
	Sender    Sender
	Logger    *slog.Logger
	Location  *time.Location
}

type Service struct {
	config Config

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	// explaining holds the principals with an answer being written; each has at most one.
	explaining map[string]bool
}

// Outcome is what became of a "why?".
type Outcome int

const (
	// NoRecord means the message has no trace, so nothing is sent.
	NoRecord Outcome = iota
	// Started means the answer is being written and follows in the background.
	Started
	// Busy means an answer to the principal's last "why?" is still being written, so nothing more
	// is started.
	Busy
)

func New(config Config) (*Service, error) {
	if config.Store == nil || config.Explainer == nil || config.Sender == nil || config.Logger == nil {
		return nil, fmt.Errorf("provenance needs a store, an explainer, a sender, and a logger")
	}
	if config.Location == nil {
		config.Location = time.UTC
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{config: config, ctx: ctx, cancel: cancel, explaining: make(map[string]bool)}, nil
}

// Explain looks up what the asked-about message was based on. With a record, the answer is written
// and sent in the background, since its explanation is a model turn, unless one for the same
// principal is still being written; otherwise nothing is sent, and the caller decides what to say.
func (s *Service) Explain(ctx context.Context, request Request) (Outcome, error) {
	trace, found, err := s.config.Store.TurnTraceByMessage(ctx, request.PrincipalID, request.ChatID, request.RepliedID)
	if err != nil || !found {
		return NoRecord, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return NoRecord, fmt.Errorf("provenance is shut down")
	case s.explaining[request.PrincipalID]:
		return Busy, nil
	}
	s.explaining[request.PrincipalID] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.explaining, request.PrincipalID)
			s.mu.Unlock()
		}()
		s.answer(thread.With(s.ctx, request.ThreadID), request, trace)
	}()
	return Started, nil
}

// Shutdown cancels the explanations still being written and waits for them to end.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown provenance: %w", ctx.Err())
	}
}

func (s *Service) answer(ctx context.Context, request Request, trace turn.Trace) {
	record := Render(trace, s.config.Location)
	text := record
	if trace.Kind != turn.TraceCode {
		text += "\n\n" + s.explanation(ctx, request, trace, record)
	}
	if ctx.Err() != nil {
		return
	}
	// The first chunk answers the question; the rest follow it unquoted.
	replyTo := request.MessageID
	for _, chunk := range conversation.SplitReply(text) {
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := s.config.Sender.SendReply(sendCtx, request.ChatID, replyTo, chunk)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.config.Logger.WarnContext(ctx, "provenance.reply.failed", "principal_id", request.PrincipalID, "kind", trace.Kind, "error", err)
			}
			return
		}
		replyTo = 0
	}
	s.config.Logger.InfoContext(ctx, "provenance.reply.sent", "principal_id", request.PrincipalID, "kind", trace.Kind, "calls", len(trace.Calls))
}

// explanation is the model's few sentences with the caveat line, or the line saying none came.
func (s *Service) explanation(ctx context.Context, request Request, trace turn.Trace, record string) string {
	turnCtx, cancel := context.WithTimeout(ctx, explainTimeout)
	defer cancel()
	reply, err := s.config.Explainer.RunExplain(turnCtx, request.PrincipalID, prompt.ExplainMessage(record, clip.Runes(request.RepliedText, maximumMessageRunes), s.earlier(ctx, request, trace)))
	if err != nil || reply.ErrorKind != "" || strings.TrimSpace(reply.Text) == "" {
		if ctx.Err() == nil {
			s.config.Logger.WarnContext(ctx, "provenance.explanation.failed", "principal_id", request.PrincipalID, "kind", trace.Kind, "error_kind", reply.ErrorKind)
		}
		return FailedLine
	}
	return strings.TrimSpace(reply.Text) + "\n\n" + CaveatLine
}

// earlier is the user's messages a chat turn answered: the last two while the tab's memory still
// holds them, which it does only while the tab's latest reply is the message asked about, and
// otherwise the one before the reply in the conversation's history. Other turns answer no message
// of the user's.
func (s *Service) earlier(ctx context.Context, request Request, trace turn.Trace) string {
	if _, chat := outbox.ChatRequest(trace.Key); !chat {
		return ""
	}
	var messages []string
	if s.config.Recent != nil {
		exchange := s.config.Recent.RecentExchange(request.PrincipalID, request.ThreadID)
		reply := slices.IndexFunc(exchange, func(line turnstate.Line) bool { return line.Role == turnstate.RoleAssistant })
		if reply >= 0 && sameText(exchange[reply].Text, request.RepliedText) {
			for _, line := range exchange[:reply] {
				messages = append(messages, line.Text)
			}
		}
	}
	if len(messages) == 0 {
		messages = s.fromHistory(ctx, request)
	}
	for index, message := range messages {
		messages[index] = clip.Runes(message, maximumMessageRunes)
	}
	return strings.Join(messages, "\n\n")
}

// fromHistory is the user's message before the reply, as the conversation's history recorded it.
func (s *Service) fromHistory(ctx context.Context, request Request) []string {
	if s.config.History == nil {
		return nil
	}
	session, sentAt, found, err := s.config.Store.ReplySession(ctx, request.PrincipalID, request.RepliedID)
	if err == nil && found {
		var exchanges []history.Exchange
		if exchanges, err = s.config.History.Exchanges(ctx, request.PrincipalID, session, []time.Time{sentAt}); err == nil && len(exchanges) == 1 && exchanges[0].User != "" {
			return []string{exchanges[0].User}
		}
	}
	if err != nil {
		s.config.Logger.WarnContext(ctx, "provenance.history.failed", "principal_id", request.PrincipalID, "error", err)
	}
	return nil
}

// sameText reports whether a kept reply and a delivered message are the same text. The message is
// one chunk of the reply as Telegram shows it, without the reply's markup, and the kept reply may
// have lost its start, so the start of either found in the other is enough.
func sameText(kept, delivered string) bool {
	kept, delivered = plainText(kept), plainText(delivered)
	if kept == "" || delivered == "" {
		return false
	}
	return strings.Contains(kept, start(delivered)) || strings.Contains(delivered, start(kept))
}

// start is the first matchRunes runes of a text.
func start(text string) string {
	if runes := []rune(text); len(runes) > matchRunes {
		return string(runes[:matchRunes])
	}
	return text
}

func plainText(text string) string {
	text = strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_`~", r) {
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

// Render is the record of a trace as the user reads it, made by code alone.
func Render(trace turn.Trace, location *time.Location) string {
	if trace.Kind == turn.TraceCode {
		lines := []string{"**What that message was based on**", "Code wrote it from the watch's rules, with no model turn:"}
		if detail := strings.TrimSpace(trace.Detail); detail != "" {
			lines = append(lines, detail)
		}
		return strings.Join(lines, "\n")
	}
	lines := []string{"**What that message was based on**", fmt.Sprintf("%s, %s, took %s.", kindLine(trace.Kind), localtime.Format(trace.StartedAt, location), roundDuration(trace.FinishedAt.Sub(trace.StartedAt)))}
	lines = append(lines, "Answered by "+modelLine(trace.Model, trace.Effort)+".")
	if len(trace.Calls) == 0 {
		lines = append(lines, "No tools ran.")
	} else {
		lines = append(lines, "Tools, in order:")
		for index, call := range trace.Calls[:min(len(trace.Calls), maximumShownCalls)] {
			lines = append(lines, callLine(index+1, call))
		}
		if hidden := len(trace.Calls) - maximumShownCalls; hidden > 0 {
			lines = append(lines, fmt.Sprintf("…and %d more.", hidden))
		}
	}
	if trace.External {
		lines = append(lines, "It read content from outside the household, such as web pages or mail.")
	} else {
		lines = append(lines, "It read no content from outside the household.")
	}
	if trace.Private {
		lines = append(lines, "It read private content, such as Notion pages or NAS files.")
	}
	return strings.Join(lines, "\n")
}

func kindLine(kind string) string {
	switch kind {
	case turn.TraceChat:
		return "A chat turn"
	case turn.TraceScheduled:
		return "A scheduled run"
	case turn.TraceTask:
		return "A background task"
	case turn.TraceCheck:
		return "A check"
	case turn.TraceAssess:
		return "The assessment of an incoming event"
	}
	return "A turn"
}

func modelLine(model, effort string) string {
	switch {
	case model == "":
		return "the default model"
	case effort == "":
		return model
	default:
		return model + " at " + effort + " effort"
	}
}

func callLine(number int, call turn.TraceCall) string {
	line := strconv.Itoa(number) + ". " + call.Tool
	if call.Label != "" {
		line += " — “" + call.Label + "”"
	}
	line += ": " + outcomeWord(call.Outcome)
	if call.Duration > 0 {
		line += ", " + roundDuration(call.Duration)
	}
	return line
}

// outcomeWord words the broker's and the runtime's outcomes for the user.
func outcomeWord(outcome string) string {
	switch outcome {
	case "success":
		return "succeeded"
	case "failed", "tool_error":
		return "failed"
	case "timeout":
		return "timed out"
	case "budget_exhausted":
		return "refused, the call budget was spent"
	case "switched_off", "not_granted", "grant_check_failed", "unknown_tool":
		return "not available"
	case "oversized":
		return "returned too much to read"
	case "unfinished":
		return "did not finish"
	}
	return strings.ReplaceAll(outcome, "_", " ")
}

func roundDuration(duration time.Duration) string {
	switch {
	case duration < time.Second:
		return "under a second"
	case duration < time.Minute:
		return strconv.Itoa(int(duration.Round(time.Second)/time.Second)) + " s"
	default:
		return duration.Round(time.Second).String()
	}
}
