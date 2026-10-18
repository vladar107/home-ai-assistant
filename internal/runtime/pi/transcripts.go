package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// forgottenImageText takes the place of a photo stripped from a closed conversation's history, so
// a resumed conversation still reads as it went.
const forgottenImageText = "[A photo was here; it was deleted when the conversation ended.]"

// imageMarker is how every image content block Pi writes begins; a line without it needs no
// parsing.
var imageMarker = []byte(`"type":"image"`)

// sessionGate is held by the turn that runs a session and by a rewrite of its files, so Pi never
// appends to a file that is being replaced.
type sessionGate struct {
	held  chan struct{}
	users int
	// forget asks the holder to strip the session's images before it lets go.
	forget bool
}

func sessionKey(principalID, sessionID string) string {
	return principalID + "/" + sessionID
}

// gate returns the session's gate with one more user. The caller holds r.gatesMu.
func (r *Runtime) gate(key string) *sessionGate {
	gate := r.gates[key]
	if gate == nil {
		gate = &sessionGate{held: make(chan struct{}, 1)}
		r.gates[key] = gate
	}
	gate.users++
	return gate
}

// leaveGate drops a user, and the gate once nobody uses it. The caller holds r.gatesMu.
func (r *Runtime) leaveGate(key string, gate *sessionGate) {
	gate.users--
	if gate.users == 0 {
		delete(r.gates, key)
	}
}

// lockSession waits for the session's gate and returns the function that lets it go.
func (r *Runtime) lockSession(ctx context.Context, principalID, sessionDir, sessionID string) (func(), error) {
	key := sessionKey(principalID, sessionID)
	r.gatesMu.Lock()
	gate := r.gate(key)
	r.gatesMu.Unlock()
	select {
	case gate.held <- struct{}{}:
	case <-ctx.Done():
		r.gatesMu.Lock()
		r.leaveGate(key, gate)
		r.gatesMu.Unlock()
		return nil, ctx.Err()
	}
	return func() { r.unlockSession(principalID, sessionDir, sessionID, gate) }, nil
}

// tryLockSession takes the session's gate if it is free. When it is held and askHolder is set, the
// holder is asked to strip the session's images before it lets go.
func (r *Runtime) tryLockSession(principalID, sessionDir, sessionID string, askHolder bool) (func(), bool) {
	key := sessionKey(principalID, sessionID)
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()
	gate := r.gate(key)
	select {
	case gate.held <- struct{}{}:
		return func() { r.unlockSession(principalID, sessionDir, sessionID, gate) }, true
	default:
		gate.forget = gate.forget || askHolder
		r.leaveGate(key, gate)
		return nil, false
	}
}

// unlockSession lets the gate go, first stripping the session's images when ForgetImages asked for
// that while the gate was held.
func (r *Runtime) unlockSession(principalID, sessionDir, sessionID string, gate *sessionGate) {
	r.gatesMu.Lock()
	forget := gate.forget
	gate.forget = false
	r.gatesMu.Unlock()
	if forget {
		if err := forgetImages(sessionDir, sessionID); err != nil {
			r.logger.Warn("pi.session.forget_images_failed", "principal_id", principalID, "error", err)
		}
	}
	r.gatesMu.Lock()
	<-gate.held
	r.leaveGate(sessionKey(principalID, sessionID), gate)
	r.gatesMu.Unlock()
}

// ForgetImages strips every photo from a session's history and keeps its text, so a closed
// conversation keeps no copy of what the user sent. A session with a turn running is stripped
// when that turn ends.
func (r *Runtime) ForgetImages(_ context.Context, principalID, sessionID string) error {
	sessionDir, err := r.SessionDirectory(principalID)
	if err != nil {
		return err
	}
	release, ok := r.tryLockSession(principalID, sessionDir, sessionID, true)
	if !ok {
		return nil
	}
	defer release()
	return forgetImages(sessionDir, sessionID)
}

func forgetImages(sessionDir, sessionID string) error {
	paths, err := sessionFiles(sessionDir, sessionID)
	if err != nil {
		return fmt.Errorf("find Pi session files: %w", err)
	}
	var errs []error
	for _, path := range paths {
		errs = append(errs, forgetFileImages(path))
	}
	return errors.Join(errs...)
}

