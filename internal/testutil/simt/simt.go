// Package simt is the part of *testing.T the simulators use, so a harness that is not a test can
// run them too.
package simt

// T is satisfied by *testing.T. Fatalf must stop the caller's goroutine, as testing.T's does.
type T interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Cleanup(func())
}
