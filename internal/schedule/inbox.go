package schedule

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	InboxName = "Inbox intake"
	// InboxCron is every three hours between 08:00 and 22:00 in the job's zone: 08:00, 11:00,
	// 14:00, 17:00, and 20:00.
	InboxCron = "0 8-22/3 * * *"
	// InboxScanType is the job type of the inbox check, whose handler lives in the intake package.
	InboxScanType = "intake.scan"
)

// InboxJobID is the principal's inbox check job ID, so the job is found and created once however
// often the switch is pressed.
func InboxJobID(principalID string) string {
	sum := sha256.Sum256([]byte("inbox-intake\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}
