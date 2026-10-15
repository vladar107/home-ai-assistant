package conversation

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// ReconsiderMessage starts the turn that a 👎 on a reply asks for, followed by when that reply was
// sent so the model finds it among the session's turns. It quotes no reply text: none is stored,
// and the turn's message is trusted as the user's instruction.
const ReconsiderMessage = "[system] The user reacted 👎 to one of your replies, marking it as wrong or unhelpful. " +
	"Look at it again: say briefly what was wrong and give a better answer, or ask what they expected if it is unclear."

// Classes of a user's reaction to a reply. Startup creates each class's series at zero from
// FeedbackClasses, so a new class belongs there too.
const (
	FeedbackPositive = "positive"
	FeedbackNegative = "negative"
	FeedbackOther    = "other"
)

func FeedbackClasses() []string {
	return []string{FeedbackPositive, FeedbackNegative, FeedbackOther}
}

// Outcomes of a reaction for its reconsider turn.
const (
	ReconsiderStarted = "started"
	ReconsiderBusy    = "busy"
	ReconsiderAlready = "already"
)

var positiveEmojis = []string{"👍", "❤", "🔥", "👌", "🎉", "🙏"}

// ownMessage reports whether a turn's message is ours rather than the user's, so it is neither the
// request actions are judged against, nor a message to hint tools for or react to.
func ownMessage(message string) bool {
	return message == ContinueMessage || strings.HasPrefix(message, ReconsiderMessage)
}

// reconsiderLesson is the reconsider message's second step while the user lets it learn from 👎.
const reconsiderLesson = " Then, if the mistake would recur because of a preference, a habit of the user's, or a rule you should follow, " +
	"propose one memory instruction for it; otherwise propose nothing and do not mention memory."

func reconsiderMessage(sentAt time.Time, learn bool) string {
	message := ReconsiderMessage
	if learn {
		message += reconsiderLesson
	}
	return message + " The reply was sent at " + sentAt.Format("15:04 on Monday 2 January 2006") + "."
}

// learnsFromFeedback reports whether a reconsider turn may propose a lesson. A failed lookup
// leaves the step out; the memory tools refuse the lesson then too.
func (s *Service) learnsFromFeedback(ctx context.Context, principalID string) bool {
	if s.config.Memory == nil {
		return false
	}
	learn, err := s.config.Memory.LearnsFromFeedback(ctx, principalID)
	return err == nil && learn
}

// markReconsider tells the run's tools that it looks again at the reply the request quotes. A
// failed lookup leaves the reply's time unknown.
func (s *Service) markReconsider(ctx context.Context, run *tools.Run, request Request) {
	origin, found, err := s.repository.ReplyOrigin(ctx, request.PrincipalID, request.ReplyToMessageID)
	if err != nil || !found {
		run.MarkReconsider(time.Time{})
		return
	}
	run.MarkReconsider(s.local(origin.SentAt))
}

// ReactionInput is the user's new set of reactions to a message in their private chat.
type ReactionInput struct {
	BotUserID        int64
	TelegramUpdateID int64
	PrincipalID      string
	ChatID           int64
	MessageID        int64
	// Emojis is the new reaction list; empty when the user removed their reactions.
	Emojis []string
}

type ReactionResult struct {
	// Recorded is false when the message is not one of the principal's delivered replies.
	Recorded bool
	// Class is the feedback kept, or "" when it was removed.
	Class string
	// Reconsider is what became of a negative reaction's reconsider turn, or "" for any other.
	Reconsider string
}

