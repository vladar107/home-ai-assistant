package nas

import (
	"errors"
	"fmt"
	"os"
)

type ErrorClass string

const (
	ErrorDenied      ErrorClass = "denied"
	ErrorNotFound    ErrorClass = "not_found"
	ErrorExists      ErrorClass = "exists"
	ErrorDirectory   ErrorClass = "directory"
	ErrorUnavailable ErrorClass = "unavailable"
)

// Error is a normalized NAS failure carrying only its class, so a host path never reaches a log,
// an error chain, or the model. The model sees names relative to a root and nothing about where
// those roots actually live.
type Error struct {
	Class ErrorClass
}

func (e *Error) Error() string {
	return fmt.Sprintf("NAS request failed: %s", e.Class)
}

func classify(err error) *Error {
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &Error{Class: ErrorNotFound}
	case errors.Is(err, os.ErrExist):
		return &Error{Class: ErrorExists}
	case errors.Is(err, os.ErrPermission):
		return &Error{Class: ErrorDenied}
	default:
		return &Error{Class: ErrorUnavailable}
	}
}

// failureText is what the model reads for a failed call. It names the user's next step where
// there is one.
func failureText(class ErrorClass) string {
	switch class {
	case ErrorDenied:
		return "That folder is read-only for the assistant."
	case ErrorNotFound:
		return "The NAS has no folder or file at that path."
	case ErrorExists:
		return "A file with that name already exists; nothing was replaced. Propose a different name."
	case ErrorDirectory:
		return "That path is a folder, not a file. List it instead of reading it."
	default:
		return "The NAS is unavailable right now."
	}
}
