package telegram

import "strings"

const (
	StatusReply  = "Home Agent is online. Telegram access is active. This command does not use a model."
	PrivacyReply = "Privacy mode is strict-local. Local file and photo content stays on the home system unless you explicitly approve a scoped disclosure. This command does not use a model."
)

func CommandReply(text, botUsername string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", false
	}

	command, target, targeted := strings.Cut(fields[0], "@")
	if targeted {
		if target == "" || botUsername == "" || !strings.EqualFold(target, strings.TrimPrefix(botUsername, "@")) {
			return "", false
		}
	}

	switch command {
	case "/status":
		return StatusReply, true
	case "/privacy":
		return PrivacyReply, true
	default:
		return "", false
	}
}
