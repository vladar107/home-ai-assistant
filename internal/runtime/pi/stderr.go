package pi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// stderrTailBytes is how much of the end of Pi's stderr a turn keeps.
	stderrTailBytes = 4 << 10
	// loggedStderrBytes bounds the scrubbed tail an early exit logs.
	loggedStderrBytes = 1 << 10
	redactedText      = "[redacted]"
)

// tailBuffer keeps the last bytes written to it. exec copies the process's stderr into it from its
// own goroutine.
type tailBuffer struct {
	limit int

	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if extra := len(b.data) - b.limit; extra > 0 {
		b.data = append(b.data[:0], b.data[extra:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// Pi's stderr can name provider responses, request headers, or file contents, so its tail is
// scrubbed of anything shaped like a credential or an address before it is logged.
var stderrSecrets = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)\b(Authorization[ \t]*[:=][ \t]*)[^\r\n]+`), "${1}" + redactedText},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + redactedText},
	{regexp.MustCompile(`(?i)\b((?:[A-Za-z0-9]+[_-])*(?:api[_-]?key|key|token|secret|password|passwd|cookie)["']?[ \t]*[=:][ \t]*)("[^"]*"|'[^']*'|[^\s"',;&]+)`), "${1}" + redactedText},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), redactedText},
	{regexp.MustCompile(`\b(?:sk-|ghp_|gho_|github_pat_|glpat-|xox[bap]-|AKIA)[A-Za-z0-9_-]{16,}`), redactedText},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`), redactedText},
	{regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}`), redactedText},
}

// scrubStderr redacts credentials from Pi's stderr and keeps its last loggedStderrBytes.
func scrubStderr(text string) string {
	for _, secret := range stderrSecrets {
		text = secret.pattern.ReplaceAllString(text, secret.replacement)
	}
	text = strings.TrimSpace(text)
	if len(text) > loggedStderrBytes {
		text = text[len(text)-loggedStderrBytes:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	return text
}

// exitedEarly logs why Pi stopped before the turn completed, with the scrubbed end of its stderr,
// and returns the error the turn ends with. The error carries the exit code only: it travels
// further than the log.
func (r *Runtime) exitedEarly(ctx context.Context, principal, reason string, waitErr error, exited bool, stderr *tailBuffer) error {
	exitCode := -1
	var exitErr *exec.ExitError
	switch {
	case !exited:
	case errors.As(waitErr, &exitErr):
		exitCode = exitErr.ExitCode()
	case waitErr == nil:
		exitCode = 0
	}
	logger := r.logger
	if r.telemetry != nil {
		logger = r.telemetry.ContextLogger(ctx, logger)
	}
	logger.Warn("pi.process.exited_early",
		"principal_id", principal,
		"reason", reason,
		"exit_code", exitCode,
		"stderr", scrubStderr(stderr.String()),
	)
	if exitCode >= 0 {
		return fmt.Errorf("%s (exit code %d)", reason, exitCode)
	}
	return errors.New(reason)
}
