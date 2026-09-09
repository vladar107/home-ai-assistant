package telegram

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
)

const (
	StatusReply              = "Home Agent is online. Telegram access is active. This command does not use a model."
	ConversationStatusReply  = StatusReply + " Natural-language conversation is enabled."
	PrivacyReply             = "Privacy mode is strict-local. Local file and photo content stays on the home system unless you explicitly approve a scoped disclosure. This command does not use a model."
	ConversationPrivacyReply = "Privacy mode is strict-local for integrations. Conversation text is sent to the configured model and Pi stores its history locally in your isolated session. Use /new to start a fresh session. This command does not use a model."
	CancelledReply           = "Stopped the current response."
	NothingToCancelReply     = "There is no response to cancel."
	NewSessionReply          = "Started a fresh conversation."
	NoSessionReply           = "There is no conversation to reset yet."
	HelpReply                = "Available commands: /status, /privacy, /cancel, /new, /settings. You can otherwise talk to me normally."

	AttachmentPrivacyLine = "Photos and files you send stay on the home system for the current conversation and are removed when you start a new one with /new."
	// Asking for a file uploads the whole file to Telegram, which reading one as text does not.
	SentFilePrivacyLine = "A NAS file you ask for is uploaded to Telegram to reach you; nothing is sent to this chat unless you ask for it."

	UnsupportedAudioReply      = "I cannot listen to voice messages or audio yet. Send text, a photo, or a file and I will work with that."
	UnsupportedVideoReply      = "I cannot watch video yet. Send text, a photo, or a file and I will work with that."
	UnsupportedStickerReply    = "I cannot read stickers. Send text, a photo, or a file and I will work with that."
	UnsupportedAttachmentReply = "I cannot accept photos or files in this conversation."
	AttachmentUnavailableReply = "I could not download that from Telegram. Please send it again."
	ConversationFullReply      = "This conversation is already holding as many files as I can keep. Use /new to start a fresh one."
)

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func registeredCommands(conversationEnabled, settingsEnabled bool) []BotCommand {
	commands := []BotCommand{
		{Command: "status", Description: "Show whether Home Agent is online"},
		{Command: "privacy", Description: "Show the current privacy mode"},
	}
	if conversationEnabled {
		commands = append(
			commands,
			BotCommand{Command: "cancel", Description: "Stop the current response"},
			BotCommand{Command: "new", Description: "Start a fresh conversation"},
		)
	}
	if settingsEnabled {
		commands = append(commands, BotCommand{Command: "settings", Description: "Show and change your settings"})
	}
	return commands
}

func ConnectInstructions(ceremony connection.Ceremony) string {
	minutes := int(math.Ceil(time.Until(ceremony.ExpiresAt).Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf(
		"Open %s and enter the code %s. The code expires in %d minutes. I will confirm here once your subscription is connected.",
		ceremony.VerificationURL, ceremony.UserCode, minutes,
	)
}

func ModelConnectedLine(status connection.Status) string {
	suffix := status.AccountID
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	return fmt.Sprintf(
		"Model subscription: ChatGPT account ending in %s, access token expires %s.",
		suffix, status.ExpiresAt.UTC().Format(time.RFC3339),
	)
}

func ParseCommand(text, botUsername string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false
	}

	command, target, targeted := strings.Cut(fields[0], "@")
	if targeted {
		if target == "" || botUsername == "" || !strings.EqualFold(target, strings.TrimPrefix(botUsername, "@")) {
			return "", false
		}
	}
	return strings.TrimPrefix(strings.ToLower(command), "/"), true
}
