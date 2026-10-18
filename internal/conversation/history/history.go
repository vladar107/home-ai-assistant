// Package history reads a principal's earlier conversations from the session files Pi keeps, for
// conversation titles and for the model's search and read tools. Nothing is copied out of the
// files: every title, match, and excerpt is read when it is asked for.
package history

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

const (
	// maximumSessions bounds how many of a principal's conversations a search scans or a read can
	// reach, newest first.
	maximumSessions  = 200
	maximumFileBytes = 32 << 20
	// maximumLineBytes skips an entry larger than this, such as a message carrying a big photo,
	// rather than holding it in memory.
	maximumLineBytes = 4 << 20
	titleRunes       = 60
)

// Sessions lists the principal's conversations as the store knows them.
type Sessions interface {
	ConversationSessions(ctx context.Context, principalID string, limit int) ([]conversation.SessionRecord, error)
}

// Directories locates the principal's Pi session files.
type Directories interface {
	SessionDirectory(principalID string) (string, error)
}

type Telemetry interface {
	RecordConversationSearch(ctx context.Context, principal, outcome string)
}

type Service struct {
	sessions    Sessions
	directories Directories
	location    *time.Location
	telemetry   Telemetry
}

var _ conversation.History = (*Service)(nil)

// NewService reads history in the household's time zone; a nil location means UTC.
func NewService(sessions Sessions, directories Directories, location *time.Location, telemetry Telemetry) (*Service, error) {
	if sessions == nil {
		return nil, fmt.Errorf("conversation history sessions are required")
	}
	if directories == nil {
		return nil, fmt.Errorf("conversation history directories are required")
	}
	if telemetry == nil {
		return nil, fmt.Errorf("conversation history telemetry is required")
	}
	if location == nil {
		location = time.UTC
	}
	return &Service{sessions: sessions, directories: directories, location: location, telemetry: telemetry}, nil
}

// Title is the first thing the user wrote in the session, on one line and cut to 60 characters.
func (s *Service) Title(_ context.Context, principalID, runtimeSessionID string) (string, error) {
	var title string
	err := s.messages(principalID, runtimeSessionID, func(message message) bool {
		if message.role != roleUser {
			return true
		}
		title = clip.Runes(oneLine(message.text), titleRunes)
		return false
	})
	return title, err
}

const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// message is one user or assistant message as the user saw it: the [system] lines the application
// adds to a turn, tool calls, tool results, and reasoning are not part of it.
type message struct {
	role string
	at   time.Time
	text string
}

type sessionEntry struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stopReason"`
	} `json:"message"`
}

// messages calls visit with each message of the session in the order Pi recorded them, until
// visit returns false. A session can span several files: before plan 0015 each working directory
// got its own, so the files are read oldest first by their timestamped names.
func (s *Service) messages(principalID, runtimeSessionID string, visit func(message) bool) error {
	directory, err := s.directories.SessionDirectory(principalID)
	if err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Pi session directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Pi session directory must be a real directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("list Pi session files: %w", err)
	}
	suffix := "_" + runtimeSessionID + ".jsonl"
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), suffix) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		more, err := s.readFile(filepath.Join(directory, name), runtimeSessionID, visit)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

func (s *Service) readFile(path, runtimeSessionID string, visit func(message) bool) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("open Pi session file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(io.LimitReader(file, maximumFileBytes))
	header := true
	for {
		line, err := readLine(reader)
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read Pi session file: %w", err)
		}
		if line == nil {
			continue
		}
		var entry sessionEntry
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if header {
			if entry.Type != "session" || entry.ID != runtimeSessionID {
				return true, nil
			}
			header = false
			continue
		}
		message, ok := s.readMessage(entry)
		if !ok {
			continue
		}
		if !visit(message) {
			return false, nil
		}
	}
}

// readLine returns the next line, or nil for a line longer than maximumLineBytes, which is skipped.
func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	oversized := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(chunk) > maximumLineBytes {
				oversized, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 && !oversized {
				return nil, io.EOF
			}
			return line, nil
		case err != nil:
			return nil, err
		}
		return line, nil
	}
}

func (s *Service) readMessage(entry sessionEntry) (message, bool) {
	if entry.Type != "message" || entry.Message == nil {
		return message{}, false
	}
	var text string
	switch entry.Message.Role {
	case roleUser:
		text = userText(contentText(entry.Message.Content))
	case roleAssistant:
		if entry.Message.StopReason == "error" || entry.Message.StopReason == "aborted" {
			return message{}, false
		}
		text = strings.TrimSpace(contentText(entry.Message.Content))
	default:
		return message{}, false
	}
	if text == "" {
		return message{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
	return message{role: entry.Message.Role, at: at.In(s.location), text: text}, true
}

// contentText joins the text blocks of a message; a user message may also be a plain string.
func contentText(content json.RawMessage) string {
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		return plain
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// attachedPrefix opens the list of files the application appends to a turn's message.
const attachedPrefix = "[Attached by the user]"

// userText removes what the application added to the user's message: the [system] paragraphs
// before it and the list of attached files after it.
func userText(text string) string {
	paragraphs := strings.Split(text, "\n\n")
	kept := paragraphs[:0]
	for _, paragraph := range paragraphs {
		trimmed := strings.TrimSpace(paragraph)
		if strings.HasPrefix(trimmed, prompt.Marker) || strings.HasPrefix(trimmed, attachedPrefix) {
			continue
		}
		kept = append(kept, paragraph)
	}
	return strings.TrimSpace(strings.Join(kept, "\n\n"))
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
