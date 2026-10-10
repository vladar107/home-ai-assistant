package conversation

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
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

func reconsiderMessage(sentAt time.Time) string {
	return ReconsiderMessage + " The reply was sent at " + sentAt.Format("15:04 on Monday 2 January 2006") + "."
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
// 👎 on a reply starts one reconsider turn in its conversation, keyed by the reply's request so a
// reply sent in several messages starts at most one. The turn is enqueued before the key is
// claimed: enqueuing is idempotent per Telegram update, so a redelivered update finds its own
// request instead of starting a second, and a principal with a turn pending claims nothing, so a
// later 👎 can still start one.
func (s *Service) React(ctx context.Context, input ReactionInput) (ReactionResult, error) {
	_, requestID, sentAt, found, err := s.repository.ReplyOrigin(ctx, input.PrincipalID, input.MessageID)
	if err != nil || !found {
		return ReactionResult{}, err
	}
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
		ChatID:            input.ChatID,
		Message:           reconsiderMessage(s.local(sentAt)),
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
