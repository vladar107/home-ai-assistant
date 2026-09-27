package telegram

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

var attachmentIDPattern = regexp.MustCompile(`id ([0-9a-f]{8})`)

func TestPhotoReachesTheModelAsAnImage(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	picture := []byte("\xff\xd8\xff\xe0 pretend JPEG bytes")
	h.addFile("photo-1", "photos/file_1.jpg", picture)
	h.push(photoUpdate(1, 101, "what is on this label?", "photo-1", int64(len(picture))))

	call := receive(t, h.ctx, runtime.calls)
	if len(call.request.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(call.request.Images))
	}
	image := call.request.Images[0]
	if !bytes.Equal(image.Data, picture) || image.MimeType != "image/jpeg" || image.Name != "photo.jpg" {
		t.Fatalf("image = {name:%q mime:%q bytes:%d}, want photo.jpg as image/jpeg with %d bytes",
			image.Name, image.MimeType, len(image.Data), len(picture))
	}
	if !strings.Contains(call.request.Message, "what is on this label?") ||
		!strings.Contains(call.request.Message, "attached to this message") {
		t.Fatalf("turn message = %q, want the caption and the image note", call.request.Message)
	}

	call.answer <- agentruntime.RunResult{Response: "a jar of amber honey"}
	h.wantReply(101, 1, "a jar of amber honey")
	h.waitState(1, "delivered")
}

func TestPhotoWithoutCaptionStillStartsATurn(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	picture := []byte("\xff\xd8\xff\xe0 another photo")
	h.addFile("photo-2", "photos/file_2.jpg", picture)
	h.push(photoUpdate(1, 101, "", "photo-2", int64(len(picture))))

	call := receive(t, h.ctx, runtime.calls)
	if len(call.request.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(call.request.Images))
	}
	if strings.TrimSpace(call.request.Message) == "" {
		t.Fatal("a photo without a caption must still describe itself to the model")
	}
	call.answer <- agentruntime.RunResult{Response: "a photo of a jar"}
	h.wantReply(101, 1, "a photo of a jar")
}

func TestDocumentIsReadOnlyWhenTheModelAsksForIt(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	content := []byte("# Notes\n\nbuy amber honey\n")
	h.addFile("doc-1", "documents/file_3.md", content)
	h.push(documentUpdate(1, 101, "file this for me", MediaFile{
		FileID: "doc-1", FileName: "notes.md", MimeType: "text/markdown", FileSize: int64(len(content)),
	}))

	call := receive(t, h.ctx, runtime.calls)
	if len(call.request.Images) != 0 {
		t.Fatalf("images = %d, want none for a document", len(call.request.Images))
	}
	if strings.Contains(call.request.Message, "buy amber honey") {
		t.Fatalf("document content reached the turn message: %q", call.request.Message)
	}
	if !strings.Contains(call.request.Message, "notes.md") || !strings.Contains(call.request.Message, "read_attachment") {
		t.Fatalf("turn message = %q, want the file named and its tool offered", call.request.Message)
	}
	matches := attachmentIDPattern.FindStringSubmatch(call.request.Message)
	if matches == nil {
		t.Fatalf("turn message = %q, want an attachment id", call.request.Message)
	}
	if call.request.Tools.Empty() {
		t.Fatal("a conversation holding a file must offer the reading tools")
	}

	listed := executeTool(t, h.ctx, call.request.Tools, "list_attachments", `{}`)
	if listed.IsError || !strings.Contains(listed.Text, "notes.md") {
		t.Fatalf("list_attachments = %+v, want the stored file", listed)
	}
	read := executeTool(t, h.ctx, call.request.Tools, "read_attachment", `{"attachment_id":"`+matches[1]+`"}`)
	if read.IsError || !strings.Contains(read.Text, "buy amber honey") {
		t.Fatalf("read_attachment = %+v, want the file content", read)
	}
	missing := executeTool(t, h.ctx, call.request.Tools, "read_attachment", `{"attachment_id":"deadbeef"}`)
	if !missing.IsError {
		t.Fatalf("read_attachment of an unknown id = %+v, want an error", missing)
	}

	call.answer <- agentruntime.RunResult{Response: "filed"}
	h.wantReply(101, 1, "filed")
	h.waitState(1, "delivered")
}

