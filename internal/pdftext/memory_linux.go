//go:build linux && !race

package pdftext

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// limitMemory makes the kernel refuse the worker's allocations once it has mapped ceiling bytes
// more than it holds now. RLIMIT_DATA counts the Go heap on current kernels; RLIMIT_AS covers
// kernels where it does not, and starts above the address space the runtime has already reserved.
// The race detector's shadow memory would trip both, so race builds rely on the watchdog.
func limitMemory(ceiling uint64) error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	for _, limit := range []struct {
		field    string
		resource int
	}{
		{"VmSize:", syscall.RLIMIT_AS},
		{"VmData:", syscall.RLIMIT_DATA},
	} {
		current, err := kilobytes(status, limit.field)
		if err != nil {
			return fmt.Errorf("memory: %w", err)
		}
		bound := current<<10 + ceiling
		if err := lower(limit.resource, &syscall.Rlimit{Cur: bound, Max: bound}); err != nil {
			return fmt.Errorf("memory (%s) %w", strings.TrimSuffix(limit.field, ":"), err)
		}
	}
	return nil
}

func kilobytes(status []byte, field string) (uint64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(status))
	for scanner.Scan() {
		value, found := strings.CutPrefix(scanner.Text(), field)
		if found {
			return strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		}
	}
	return 0, fmt.Errorf("/proc/self/status has no %s", field)
}
