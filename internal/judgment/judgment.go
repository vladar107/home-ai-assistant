// Package judgment asks TypeSafe's Jev model narrow, typed questions about
// household text. Only Redacted values, which this package alone can build,
// ever leave the process.
package judgment

import "context"

// OptIn reports whether a principal has agreed to send redacted text to TypeSafe.
type OptIn interface {
	JudgmentsEnabled(ctx context.Context, principalID string) (bool, error)
}