func TestPDFIsReadAsTextFromACacheThatNewRemoves(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	handbook, err := os.ReadFile(filepath.Join("..", "pdftext", "testdata", "handbook.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	h.addFile("doc-pdf", "documents/file_5.pdf", handbook)
	h.push(documentUpdate(1, 101, "what does page 5 say?", MediaFile{
		FileID: "doc-pdf", FileName: "handbook.pdf", MimeType: "application/pdf", FileSize: int64(len(handbook)),
	}))

	call := receive(t, h.ctx, runtime.calls)
	if !strings.Contains(call.request.Message, "handbook.pdf") || !strings.Contains(call.request.Message, "read_attachment") ||
		strings.Contains(call.request.Message, "cannot be read") {
		t.Fatalf("turn message = %q, want the PDF offered as readable", call.request.Message)
	}
	id := attachmentIDPattern.FindStringSubmatch(call.request.Message)[1]

	first := executeTool(t, h.ctx, call.request.Tools, "read_attachment", `{"attachment_id":"`+id+`"}`)
	next := regexp.MustCompile(`of the extracted text; continue with offset_bytes (\d+)`).FindStringSubmatch(first.Text)
	if first.IsError || next == nil || !strings.Contains(first.Text, "Household handbook, page 1") {
		t.Fatalf("first window = %+v", first)
	}
	caches, err := filepath.Glob(filepath.Join(h.attachmentsDir, "*", id+".txt"))
	if err != nil || len(caches) != 1 {
		t.Fatalf("text caches = %v %v, want one beside the PDF", caches, err)
	}
	if info, err := os.Stat(caches[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("text cache = %v %v, want mode 0600", info, err)
	}
	rest := executeTool(t, h.ctx, call.request.Tools, "read_attachment", `{"attachment_id":"`+id+`","offset_bytes":`+next[1]+`}`)
	if rest.IsError || !strings.Contains(rest.Text, "Household handbook, page 5") || strings.Contains(rest.Text, "continue with offset_bytes") {
		t.Fatalf("second window = %+v", rest)
	}
	// Later windows come from the cache rather than from parsing the PDF again.
	if err := os.WriteFile(caches[0], []byte("text served from the cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cached := executeTool(t, h.ctx, call.request.Tools, "read_attachment", `{"attachment_id":"`+id+`"}`); !strings.Contains(cached.Text, "text served from the cache") {
		t.Fatalf("read after caching = %+v, want the cached text", cached)
	}

	call.answer <- agentruntime.RunResult{Response: "page 5 is about the budget"}
	h.wantReply(101, 1, "page 5 is about the budget")
	h.waitState(1, "delivered")

	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)
	if files := countFiles(t, h.attachmentsDir); files != 0 {
		t.Fatalf("attachment files on disk = %d after /new, want neither the PDF nor its text", files)
	}
}

func TestScannedPDFIsReportedAsHavingNoText(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	scanned, err := os.ReadFile(filepath.Join("..", "pdftext", "testdata", "scanned.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	h.addFile("doc-scan", "documents/file_6.pdf", scanned)
	h.push(documentUpdate(1, 101, "what does this letter say?", MediaFile{
		FileID: "doc-scan", FileName: "letter.pdf", MimeType: "application/pdf", FileSize: int64(len(scanned)),
	}))

	call := receive(t, h.ctx, runtime.calls)
	id := attachmentIDPattern.FindStringSubmatch(call.request.Message)[1]
	read := executeTool(t, h.ctx, call.request.Tools, "read_attachment", `{"attachment_id":"`+id+`"}`)
	if !read.IsError || !strings.Contains(read.Text, "probably a scanned document") {
		t.Fatalf("read_attachment = %+v, want the PDF reported as scanned", read)
	}

	call.answer <- agentruntime.RunResult{Response: "it is a scan"}
	h.wantReply(101, 1, "it is a scan")
}

func TestUnsupportedAndOversizedMediaAreAnswered(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	h.push(voiceUpdate(1, 101))
	h.wantReply(101, 1, UnsupportedAudioReply)

	h.push(documentUpdate(2, 101, "", MediaFile{
		FileID: "doc-huge", FileName: "backup.zip", MimeType: "application/zip", FileSize: 11 << 20,
	}))
	h.wantReply(101, 2, "That file is too large for me to hold; files must stay under 10 MB.")

	// Neither message may have started a turn, which the next text message proves by arriving first.
	h.push(textUpdate(3, 101, "are you there?"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "are you there?" {
		t.Fatalf("runtime received %q, want the only message that could become a turn", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: "still here"}
	h.wantReply(101, 3, "still here")
}

func TestFilesOfARefusedMessageAreNotKept(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	first := []byte("first note\n")
	h.addFile("doc-a", "documents/file_a.txt", first)
	h.push(documentUpdate(1, 101, "read this", MediaFile{
		FileID: "doc-a", FileName: "a.txt", MimeType: "text/plain", FileSize: int64(len(first)),
	}))
	call := receive(t, h.ctx, runtime.calls)

	// The turn is still running, so the next message is refused as busy and keeps nothing.
	second := []byte("second note\n")
	h.addFile("doc-b", "documents/file_b.txt", second)
	h.push(documentUpdate(2, 101, "and this", MediaFile{
		FileID: "doc-b", FileName: "b.txt", MimeType: "text/plain", FileSize: int64(len(second)),
	}))
	h.wantReply(101, 2, conversation.BusyReply)
	if files := countFiles(t, h.attachmentsDir); files != 1 {
		t.Fatalf("attachment files on disk = %d, want only the accepted one", files)
	}

	call.answer <- agentruntime.RunResult{Response: "read"}
	h.wantReply(101, 1, "read")
}

func TestNewSessionRemovesStoredAttachments(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	content := []byte("private note\n")
	h.addFile("doc-2", "documents/file_4.txt", content)
	h.push(documentUpdate(1, 101, "keep this", MediaFile{
		FileID: "doc-2", FileName: "note.txt", MimeType: "text/plain", FileSize: int64(len(content)),
	}))
	call := receive(t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "kept"}
	h.wantReply(101, 1, "kept")
	h.waitState(1, "delivered")
	if stored := storedAttachments(t, h); stored != 1 {
		t.Fatalf("stored attachments = %d, want 1", stored)
	}

	h.push(textUpdate(2, 101, "/new"))
	h.wantReply(101, 2, NewSessionReply)

	if stored := storedAttachments(t, h); stored != 0 {
		t.Fatalf("stored attachments = %d after /new, want none", stored)
	}
	if files := countFiles(t, h.attachmentsDir); files != 0 {
		t.Fatalf("attachment files on disk = %d after /new, want none", files)
	}
}

func executeTool(t *testing.T, ctx context.Context, set agentruntime.ToolSet, name, arguments string) agentruntime.ToolResult {
	t.Helper()
	result, err := set.Executor.Execute(ctx, agentruntime.ToolCall{Name: name, Arguments: []byte(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func storedAttachments(t *testing.T, h *conversationHarness) int {
	t.Helper()
	var count int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM conversation_attachments`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}
