package attachment_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

func TestAFailedStageLeavesNoFilesBehind(t *testing.T) {
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	directory := filepath.Join(t.TempDir(), "attachments")
	service, err := attachment.NewService(attachment.Config{Directory: directory}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Stage("alice", "request-1", []attachment.Incoming{
		{Kind: attachment.KindFile, FileName: "first.pdf", MimeType: "application/pdf", Data: []byte("first")},
		{Kind: attachment.KindFile, FileName: "second.pdf", MimeType: "application/pdf", Data: []byte("second")},
		{Kind: attachment.KindFile, FileName: "empty.pdf", MimeType: "application/pdf"},
	})
	if err == nil {
		t.Fatal("staging an empty file succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(directory, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed stage left %d file(s) behind", len(entries))
	}
}
