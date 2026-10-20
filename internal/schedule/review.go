package schedule

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	ReviewName = "Weekly review"
	// ReviewCron is Sunday at 18:00 in the job's zone.
	ReviewCron = "0 18 * * 0"
	// ReviewType is the job type of the weekly review, whose handler lives in the memory package.
	ReviewType = "memory.review"
)

// ReviewJobID is the principal's weekly review job ID, so the job is found and created once however
// often the switch is pressed.
func ReviewJobID(principalID string) string {
	sum := sha256.Sum256([]byte("weekly-review\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}
