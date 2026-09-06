package notion

import "fmt"

type ErrorClass string

const (
	ErrorUnauthorized ErrorClass = "unauthorized"
	ErrorRestricted   ErrorClass = "restricted"
	ErrorNotShared    ErrorClass = "not_shared"
	ErrorRateLimited  ErrorClass = "rate_limited"
	ErrorInvalid      ErrorClass = "invalid"
	ErrorUnavailable  ErrorClass = "unavailable"
)

// Error is a normalized Notion failure. It carries the class and HTTP status only, so Notion's
// message text never reaches a log, an error chain, or the model.
type Error struct {
	Class  ErrorClass
	Status int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("Notion request failed: %s (HTTP status %d)", e.Class, e.Status)
	}
	return fmt.Sprintf("Notion request failed: %s", e.Class)
}

func classify(status int) ErrorClass {
	switch {
	case status == 400:
		return ErrorInvalid
	case status == 401:
		return ErrorUnauthorized
	case status == 403:
		return ErrorRestricted
	case status == 404:
		return ErrorNotShared
	case status == 429 || status == 529:
		return ErrorRateLimited
	default:
		return ErrorUnavailable
	}
}
