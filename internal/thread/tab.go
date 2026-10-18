package thread

import "time"

// Who chose a tab's name. A fixed name never follows the tab's topic.
const (
	NamedByBot   = "bot"
	NamedByUser  = "user"
	NamedByFixed = "fixed"
)

// Tab is one tab of a principal's chat as the registry knows it.
type Tab struct {
	ThreadID int64
	Name     string
	NamedBy  string
	// IconCustomEmojiID is the icon the bot gave the tab, or "".
	IconCustomEmojiID string
	// NamedSessionID is the conversation the bot last named the tab after, or "".
	NamedSessionID string
	Confirmed      bool
	CreatedAt      time.Time
}
