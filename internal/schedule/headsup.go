package schedule

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const (
	HeadsUpName = "Heads-up"
	HeadsUpCron = "0 8,19 * * *"
	// headsUpCallBudget fits the dozen lookups its prompt asks for, which a turn's usual budget
	// would cut short and leave reported as failed checks.
	headsUpCallBudget = 20
)

// HeadsUpPrompt is the built-in instruction of the Heads-up job. It is stored with the job, so a
// later change reaches only jobs created afterwards.
const HeadsUpPrompt = "Look at the user's calendar for today and tomorrow, their jobs, watches, and saved routes, " +
	"transit disruptions on the trips those imply, the weather where they will be, Home Assistant devices that need attention " +
	"(doors left open, errors, low batteries, a full bin), health and energy trends worth knowing, and pending reminders. " +
	"Read the reports already sent today with job_reports and repeat none of them. " +
	"Write only when something needs the user's attention or action: a few short lines, most important first, " +
	"in the language of the user's memory and other jobs. A source that is off or not connected is not worth mentioning. Otherwise reply with exactly " + conversation.NothingToAdd + "."

// HeadsUpJobID is the principal's Heads-up job ID, so the job is found and created once however
// often the switch is pressed.
func HeadsUpJobID(principalID string) string {
	sum := sha256.Sum256([]byte("heads-up\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}
