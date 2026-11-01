package telegram

import (
	"cmp"
	"context"
	"strings"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/provenance"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// WhyController explains what one of the bot's messages was based on, from the record its turn kept.
type WhyController interface {
	// Explain reports what became of the question; when started, the answer follows in the
	// background.
	Explain(ctx context.Context, request provenance.Request) (provenance.Outcome, error)
}

// provenanceQuestions are the short replies that ask where a message came from, which are answered
// from the message's record. A bare "why?" asks about the content as often as about its sources, so
// it goes to the conversation; /why is the explicit way to ask for the record.
var provenanceQuestions = map[string]bool{
	"how do you know": true, "how do you know that": true, "where did you get that": true, "where is that from": true,
	"what is that based on": true, "what was that based on": true, "what's that based on": true,
	"source": true, "sources": true, "what is the source": true, "what are your sources": true,
	"откуда ты знаешь": true, "откуда ты это знаешь": true, "откуда это": true, "откуда инфа": true, "откуда информация": true,
	"на чём это основано": true, "на чем это основано": true, "источник": true, "источники": true,
	"hoe weet je dat": true, "waar komt dat vandaan": true, "waar heb je dat vandaan": true, "wat is de bron": true, "bron": true, "bronnen": true,
}

// isProvenanceQuestion reports whether a message is only one of the short questions about where a
// message came from, whatever its case, spacing, or the punctuation and emoji after it.
func isProvenanceQuestion(text string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
	normalized = strings.TrimRightFunc(normalized, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) || unicode.Is(unicode.Variation_Selector, r) || r == '‍'
	})
	return provenanceQuestions[strings.ReplaceAll(normalized, "’", "'")]
}

// processWhy answers a question about where the message replied to came from. As a command it
// always answers: how to use it, that no record is left, that the last answer is still being
// written, or, in the background, the record. A short question is handled only when a record
// exists, and otherwise goes on to the conversation.
func (s *Service) processWhy(ctx context.Context, message *Message, principalID string, command bool) (processingResult, bool, string, error) {
	replied := message.ReplyToMessage
	if replied == nil || replied.ForumTopicCreated != nil || replied.MessageID <= 0 {
		if !command {
			return processingResult{}, false, "", nil
		}
		result, failure, err := s.sendCommandReply(ctx, message, principalID, "why", provenance.UsageReply, nil)
		return result, true, failure, err
	}
	outcome, err := s.why.Explain(ctx, provenance.Request{
		PrincipalID: principalID,
		ChatID:      message.Chat.ID,
		ThreadID:    thread.From(ctx),
		MessageID:   message.MessageID,
		RepliedID:   replied.MessageID,
		RepliedText: cmp.Or(replied.Text, replied.Caption),
	})
	switch {
	case err != nil:
		return processingResult{}, true, "explain_message", err
	case outcome == provenance.Busy:
		result, failure, err := s.sendCommandReply(ctx, message, principalID, "why", provenance.BusyReply, nil)
		return result, true, failure, err
	case outcome == provenance.NoRecord && !command:
		return processingResult{}, false, "", nil
	case outcome == provenance.NoRecord:
		result, failure, err := s.sendCommandReply(ctx, message, principalID, "why", provenance.NoRecordReply, nil)
		return result, true, failure, err
	}
	return processingResult{event: "telegram.why.started", principalID: principalID, command: "why"}, true, "", nil
}