// forgetFileImages rewrites one session file without its image blocks. A file that has none is
// left untouched, and a rewritten one keeps its modification time, so stripping never counts as
// the conversation being used.
func forgetFileImages(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Pi session file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	source, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open Pi session file: %w", err)
	}
	defer source.Close()

	var rewritten bytes.Buffer
	changed := false
	reader := bufio.NewReader(source)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			stripped, lineChanged := stripLineImages(line)
			changed = changed || lineChanged
			rewritten.Write(stripped)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read Pi session file: %w", readErr)
		}
	}
	if !changed {
		return nil
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".forget-*.tmp")
	if err != nil {
		return fmt.Errorf("create Pi session rewrite: %w", err)
	}
	replaced := false
	defer func() {
		if !replaced {
			_ = os.Remove(temporary.Name())
		}
	}()
	if _, err := temporary.Write(rewritten.Bytes()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write Pi session rewrite: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Pi session rewrite: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Pi session rewrite: %w", err)
	}
	if err := os.Chtimes(temporary.Name(), time.Time{}, info.ModTime()); err != nil {
		return fmt.Errorf("date Pi session rewrite: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("replace Pi session file: %w", err)
	}
	replaced = true
	return nil
}

// stripLineImages replaces the image blocks of one JSONL entry with a text block. A line that is
// not JSON, or has no image block, comes back as it was.
func stripLineImages(line []byte) ([]byte, bool) {
	if !bytes.Contains(line, imageMarker) {
		return line, false
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var entry any
	if decoder.Decode(&entry) != nil {
		return line, false
	}
	entry, changed := stripImages(entry)
	if !changed {
		return line, false
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(entry) != nil {
		return line, false
	}
	return encoded.Bytes(), true
}

func stripImages(value any) (any, bool) {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		if _, carriesData := typed["data"]; carriesData && typed["type"] == "image" {
			return map[string]any{"type": "text", "text": forgottenImageText}, true
		}
		for key, child := range typed {
			if stripped, childChanged := stripImages(child); childChanged {
				typed[key], changed = stripped, true
			}
		}
	case []any:
		for index, child := range typed {
			if stripped, childChanged := stripImages(child); childChanged {
				typed[index], changed = stripped, true
			}
		}
	}
	return value, changed
}

// SessionReferences is what the store still keeps of conversations and tasks.
type SessionReferences interface {
	// ReferencedSessions maps the runtime session ID of every kept conversation and task to whether
	// its conversation is closed.
	ReferencedSessions(ctx context.Context) (map[string]bool, error)
	// DeleteClosedConversations deletes the conversations that closed before the cutoff.
	DeleteClosedConversations(ctx context.Context, closedBefore time.Time) error
}

// closedConversationRetention is how long a closed conversation stays resumable and searchable.
const closedConversationRetention = 90 * 24 * time.Hour

// SessionPruner keeps the session files within their retention. A closed conversation is kept,
// without its photos, for closedConversationRetention: its row is deleted then, and its file goes
// with those of scheduled runs and tasks that nothing refers to and that were last written before
// the cutoff.
type SessionPruner struct {
	runtime    *Runtime
	references SessionReferences
	// stripped is when the last pass that stripped every closed conversation began; a closed
	// conversation's file last written before then has been stripped already.
	stripped time.Time
}

func (r *Runtime) SessionPruner(references SessionReferences) *SessionPruner {
	return &SessionPruner{runtime: r, references: references}
}

// PruneSessions runs one pass. Passes must not overlap, which the scheduler's single tick loop
// guarantees.
func (p *SessionPruner) PruneSessions(ctx context.Context, before time.Time) error {
	if err := p.references.DeleteClosedConversations(ctx, time.Now().Add(-closedConversationRetention)); err != nil {
		return err
	}
	referenced, err := p.references.ReferencedSessions(ctx)
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(p.runtime.profilesDir, "*", "sessions", "*_*.jsonl"))
	if err != nil {
		return err
	}
	started := time.Now()
	var errs []error
	stripFailed := false
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		sessionID := name[strings.LastIndex(name, "_")+1:]
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if closed, kept := referenced[sessionID]; kept {
			if closed && !info.ModTime().Before(p.stripped) {
				if err := p.stripClosed(path, sessionID); err != nil {
					errs = append(errs, err)
					stripFailed = true
				}
			}
			continue
		}
		if !info.ModTime().Before(before) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if !stripFailed {
		p.stripped = started
	}
	return errors.Join(errs...)
}

// stripClosed strips one file of a closed conversation. A session with a turn running may have
// been resumed since the store was read, so it is left for the next pass, which sees the file the
// turn wrote.
func (p *SessionPruner) stripClosed(path, sessionID string) error {
	sessionDir := filepath.Dir(path)
	principalID := filepath.Base(filepath.Dir(sessionDir))
	if _, err := agentruntime.ProfileDirectoryName(principalID); err != nil {
		return nil
	}
	release, ok := p.runtime.tryLockSession(principalID, sessionDir, sessionID, false)
	if !ok {
		return nil
	}
	defer release()
	return forgetFileImages(path)
}
