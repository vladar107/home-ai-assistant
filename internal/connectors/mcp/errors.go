package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Error is a failed exchange reduced to a class the caller can act on. Its text never carries the
// credential, the address, or anything the server sent.
type Error struct {
	class string
	text  string
}

func (e *Error) Error() string { return e.text }

// Class is a stable name for the failure, fit for a metric label.
func (e *Error) Class() string { return e.class }

var (
	ErrInvalidOptions     = &Error{"invalid_options", "the MCP client options are invalid"}
	ErrInvalidArguments   = &Error{"invalid_arguments", "the tool name or arguments are invalid"}
	ErrUnauthorized       = &Error{"unauthorized", "the MCP server refused the credential"}
	ErrNotFound           = &Error{"not_found", "the MCP endpoint was not found"}
	ErrSessionLost        = &Error{"session_lost", "the MCP server lost the session again after it was renewed"}
	ErrUnavailable        = &Error{"unavailable", "the MCP server is unavailable"}
	ErrTimeout            = &Error{"timeout", "the MCP server did not answer in time"}
	ErrMalformed          = &Error{"malformed", "the MCP server sent an unexpected answer"}
	ErrTooLarge           = &Error{"too_large", "the MCP server's answer exceeded the size limit"}
	ErrProtocol           = &Error{"protocol", "the MCP server rejected the request"}
	ErrUnsupportedVersion = &Error{"unsupported_version", "the MCP server speaks no protocol revision the client supports"}
	ErrRedirect           = &Error{"redirect_refused", "the MCP server answered with a redirect, which is not followed"}
	ErrClosed             = &Error{"closed", "the MCP client is closed"}
	// ErrCancelled is the caller giving up, which says nothing about the server.
	ErrCancelled = &Error{"cancelled", "the request was cancelled"}
)

// errSessionGone is a 404 to a request that carried a session ID, which the transport defines as
// the server having ended the session. It never leaves the package.
var errSessionGone = errors.New("mcp session gone")

// ClassOf returns the class of an error from this package, or "unknown".
func ClassOf(err error) string {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.class
	}
	return "unknown"
}

func statusError(status int, session string) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrUnauthorized
	case status == http.StatusNotFound && session != "":
		return errSessionGone
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return ErrNotFound
	case status >= 300 && status < 400:
		return ErrRedirect
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return ErrUnavailable
	case status >= 400:
		return ErrProtocol
	default:
		return ErrMalformed
	}
}

func transportError(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return ErrTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ErrCancelled
	}
	return ErrUnavailable
}
