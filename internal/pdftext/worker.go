package pdftext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"syscall"
	"time"
)

// The worker is this same binary started again with workerVariable set; it is recognised in init,
// so the service and every test binary that links this package can parse PDFs without any hook in
// their main functions. The variable is an internal marker, not a setting.
const (
	workerVariable = "PDFTEXT_WORKER"
	workerValue    = "1"
)

const (
	// workerMemory is the most memory the worker's Go runtime may hold, on top of what it holds
	// when it starts; workerHeapGoal is where the collector starts working hard to stay below it.
	workerMemory   = 384 << 20
	workerHeapGoal = 256 << 20
	workerStack    = 64 << 20
	workerCPU      = 20
	workerTimeout  = 30 * time.Second
	// workerReplyBytes allows for the JSON escaping of the longest text the worker may return.
	workerReplyBytes = 2*MaximumTextBytes + 4<<10
	// workerOverMemory is the exit status of a worker its watchdog stopped.
	workerOverMemory = 3
)

// turn lets one worker run at a time, so concurrent readers cannot multiply the memory bound.
var turn = make(chan struct{}, 1)

type reply struct {
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

const (
	refusalEncrypted = "encrypted"
	refusalNoText    = "no-text"
	refusalMalformed = "malformed"
)

func delegate(ctx context.Context, data []byte) (string, error) {
	executable, err := self()
	if err != nil {
		return "", fmt.Errorf("pdftext: find the worker binary: %w", err)
	}
	workerCtx, cancel := context.WithTimeout(ctx, workerTimeout)
	defer cancel()

	command := exec.CommandContext(workerCtx, executable)
	// The worker gets nothing of the service's environment, so a hostile document never runs
	// beside its secrets.
	command.Env = []string{workerVariable + "=" + workerValue}
	command.Stdin = bytes.NewReader(data)
	output := &cappedBuffer{limit: workerReplyBytes}
	command.Stdout = output
	command.WaitDelay = time.Second
	err = command.Run()

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if errors.Is(workerCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%w: reading it took longer than %s", ErrMalformed, workerTimeout)
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return "", fmt.Errorf("pdftext: run the worker: %w", err)
		}
		if exit.ExitCode() == workerOverMemory {
			return "", fmt.Errorf("%w: reading it needs more than %d MB of memory", ErrMalformed, workerMemory>>20)
		}
		return "", fmt.Errorf("%w: the reader stopped (%v), probably at its memory or time limit", ErrMalformed, exit)
	}
	if output.overflowed {
		return "", fmt.Errorf("%w: the reader's answer is too long", ErrMalformed)
	}
	var answer reply
	if err := json.Unmarshal(output.buffer.Bytes(), &answer); err != nil {
		return "", fmt.Errorf("pdftext: decode the worker's answer: %w", err)
	}
	switch answer.Refusal {
	case "":
		return answer.Text, nil
	case refusalEncrypted:
		return "", ErrEncrypted
	case refusalNoText:
		return "", ErrNoText
	case refusalMalformed:
		if answer.Detail == "" {
			return "", ErrMalformed
		}
		return "", fmt.Errorf("%w: %s", ErrMalformed, answer.Detail)
	}
	return "", fmt.Errorf("pdftext: the worker answered with an unknown refusal %q", answer.Refusal)
}

// self names the running binary. On Linux /proc/self/exe still starts it after the file on disk
// has been replaced by a rebuild or an upgrade.
func self() (string, error) {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/proc/self/exe"); err == nil {
			return "/proc/self/exe", nil
		}
	}
	return os.Executable()
}

// cappedBuffer keeps at most limit bytes and swallows the rest, so a runaway worker can neither
// grow the service's memory nor stall on a full pipe.
type cappedBuffer struct {
	buffer     bytes.Buffer
	limit      int
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buffer.Len(); len(p) > room {
		b.overflowed = true
		b.buffer.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.buffer.Write(p)
}

func init() {
	if os.Getenv(workerVariable) == workerValue {
		os.Exit(serve(os.Stdin, os.Stdout))
	}
}

// serve is the worker: it limits itself, reads one PDF from input, and writes one reply.
func serve(input io.Reader, output io.Writer) int {
	if err := limit(); err != nil {
		fmt.Fprintln(os.Stderr, "pdftext: limit the worker:", err)
		return 1
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return 1
	}
	var answer reply
	text, err := extract(data)
	switch {
	case err == nil:
		answer.Text = text
	case errors.Is(err, ErrEncrypted):
		answer.Refusal = refusalEncrypted
	case errors.Is(err, ErrNoText):
		answer.Refusal = refusalNoText
	default:
		answer.Refusal = refusalMalformed
		answer.Detail = strings.TrimPrefix(err.Error(), ErrMalformed.Error()+": ")
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(answer); err != nil {
		return 1
	}
	return 0
}

func limit() error {
	runtime.GOMAXPROCS(1)
	debug.SetMaxStack(workerStack)
	debug.SetMemoryLimit(workerHeapGoal)
	cpu := &syscall.Rlimit{Cur: workerCPU, Max: workerCPU}
	if err := lower(syscall.RLIMIT_CPU, cpu); err != nil {
		return fmt.Errorf("CPU time: %w", err)
	}
	if err := limitMemory(workerMemory); err != nil {
		return err
	}
	go watch(committed() + workerMemory)
	return nil
}

// lower sets a resource limit without trying to raise one already below it, which an
// unprivileged process may not do.
func lower(resource int, wanted *syscall.Rlimit) error {
	var current syscall.Rlimit
	if err := syscall.Getrlimit(resource, &current); err != nil {
		return err
	}
	wanted.Max = min(wanted.Max, current.Max)
	wanted.Cur = min(wanted.Cur, wanted.Max)
	return syscall.Setrlimit(resource, wanted)
}

// watch stops the worker once the Go runtime holds more than ceiling bytes. It is the only memory
// bound on systems without kernel limits for it, such as macOS, and a second line on Linux.
func watch(ceiling uint64) {
	for range time.Tick(5 * time.Millisecond) {
		if committed() > ceiling {
			fmt.Fprintln(os.Stderr, "pdftext: the worker passed its memory limit")
			os.Exit(workerOverMemory)
		}
	}
}

func committed() uint64 {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(samples)
	return samples[0].Value.Uint64() - samples[1].Value.Uint64()
}
