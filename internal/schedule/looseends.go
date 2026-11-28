package schedule

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	LooseEndsName = "Loose ends"
	LooseEndsCron = "20 7 * * *"
	// LooseEndScanType is the job type of the daily Loose ends run, whose handler lives in the
	// looseend package.
	LooseEndScanType = "looseend.scan"
)

// LooseEndsJobID is the principal's Loose ends job ID, so the job is found and created once however
// often the switch is pressed.
func LooseEndsJobID(principalID string) string {
	sum := sha256.Sum256([]byte("loose-ends\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}
