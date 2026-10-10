package delivery_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/delivery"
)

// allowlist is the identity mapping as delivery sees it: only a principal with an entry has a
// chat, which is what a revoked entry or a disabled principal looks like from here.
type allowlist struct {
	chats map[string]int64
}

func (a allowlist) ResolveActiveChat(_ context.Context, principalID string) (int64, bool, error) {
	chatID, found := a.chats[principalID]
	return chatID, found, nil
}

type upload struct {
	chatID  int64
	name    string
	size    int64
	content []byte
}

// telegram is the Bot API as delivery sees it, recording what an upload actually carried.
type telegram struct {
	uploads []upload
	err     error
}

func (t *telegram) SendDocument(_ context.Context, chatID int64, name string, body io.Reader, size int64) error {
	if t.err != nil {
		return t.err
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	t.uploads = append(t.uploads, upload{chatID: chatID, name: name, size: size, content: content})
	return nil
}

func (t *telegram) SendPhoto(_ context.Context, chatID, _ int64, image []byte, _ string) error {
	if t.err != nil {
		return t.err
	}
	t.uploads = append(t.uploads, upload{chatID: chatID, name: "photo", size: int64(len(image)), content: image})
	return nil
}

type body struct {
	io.Reader
	closes int
}

func newBody(content string) *body {
	return &body{Reader: strings.NewReader(content)}
}

func (b *body) Close() error {
	b.closes++
	return nil
}

func newService(t *testing.T, chats allowlist, documents *telegram) (*delivery.Service, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	service, err := delivery.NewService(chats, documents, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service, logs
}

func TestFileReachesThePrincipalsOwnChat(t *testing.T) {
	documents := &telegram{}
	service, logs := newService(t, allowlist{chats: map[string]int64{"alice": 101, "bob": 102}}, documents)
	content := newBody("payslip")

	if err := service.SendFile(t.Context(), "alice", delivery.File{Name: "payslip.pdf", Size: 7, Body: content}); err != nil {
		t.Fatal(err)
	}

	if len(documents.uploads) != 1 {
		t.Fatalf("uploads = %+v", documents.uploads)
	}
	sent := documents.uploads[0]
	if sent.chatID != 101 || sent.name != "payslip.pdf" || sent.size != 7 || string(sent.content) != "payslip" {
		t.Fatalf("upload = %+v", sent)
	}
	if content.closes != 1 {
		t.Fatalf("body closes = %d", content.closes)
	}
	// A file name never reaches the logs, as with attachments.
	if !strings.Contains(logs.String(), "delivery.file.sent") || strings.Contains(logs.String(), "payslip.pdf") {
		t.Fatalf("logs = %q", logs.String())
	}
}

func TestPrincipalWithoutAChatIsRefusedBeforeAnyUpload(t *testing.T) {
	documents := &telegram{}
	service, _ := newService(t, allowlist{chats: map[string]int64{"alice": 101}}, documents)
	content := newBody("payslip")

	err := service.SendFile(t.Context(), "bob", delivery.File{Name: "payslip.pdf", Size: 7, Body: content})

	if !errors.Is(err, delivery.ErrNoChat) {
		t.Fatalf("error = %v, want ErrNoChat", err)
	}
	if len(documents.uploads) != 0 {
		t.Fatalf("uploads = %+v", documents.uploads)
	}
	if content.closes != 1 {
		t.Fatalf("body closes = %d", content.closes)
	}
}

func TestUploadFailureReachesTheCaller(t *testing.T) {
	refused := errors.New("Telegram refused the document")
	documents := &telegram{err: refused}
	service, logs := newService(t, allowlist{chats: map[string]int64{"alice": 101}}, documents)
	content := newBody("payslip")

	err := service.SendFile(t.Context(), "alice", delivery.File{Name: "payslip.pdf", Size: 7, Body: content})

	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the upload failure", err)
	}
	if errors.Is(err, delivery.ErrNoChat) {
		t.Fatalf("an upload failure reads as a missing chat: %v", err)
	}
	if content.closes != 1 {
		t.Fatalf("body closes = %d", content.closes)
	}
	if strings.Contains(logs.String(), "delivery.file.sent") {
		t.Fatalf("a failed upload was logged as sent: %q", logs.String())
	}
}

func TestIncompleteFileIsRefusedAndClosed(t *testing.T) {
	documents := &telegram{}
	service, _ := newService(t, allowlist{chats: map[string]int64{"alice": 101}}, documents)
	content := newBody("")

	err := service.SendFile(t.Context(), "alice", delivery.File{Name: "empty.pdf", Body: content})

	if err == nil {
		t.Fatal("an empty file was accepted")
	}
	if len(documents.uploads) != 0 {
		t.Fatalf("uploads = %+v", documents.uploads)
	}
	if content.closes != 1 {
		t.Fatalf("body closes = %d", content.closes)
	}
}
