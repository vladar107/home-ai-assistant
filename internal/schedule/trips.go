package schedule

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	TripsName = "Trips"
	TripsCron = "30 7 * * *"
	// TripScanType is the job type of the Trips scan, whose handler lives in the trip package.
	TripScanType = "trip.scan"
)

// TripsJobID is the principal's Trips scan job ID, so the job is found and created once however
// often the switch is pressed.
func TripsJobID(principalID string) string {
	sum := sha256.Sum256([]byte("trips\x00" + principalID))
	return hex.EncodeToString(sum[:8])
}
