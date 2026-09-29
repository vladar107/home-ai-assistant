package conversation

import (
	"context"
	"log/slog"
)

// SendCard sends the card a successful turn staged as a photo ahead of its reply, with the first
// chunk as its caption when that fits. It returns the chunks still to send and the message they
// should quote, which is none once the photo has quoted the request. A card whose photo already
// went out is not sent again, nor is the chunk its caption carried, so a retry after a failed
// chunk repeats nothing. A card that cannot be read, or a photo Telegram refuses for good, is
// dropped and the reply goes out as text alone; only a transient photo failure returns an error,
// leaving the card for the retry.
func SendCard(ctx context.Context, logger *slog.Logger, cards Cards, photos PhotoDeliverer, principalID, turn string, chatID, replyTo int64, chunks []string) ([]string, int64, error) {
	card, found, err := cards.Card(principalID, turn)
	if err != nil {
		logger.WarnContext(ctx, "conversation.card.dropped", "principal_id", principalID, "stage", "read", "error", err)
		cards.Remove(principalID, turn)
		return chunks, replyTo, nil
	}
	if !found {
		return chunks, replyTo, nil
	}
	captioned := card.Captioned
	if !card.Sent {
		caption := ""
		if len(chunks) > 0 && photos.CaptionFits(chunks[0]) {
			caption = chunks[0]
		}
		if err := photos.SendPhoto(ctx, chatID, replyTo, card.PNG, caption); err != nil {
			if isPermanentDeliveryError(err) {
				cards.Remove(principalID, turn)
				return chunks, replyTo, nil
			}
			return nil, 0, err
		}
		captioned = caption != ""
		// The photo is already in the chat, so failing here would only upload it again on the
		// retry; the rest of the reply goes out in this attempt instead.
		if err := cards.MarkSent(principalID, turn, captioned); err != nil {
			logger.WarnContext(ctx, "conversation.card.mark_sent_failed", "principal_id", principalID, "error", err)
		}
	}
	if captioned && len(chunks) > 0 {
		chunks = chunks[1:]
	}
	return chunks, 0, nil
}
