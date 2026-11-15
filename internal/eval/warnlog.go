package eval

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const warnLogFile = "run.log"

// warnLog collects the warn-and-above records the runtime logs during a run, as JSON lines. Records
// logged before the run's directory exists wait in memory; the file is created only once there is a
// record to write, so an empty run leaves none.
type warnLog struct {
	mu    sync.Mutex
	dir   string
	file  *os.File
	lines [][]byte
	err   error
}

func newWarnLog() *warnLog { return &warnLog{} }

func (l *warnLog) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// Write receives one JSON record per call.
func (l *warnLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, bytes.Clone(p))
	if l.dir != "" {
		l.append(p)
	}
	return len(p), nil
}

// open binds the log to the run's directory and writes what was logged so far.
func (l *warnLog) open(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dir = dir
	for _, line := range l.lines {
		l.append(line)
	}
}

func (l *warnLog) append(line []byte) {
	if l.file == nil && l.err == nil {
		l.file, l.err = os.OpenFile(filepath.Join(l.dir, warnLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	if l.file != nil {
		_, l.err = l.file.Write(line)
	}
}

// mark returns the position a trial's records start at.
func (l *warnLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

// since returns the records logged after a mark.
func (l *warnLog) since(mark int) []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	var records []json.RawMessage
	for _, line := range l.lines[mark:] {
		records = append(records, json.RawMessage(bytes.TrimSpace(line)))
	}
	return records
}

// close closes the file and reports whether any record was written.
func (l *warnLog) close() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
	}
	return l.file != nil
}
