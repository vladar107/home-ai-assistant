package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// nasRootsFor mounts a read-only home and a writable inbox for alice, the way the deployment
// mounts the household's own directories into the container.
func nasRootsFor(t *testing.T, writable bool) []nas.PrincipalRoots {
	t.Helper()
	home, inbox := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "notes.md"), []byte("milk"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []nas.RootPath{{Name: "home", Path: home}}
	principal := nas.PrincipalRoots{PrincipalID: "alice", Roots: roots}
	if writable {
		principal.Roots = append(principal.Roots, nas.RootPath{Name: "inbox", Path: inbox, Writable: true})
		principal.DefaultWrite = "inbox"
	}
	return []nas.PrincipalRoots{principal}
}

func toolNames(set agentruntime.ToolSet) []string {
	names := make([]string, 0, len(set.Definitions))
	for _, definition := range set.Definitions {
		names = append(names, definition.Name)
	}
	return names
}

// The settings section is the whole user-facing surface of the connector: it names the folders a
// user can reach and switches saving on and off. There is nothing to connect or disconnect,
// because access is a set of mounted directories rather than an account.
func TestSettingsShowsNASFoldersAndSavingSwitch(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{nasRoots: nasRootsFor(t, true)})
	h.start()
	h.provision("alice", "account-alice")

	overview := h.requestSettings(1, 101)
	if !strings.Contains(overview.text, "Home &amp; files: NAS saving off") || !slices.Contains(buttonData(overview.keyboard), "nav:home") {
		t.Fatalf("settings overview = %+v", overview)
	}
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "home")
	if !strings.Contains(settings.text, "NAS: home, inbox. Saving off, to inbox.") {
		t.Fatalf("settings = %+v", settings)
	}
	if !slices.Contains(buttonData(settings.keyboard), "nas:writes-on") {
		t.Fatalf("no saving switch = %+v", settings.keyboard)
	}

	enabled := h.press(3, 101, settingsMessageID(1), "nas:writes-on")
	if !strings.Contains(enabled.text, "Saving on, to inbox.") || !strings.Contains(enabled.text, NASWritesOnReply) || !slices.Contains(buttonData(enabled.keyboard), "nas:writes-off") || !slices.Contains(buttonData(enabled.keyboard), "nav:root") {
		t.Fatalf("settings after enabling saving = %+v", enabled)
	}

	disabled := h.press(4, 101, settingsMessageID(1), "nas:writes-off")
	if !strings.Contains(disabled.text, "Saving off, to inbox.") || !strings.Contains(disabled.text, NASWritesOffReply) {
		t.Fatalf("settings after disabling saving = %+v", disabled)
	}
}

// A user with no writable root must not be offered a switch that cannot do anything.
func TestSettingsHidesSavingSwitchWithoutAWritableRoot(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{nasRoots: nasRootsFor(t, false)})
	h.start()
	h.provision("alice", "account-alice")

	h.requestSettings(1, 101)
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "home")
	if !strings.Contains(settings.text, "NAS: home (read-only).") || !strings.Contains(settings.text, NASSavingUnavailableLine) {
		t.Fatalf("settings without a writable root = %+v", settings)
	}
	if slices.Contains(buttonData(settings.keyboard), "nas:writes-on") {
		t.Fatalf("a saving switch appeared with nowhere to write: %+v", settings.keyboard)
	}
}

// A user the operator did not configure sees the NAS section report exactly that, and gets no
// buttons that would imply otherwise, even though another user has folders.
func TestSettingsReportsNoNASFoldersForAnUnconfiguredUser(t *testing.T) {
	roots := nasRootsFor(t, true)
	roots[0].PrincipalID = "someone-else"
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{nasRoots: roots})
	h.start()
	h.provision("alice", "account-alice")

	h.requestSettings(1, 101)
	settings := h.openSettingsPage(2, 101, settingsMessageID(1), "home")
	if !strings.Contains(settings.text, NASNoAccessLine) {
		t.Fatalf("settings for an unconfigured user = %+v", settings)
	}
	if slices.Contains(buttonData(settings.keyboard), "nas:writes-on") {
		t.Fatalf("a saving switch appeared for an unconfigured user: %+v", settings.keyboard)
	}
}

// The whole outbound path in one turn: the model asks for a file by name, the bytes leave the
// mounted root through the real Telegram client, and the user gets the file itself rather than an
// account of it. Nothing is read to make that happen.
func TestSendingANASFileDeliversADocumentToTheChat(t *testing.T) {
	runtime := newControlledRuntime()
	roots := nasRootsFor(t, false)
	home := roots[0].Roots[0].Path
	if err := os.WriteFile(filepath.Join(home, "Payslip.pdf"), []byte("%PDF-1.7 payslip"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{nasRoots: roots})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "send me my payslip"))
	turn := receive(t, h.ctx, runtime.calls)
	if !slices.Contains(toolNames(turn.request.Tools), "nas_send_file") {
		t.Fatalf("turn tools = %+v", toolNames(turn.request.Tools))
	}
	result, err := turn.request.Tools.Executor.Execute(turn.ctx, agentruntime.ToolCall{
		Name:      "nas_send_file",
		Arguments: json.RawMessage(`{"path":"home/Payslip.pdf"}`),
	})
	if err != nil || result.IsError || !strings.Contains(result.Text, "Sent home/Payslip.pdf") {
		t.Fatalf("send = %+v %v", result, err)
	}

	document := h.nextDocument()
	if document.chatID != 101 || document.name != "Payslip.pdf" || string(document.data) != "%PDF-1.7 payslip" {
		t.Fatalf("document = %+v", document)
	}
	// The model never saw the bytes; only Telegram did.
	if strings.Contains(result.Text, "%PDF") {
		t.Fatalf("the file's content reached the model: %+v", result)
	}

	turn.answer <- agentruntime.RunResult{Response: "Here is your payslip."}
	h.wantReply(101, 1, "Here is your payslip.")
	h.waitState(1, "delivered")
}

// A file the assistant cannot deliver must leave the model able to say so, and must not put a
// document in the chat.
func TestARefusedSendDeliversNoDocument(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{nasRoots: nasRootsFor(t, false)})
	h.provision("alice", "account-alice")
	h.start()

	h.push(textUpdate(1, 101, "send me the second folder"))
	turn := receive(t, h.ctx, runtime.calls)
	result, err := turn.request.Tools.Executor.Execute(turn.ctx, agentruntime.ToolCall{
		Name:      "nas_send_file",
		Arguments: json.RawMessage(`{"path":"home/nothing-here.pdf"}`),
	})
	if err != nil || !result.IsError {
		t.Fatalf("send of a missing file = %+v %v", result, err)
	}
	turn.answer <- agentruntime.RunResult{Response: "I could not find that file."}
	h.wantReply(101, 1, "I could not find that file.")
	h.waitState(1, "delivered")
	if h.documentCount() != 0 {
		t.Fatalf("a refused send delivered %d documents", h.documentCount())
	}
}
