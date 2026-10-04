// Package outbox holds an image a tool staged for the reply of the turn that called it, between the
// call and the reply's delivery. The bytes live on disk under the principal's own directory, keyed
// by the turn, beside the durable delivery row, so a restart in between still sends them; delivery
// removes them, and a sweep collects what a cancelled or abandoned turn left behind. Chat turns and
// scheduled runs keep their cards in separate subdirectories, so starting a new conversation
// discards only the chat's; background tasks keep theirs apart too.
package outbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// MaximumBytes is Telegram's limit for a photo upload.
const MaximumBytes = 10 << 20

const (
	stagedSuffix = ".png"
	sentSuffix   = ".sent"

	captionedMark   = '1'
	uncaptionedMark = '0'

	chatKind     = "chat"
	scheduleKind = "schedule"
	taskKind     = "task"
)

// idPattern accepts the request and run identifiers turns are keyed by and nothing that could name
// a path outside the principal's directory.
var idPattern = regexp.MustCompile(`^[0-9a-f][0-9a-f-]{0,63}$`)

// ChatTurn is the turn key of a conversation request.
func ChatTurn(requestID string) string { return chatKind + "/" + requestID }

// ChatRequest returns the conversation request a chat turn key names; any other turn reports false.
func ChatRequest(turn string) (string, bool) {
	requestID, found := strings.CutPrefix(turn, chatKind+"/")
	return requestID, found && idPattern.MatchString(requestID)
}

// ScheduledTurn is the turn key of a scheduled run.
func ScheduledTurn(runID string) string { return scheduleKind + "/" + runID }

// TaskTurn is the turn key of a background task's runs.
func TaskTurn(taskID string) string { return taskKind + "/" + taskID }

// Card is what one turn staged. A sent card has no bytes left: only whether its photo carried the
// reply's first chunk as a caption, so a retried delivery sends neither again.
type Card struct {
	PNG       []byte
	Sent      bool
	Captioned bool
}

type Box struct {
	root string
}

func New(directory string) (*Box, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("outbox directory is required")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve outbox directory: %w", err)
	}
	if err := preparePrivateDirectory(root); err != nil {
		return nil, err
	}
	return &Box{root: root}, nil
}

// Stage keeps a PNG for the reply of the principal's turn. A later card of the same turn replaces
// the earlier one.
func (b *Box) Stage(principalID, turn string, png []byte) error {
	if len(png) == 0 {
		return fmt.Errorf("outbox card is empty")
	}
	if len(png) > MaximumBytes {
		return fmt.Errorf("outbox card exceeds %d bytes", MaximumBytes)
	}
	path, err := b.path(principalID, turn, stagedSuffix)
	if err != nil {
		return err
	}
	kindDirectory := filepath.Dir(path)
	if err := preparePrivateDirectory(filepath.Dir(kindDirectory)); err != nil {
		return err
	}
	if err := preparePrivateDirectory(kindDirectory); err != nil {
		return err
	}
	return writeAtomically(path, png)
}

// Card returns what the principal's turn staged, if anything.
func (b *Box) Card(principalID, turn string) (Card, bool, error) {
	sentPath, err := b.path(principalID, turn, sentSuffix)
	if err != nil {
		return Card{}, false, err
	}
	mark, err := os.ReadFile(sentPath)
	switch {
	case err == nil:
		return Card{Sent: true, Captioned: len(mark) == 1 && mark[0] == captionedMark}, true, nil
	case !errors.Is(err, os.ErrNotExist):
		return Card{}, false, fmt.Errorf("read outbox card: %w", err)
	}
	data, err := os.ReadFile(strings.TrimSuffix(sentPath, sentSuffix) + stagedSuffix)
	switch {
	case err == nil:
		return Card{PNG: data}, true, nil
	case errors.Is(err, os.ErrNotExist):
		return Card{}, false, nil
	default:
		return Card{}, false, fmt.Errorf("read outbox card: %w", err)
	}
}

