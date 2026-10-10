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
	ConversationPrivacyReply = "Your messages go to your own model subscription. Conversation history stays on the home system, readable only by your assistant; /new starts fresh. Integrations can be turned off in /settings."
	CancelledReply           = "Stopped the current response."
	NothingToCancelReply     = "There is no response to cancel."
	NewSessionReply          = "Started a fresh conversation."
	NoSessionReply           = "There is no conversation to reset yet."
	HelpReply                = "Available commands: /status, /privacy, /cancel, /new, /sessions, /settings. You can otherwise talk to me normally, and reply to one of my earlier answers to continue that conversation."

	// privacyItem starts each line /privacy adds below its opening paragraph.
	privacyItem = "\n• "

	AttachmentPrivacyLine = "Photos and files you send are kept until /new."
	// Asking for a file uploads the whole file to Telegram, which reading one as text does not.
	SentFilePrivacyLine = "A NAS file reaches Telegram only when you ask for it."
	// Only the search stays with the provider the conversation already uses, so each other place a
	// web request goes is named.
	WebPrivacyLine         = "Web: searches go to your model provider, pages are fetched from home, and places go to Open-Meteo for weather."
	NSPrivacyLine          = "Trains: stations and times go to NS."
	BusPrivacyLine         = "Buses: stops go to OVapi over plain HTTP."
	BusPlanningPrivacyLine = "Planned trips go to Transitous."

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

func registeredCommands(conversationEnabled, settingsEnabled, jobsEnabled bool) []BotCommand {
	commands := []BotCommand{
		{Command: "status", Description: "Show whether Home Agent is online"},
		{Command: "privacy", Description: "Show the current privacy mode"},
	}
	if conversationEnabled {
		commands = append(
			commands,
			BotCommand{Command: "cancel", Description: "Stop the current response"},
			BotCommand{Command: "new", Description: "Start a fresh conversation"},
			BotCommand{Command: "sessions", Description: "Return to a recent conversation"},
		)
	}
	if jobsEnabled {
		commands = append(commands, BotCommand{Command: "jobs", Description: "Show and manage your reminders and scheduled jobs"})
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
