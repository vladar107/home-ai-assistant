package parcel

import "strings"

// Status is where a parcel is on its way, in the vocabulary every source is mapped to.
type Status string

const (
	StatusRegistered     Status = "registered"
	StatusInTransit      Status = "in_transit"
	StatusOutForDelivery Status = "out_for_delivery"
	StatusAtPickupPoint  Status = "at_pickup_point"
	StatusDelivered      Status = "delivered"
	StatusProblem        Status = "problem"
	StatusUnknown        Status = "unknown"
)

var statuses = []Status{StatusRegistered, StatusInTransit, StatusOutForDelivery, StatusAtPickupPoint, StatusDelivered, StatusProblem, StatusUnknown}

// ParseStatus maps a source's normalised status string to a Status; anything else is unknown.
func ParseStatus(value string) Status {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("-", "_", " ", "_").Replace(value)
	for _, status := range statuses {
		if string(status) == value {
			return status
		}
	}
	return StatusUnknown
}

// Valid reports whether the status is one of the vocabulary.
func (s Status) Valid() bool {
	for _, status := range statuses {
		if status == s {
			return true
		}
	}
	return false
}

// Supersedes reports whether a reading of the status should replace the stored one: a status moves
// forward along the way, never back, a problem replaces anything and is replaced by anything, and
// an unknown status replaces nothing.
func (s Status) Supersedes(stored Status) bool {
	switch {
	case s == StatusUnknown || !s.Valid():
		return false
	case s == StatusProblem || stored == StatusProblem || stored == StatusUnknown:
		return true
	}
	return s.progress() >= stored.progress()
}

func (s Status) progress() int {
	switch s {
	case StatusRegistered:
		return 1
	case StatusInTransit:
		return 2
	case StatusOutForDelivery, StatusAtPickupPoint:
		return 3
	case StatusDelivered:
		return 4
	}
	return 0
}

// Notice reports whether a change to the status is worth a message of its own, as opposed to a
// silent edit of the card.
func (s Status) Notice() bool {
	switch s {
	case StatusOutForDelivery, StatusAtPickupPoint, StatusDelivered, StatusProblem:
		return true
	}
	return false
}