// MarkSent records that the turn's photo reached the chat and whether its caption carried the
// reply's first chunk, dropping the bytes.
func (b *Box) MarkSent(principalID, turn string, captioned bool) error {
	sentPath, err := b.path(principalID, turn, sentSuffix)
	if err != nil {
		return err
	}
	mark := byte(uncaptionedMark)
	if captioned {
		mark = captionedMark
	}
	if err := writeAtomically(sentPath, []byte{mark}); err != nil {
		return err
	}
	if err := os.Remove(strings.TrimSuffix(sentPath, sentSuffix) + stagedSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove sent outbox card: %w", err)
	}
	return nil
}

// Remove forgets the turn's card once its reply is delivered or abandoned. What it cannot remove is
// left for Sweep.
func (b *Box) Remove(principalID, turn string) {
	sentPath, err := b.path(principalID, turn, sentSuffix)
	if err != nil {
		return
	}
	_ = os.Remove(sentPath)
	_ = os.Remove(strings.TrimSuffix(sentPath, sentSuffix) + stagedSuffix)
}

// Discard removes every card the principal's conversation has staged, as when they start a new
// one. Cards of scheduled runs and background tasks belong to no conversation and stay for their
// own delivery.
func (b *Box) Discard(principalID string) {
	directory, err := b.principalDirectory(principalID)
	if err != nil {
		return
	}
	directory = filepath.Join(directory, chatKind)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(directory, entry.Name()))
	}
}

// Sweep removes every file last written before the given time, whatever turn it belonged to. It
// keeps going past what it cannot remove and reports all of it.
func (b *Box) Sweep(before time.Time) error {
	principals, err := os.ReadDir(b.root)
	if err != nil {
		return fmt.Errorf("read outbox directory: %w", err)
	}
	var errs []error
	for _, principal := range principals {
		if !principal.IsDir() {
			continue
		}
		directory := filepath.Join(b.root, principal.Name())
		kinds, err := os.ReadDir(directory)
		if err != nil {
			errs = append(errs, fmt.Errorf("read outbox directory: %w", err))
			continue
		}
		for _, kind := range kinds {
			if !kind.IsDir() {
				continue
			}
			errs = append(errs, sweepDirectory(filepath.Join(directory, kind.Name()), before))
		}
	}
	return errors.Join(errs...)
}

func sweepDirectory(directory string, before time.Time) error {
	files, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read outbox directory: %w", err)
	}
	var errs []error
	for _, file := range files {
		info, err := file.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect outbox file: %w", err))
			continue
		}
		if !info.ModTime().Before(before) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(directory, file.Name())); err != nil {
			errs = append(errs, fmt.Errorf("remove outbox file: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (b *Box) path(principalID, turn, suffix string) (string, error) {
	directory, err := b.principalDirectory(principalID)
	if err != nil {
		return "", err
	}
	kind, id, _ := strings.Cut(turn, "/")
	if (kind != chatKind && kind != scheduleKind && kind != taskKind) || !idPattern.MatchString(id) {
		return "", fmt.Errorf("outbox turn key is invalid")
	}
	return filepath.Join(directory, kind, id+suffix), nil
}

func (b *Box) principalDirectory(principalID string) (string, error) {
	name, err := agentruntime.ProfileDirectoryName(principalID)
	if err != nil {
		return "", err
	}
	return filepath.Join(b.root, name), nil
}

// writeAtomically replaces path in one rename, so delivery never reads half a card.
func writeAtomically(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create outbox file: %w", err)
	}
	temporary := file.Name()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("write outbox file: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("secure outbox file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("store outbox file: %w", err)
	}
	return nil
}

func preparePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("outbox directory %q must be a real directory", filepath.Base(path))
		}
		if info.Mode().Perm() != 0o700 {
			return fmt.Errorf("outbox directory %q permissions are %04o, want 0700", filepath.Base(path), info.Mode().Perm())
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create outbox directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure outbox directory: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("inspect outbox directory: %w", err)
	}
}
