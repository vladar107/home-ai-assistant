package telegram

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const (
	// maximumRecordingBytes is the most a bot may download from Telegram.
	maximumRecordingBytes = 20 << 20
	transcriptionTimeout  = 90 * time.Second

	voiceMarker = "[Voice message, transcribed]"

	TranscriptionFailedReply = "I could not transcribe that voice message. Try again, or send it as text."
	NoSpeechReply            = "I could not hear any speech in that recording."
	RecordingTooLargeReply   = "That recording is too large for me to listen to; recordings must stay under 20 MB."
	TranscriptTooLongReply   = "That recording is too long for one message. Keep voice messages under about four minutes, or split them."
	CaptionTooLongReply      = "That recording and its caption are too long together for one message. Send the caption as a separate message, or shorten the recording."
	UsageLimitReply          = "Your ChatGPT usage limit is reached, so I cannot do that right now. Try again later."
	// VoicePrivacyLine is added to /privacy while voice messages are transcribed.
	VoicePrivacyLine = "Voice: transcribed by your own ChatGPT subscription; the recording is not kept."
)

// Transcriber turns a principal's recording into text with that principal's own subscription.
type Transcriber interface {
	Transcribe(ctx context.Context, principalID string, audio chatgpt.Audio) (string, error)
}

// recording is a voice note or audio file a message carried, before it has been downloaded.
type recording struct {
	fileID   string
	name     string
	mimeType string
	size     int64
}

func voiceRecording(message *Message) (recording, bool) {
	switch {
	case message.Voice != nil:
		return recording{fileID: message.Voice.FileID, name: "voice.ogg", mimeType: cmp.Or(message.Voice.MimeType, "audio/ogg"), size: message.Voice.FileSize}, true
	case message.Audio != nil:
		return recording{fileID: message.Audio.FileID, name: cmp.Or(strings.TrimSpace(message.Audio.FileName), "audio"), mimeType: cmp.Or(message.Audio.MimeType, "application/octet-stream"), size: message.Audio.FileSize}, true
	}
	return recording{}, false
}

// transcribe returns a recording's transcript, or the reply that answers the message instead.
// Every failure is answered rather than retried, since a retry would download and spend the
// subscription on the same recording again; only shutdown returns an error, so the update is
// processed again after a restart.
func (s *Service) transcribe(ctx context.Context, principalID string, item recording) (string, string, string, error) {
	transcript, reply, reason := s.transcribeWithin(ctx, principalID, item)
	if reply != "" && ctx.Err() != nil {
		return "", "", "", ctx.Err()
	}
	return transcript, reply, reason, nil
}

func (s *Service) transcribeWithin(parent context.Context, principalID string, item recording) (string, string, string) {
	if item.fileID == "" {
		return "", AttachmentUnavailableReply, "unavailable"
	}
	if item.size > maximumRecordingBytes {
		return "", RecordingTooLargeReply, "too_large"
	}
	ctx, cancel := context.WithTimeout(parent, transcriptionTimeout)
	defer cancel()

	reference, err := s.client.GetFile(ctx, item.fileID)
	if err != nil {
		return "", AttachmentUnavailableReply, "unavailable"
	}
	data, err := s.client.DownloadFile(ctx, reference.FilePath, maximumRecordingBytes)
	if err != nil {
		var oversized *oversizedFileError
		if errors.As(err, &oversized) {
			return "", RecordingTooLargeReply, "too_large"
		}
		return "", AttachmentUnavailableReply, "unavailable"
	}

	transcript, err := s.transcriber.Transcribe(ctx, principalID, chatgpt.Audio{FileName: item.name, MimeType: item.mimeType, Data: data})
	if err != nil {
		reply, reason := transcriptionFailure(err)
		s.telemetry.ContextLogger(ctx, s.logger).WarnContext(ctx, "telegram.voice.failed", "principal_id", principalID, "reason", reason, "error", err)
		return "", reply, reason
	}
	if transcript == "" {
		return "", NoSpeechReply, "no_speech"
	}
	return transcript, "", ""
}

// voiceTurn builds the turn's message from a transcript and the caption, or the reply that refuses
// one the conversation could not take.
func voiceTurn(transcript, caption string) (string, string, string) {
	message := voiceMarker + "\n" + transcript
	if utf8.RuneCountInString(message) > conversation.MaximumMessageRunes {
		return "", TranscriptTooLongReply, "too_long"
	}
	if caption = strings.TrimSpace(caption); caption != "" {
		message += "\n\n" + caption
	}
	if utf8.RuneCountInString(message) > conversation.MaximumMessageRunes {
		return "", CaptionTooLongReply, "caption_too_long"
	}
	return message, "", ""
}

func transcriptionFailure(err error) (string, string) {
	switch {
	case errors.Is(err, connection.ErrNotConnected):
		return conversation.NotConnectedReply, "not_connected"
	case errors.Is(err, chatgpt.ErrUnauthorized):
		return conversation.ReconnectReply, "unauthorized"
	case errors.Is(err, chatgpt.ErrUsageLimit):
		return UsageLimitReply, "usage_limit"
	case errors.Is(err, context.DeadlineExceeded):
		return TranscriptionFailedReply, "timeout"
	default:
		return TranscriptionFailedReply, "failed"
	}
}
