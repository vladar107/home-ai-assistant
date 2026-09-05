package telegram

import "strings"

const (
	StatusReply              = "Home Agent is online. Telegram access is active. This command does not use a model."
	ConversationStatusReply  = StatusReply + " Natural-language conversation is enabled."
	PrivacyReply             = "Privacy mode is strict-local. Local file and photo content stays on the home system unless you explicitly approve a scoped disclosure. This command does not use a model."
	ConversationPrivacyReply = "Privacy mode is strict-local for integrations. Conversation text is sent to the configured model and Pi stores its history locally in your isolated session. Use /new to start a fresh session. This command does not use a model."
	CancelledReply           = "Stopped the current response."
	NothingToCancelReply     = "There is no response to cancel."
	NewSessionReply          = "Started a fresh conversation."
	NoSessionReply           = "There is no conversation to reset yet."
	HelpReply                = "Available commands: /status, /privacy, /cancel, /new. You can otherwise talk to me normally."
)

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