// React keeps the user's latest reaction to one of their delivered replies as feedback. The first
// 👎 on a reply starts one reconsider turn in its conversation and the tab it was sent to, keyed by the reply's request so a
// reply sent in several messages starts at most one. The turn is enqueued before the key is
// claimed: enqueuing is idempotent per Telegram update, so a redelivered update finds its own
// request instead of starting a second, and a principal with a turn pending claims nothing, so a
// later 👎 can still start one.
func (s *Service) React(ctx context.Context, input ReactionInput) (ReactionResult, error) {
	origin, found, err := s.repository.ReplyOrigin(ctx, input.PrincipalID, input.MessageID)
	if err != nil || !found {
		return ReactionResult{}, err
	}
	requestID := origin.RequestID
	class := feedbackClass(input.Emojis)
	if class == "" {
		return ReactionResult{Recorded: true}, s.repository.DeleteReplyFeedback(ctx, input.PrincipalID, input.MessageID)
	}
	if err := s.repository.SetReplyFeedback(ctx, input.PrincipalID, input.MessageID, class, input.Emojis[0]); err != nil {
		return ReactionResult{}, err
	}
	result := ReactionResult{Recorded: true, Class: class}
	if class != FeedbackNegative {
		s.telemetry.RecordReplyFeedback(ctx, input.PrincipalID, class)
		return result, nil
	}

	key := requestID
	if key == "" {
		key = "message:" + strconv.FormatInt(input.MessageID, 10)
	}
	reconsidered, err := s.repository.ReplyReconsidered(ctx, input.PrincipalID, key)
	if err != nil {
		return ReactionResult{}, err
	}
	if reconsidered {
		s.telemetry.RecordReplyFeedback(ctx, input.PrincipalID, class)
		result.Reconsider = ReconsiderAlready
		return result, nil
	}
	err = s.Enqueue(ctx, EnqueueInput{
		BotUserID:         input.BotUserID,
		TelegramUpdateID:  input.TelegramUpdateID,
		TelegramMessageID: input.MessageID,
		PrincipalID:       input.PrincipalID,
		ThreadID:          origin.ThreadID,
		ChatID:            input.ChatID,
		Message:           reconsiderMessage(s.local(origin.SentAt), s.learnsFromFeedback(ctx, input.PrincipalID)),
		ReplyToMessageID:  input.MessageID,
	})
	if errors.Is(err, ErrBusy) {
		s.telemetry.RecordReplyFeedback(ctx, input.PrincipalID, class)
		s.telemetry.RecordReconsider(ctx, input.PrincipalID, ReconsiderBusy)
		result.Reconsider = ReconsiderBusy
		return result, nil
	}
	if err != nil {
		return ReactionResult{}, err
	}
	claimed, err := s.repository.ClaimReplyReconsider(ctx, input.PrincipalID, key)
	if err != nil {
		return ReactionResult{}, err
	}
	s.telemetry.RecordReplyFeedback(ctx, input.PrincipalID, class)
	if claimed {
		s.telemetry.RecordReconsider(ctx, input.PrincipalID, ReconsiderStarted)
	}
	result.Reconsider = ReconsiderStarted
	return result, nil
}

// feedbackClass is the class of a reaction list, or "" for an empty one.
func feedbackClass(emojis []string) string {
	if len(emojis) == 0 {
		return ""
	}
	class := FeedbackOther
	for _, emoji := range emojis {
		emoji = strings.ReplaceAll(emoji, "\uFE0F", "")
		switch {
		case emoji == "👎":
			return FeedbackNegative
		case slices.Contains(positiveEmojis, emoji):
			class = FeedbackPositive
		}
	}
	return class
}

// replyAsReaction reports whether a chat turn's reply, made only of emoji, was answered with a
// reaction instead, so it goes unsent. The model is told to react rather than write emoji but does
// not always do so; a failed reaction leaves the reply to be sent.
func (s *Service) replyAsReaction(ctx context.Context, principalID, reply string) bool {
	if s.config.Reactions == nil {
		return false
	}
	emoji, ok := onlyEmoji(reply)
	if !ok {
		return false
	}
	reacted, err := s.config.Reactions.ReplyAsReaction(ctx, principalID, emoji)
	if err != nil {
		s.telemetry.ContextLogger(ctx, s.logger).Warn(
			"conversation.reply_reaction.failed",
			"principal_id", principalID,
		)
		return false
	}
	return reacted
}

const zeroWidthJoiner = '‍'

// onlyEmoji returns the first emoji of a text made only of emoji and the space between them, or
// false for any other text.
func onlyEmoji(text string) (string, bool) {
	var first strings.Builder
	seen, firstDone, joined := false, false, false
	for _, r := range strings.TrimSpace(text) {
		switch {
		case unicode.IsSpace(r):
			firstDone = firstDone || seen
			joined = false
		case emojiModifier(r):
			if !seen {
				return "", false
			}
			if !firstDone {
				first.WriteRune(r)
			}
			joined = r == zeroWidthJoiner
		case pictographic(r):
			if seen && !joined {
				firstDone = true
			}
			seen, joined = true, false
			if !firstDone {
				first.WriteRune(r)
			}
		default:
			return "", false
		}
	}
	return first.String(), seen
}

// emojiModifier reports whether a rune only changes the emoji before it: a variation selector, a
// skin tone, a joiner, a keycap, or a subdivision flag's tag.
func emojiModifier(r rune) bool {
	return r == '︎' || r == '️' || r == zeroWidthJoiner || r == '⃣' ||
		(r >= 0x1F3FB && r <= 0x1F3FF) || (r >= 0xE0020 && r <= 0xE007F)
}

// pictographic reports whether a rune starts an emoji, judged by the blocks that hold them. Those
// blocks also hold a few plain symbols; a reply of only those is treated as emoji too.
func pictographic(r rune) bool {
	return (r >= 0x1F000 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF) ||
		(r >= 0x2300 && r <= 0x23FF) || (r >= 0x2B00 && r <= 0x2BFF)
}
