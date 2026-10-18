package pi_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
)

const pictureText = "synthetic picture bytes"

var encodedPicture = base64.StdEncoding.EncodeToString([]byte(pictureText))

func sessionFile(t *testing.T, cfg pi.Config, session string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(sessionDir(cfg, testPrincipal), "*_"+session+".jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("session files = %v, %v", paths, err)
	}
	return paths[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeSessionFile writes a session the way Pi 1.0.0 records one with a photo in its first message.
func writeSessionFile(t *testing.T, cfg pi.Config, session string, modified time.Time) string {
	t.Helper()
	directory := sessionDir(cfg, testPrincipal)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "2026-01-01T00-00-00-000Z_"+session+".jsonl")
	lines := `{"type":"session","version":3,"id":"` + session + `","timestamp":"2026-01-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"what is on this label?"},{"type":"image","data":"` + encodedPicture + `","mimeType":"image/png"}],"timestamp":1767225601000}}` + "\n" +
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"a <b>label</b> & more"}],"stopReason":"stop"}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertStripped(t *testing.T, path string) {
	t.Helper()
	contents := readFile(t, path)
	if strings.Contains(contents, encodedPicture) || strings.Contains(contents, `"type":"image"`) {
		t.Fatalf("session file still holds the photo: %s", contents)
	}
	if !strings.Contains(contents, "what is on this label?") || !strings.Contains(contents, "deleted when the conversation ended") {
		t.Fatalf("session file lost its text: %s", contents)
	}
}

func TestForgetImagesKeepsTheConversationResumable(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	run := request("photo-session", "what is on this label?")
	run.Images = []agentruntime.Image{{Name: "photo.png", MimeType: "image/png", Data: []byte(pictureText)}}
	if _, err := r.Run(context.Background(), run, &eventSink{}); err != nil {
		t.Fatal(err)
	}
	path := sessionFile(t, cfg, "photo-session")
	if !strings.Contains(readFile(t, path), encodedPicture) {
		t.Fatal("the fixture did not record the photo the way Pi does")
	}
	modified := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}

	if err := r.ForgetImages(context.Background(), testPrincipal, "photo-session"); err != nil {
		t.Fatal(err)
	}
	assertStripped(t, path)
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(modified) || info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten file = %v, %v; want the old time and mode 0600", info, err)
	}

	result, err := r.Run(context.Background(), request("photo-session", "and now?"), &eventSink{})
	if err != nil || result.Response != "reply: and now?" {
		t.Fatalf("resumed turn = %q, %v", result.Response, err)
	}
	if sessionFile(t, cfg, "photo-session") != path {
		t.Fatal("the resumed turn did not continue the stripped file")
	}
}

func TestForgetImagesWaitsForTheRunningTurn(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	path := writeSessionFile(t, cfg, "busy-session", time.Now().Add(-time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, request("busy-session", "fixture:hold"), &eventSink{})
		finished <- err
	}()
	readReport(t, cfg, testPrincipal, "busy-session")

	if err := r.ForgetImages(context.Background(), testPrincipal, "busy-session"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), encodedPicture) {
		t.Fatal("the file was rewritten while a turn was using it")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(8 * time.Second):
		t.Fatal("the turn did not stop")
	}
	assertStripped(t, path)
}

type sessionReferences struct {
	referenced   map[string]bool
	closedBefore time.Time
}

func (r *sessionReferences) ReferencedSessions(context.Context) (map[string]bool, error) {
	return r.referenced, nil
}

func (r *sessionReferences) DeleteClosedConversations(_ context.Context, closedBefore time.Time) error {
	r.closedBefore = closedBefore
	return nil
}

func TestPrunerKeepsConversationsWithoutPhotosAndRemovesOldUnreferencedFiles(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour).Truncate(time.Second)
	active := writeSessionFile(t, cfg, "active-chat", old)
	closed := writeSessionFile(t, cfg, "closed-chat", old)
	oldRun := writeSessionFile(t, cfg, "old-run", old)
	recentRun := writeSessionFile(t, cfg, "recent-run", cutoff.Add(time.Hour))
	references := &sessionReferences{referenced: map[string]bool{"active-chat": false, "closed-chat": true}}
	pruner := r.SessionPruner(references)

	if err := pruner.PruneSessions(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	if retention := time.Since(references.closedBefore); retention < 90*24*time.Hour || retention > 90*24*time.Hour+time.Minute {
		t.Errorf("closed conversations deleted before %v, want 90 days ago", references.closedBefore)
	}
	if !strings.Contains(readFile(t, active), encodedPicture) {
		t.Error("an active conversation lost its photo")
	}
	assertStripped(t, closed)
	if info, err := os.Stat(closed); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("stripping changed the closed conversation's time: %v, %v", info, err)
	}
	if _, err := os.Stat(oldRun); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old unreferenced file kept: %v", err)
	}
	if _, err := os.Stat(recentRun); err != nil {
		t.Errorf("recent unreferenced file removed: %v", err)
	}

	// A closed conversation written to after a pass, such as one resumed and closed again, is
	// stripped by the next.
	appended, err := os.OpenFile(closed, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appended.WriteString(`{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-01-02T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"another"},{"type":"image","data":"` + encodedPicture + `","mimeType":"image/png"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := appended.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pruner.PruneSessions(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	assertStripped(t, closed)
	if !strings.Contains(readFile(t, closed), `"text":"a <b>label</b> & more"`) {
		t.Errorf("rewriting changed an untouched line: %s", readFile(t, closed))
	}
}
