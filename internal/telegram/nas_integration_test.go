package telegram

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
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

// The settings section is the whole user-facing surface of the connector: it names the folders a
// user can reach and switches saving on and off. There is nothing to connect or disconnect,
// because access is a set of mounted directories rather than an account.
func TestSettingsShowsNASFoldersAndSavingSwitch(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{nasRoots: nasRootsFor(t, true)})
	h.start()
	h.provision("alice", "account-alice")

	settings := h.requestSettings(1, 101)
	if !strings.Contains(settings.text, "NAS: home, inbox. Saving off, to inbox.") {
		t.Fatalf("settings = %+v", settings)
	}
	if !slices.Contains(buttonData(settings.keyboard), "nas:writes-on") {
		t.Fatalf("no saving switch = %+v", settings.keyboard)
	}

	enabled := h.press(2, 101, settings.messageID, "nas:writes-on")
	if !strings.Contains(enabled.text, "Saving on, to inbox.") || !slices.Contains(buttonData(enabled.keyboard), "nas:writes-off") {
		t.Fatalf("settings after enabling saving = %+v", enabled)
	}
	h.wantMessage(101, NASWritesOnReply)

	disabled := h.press(3, 101, settings.messageID, "nas:writes-off")
	if !strings.Contains(disabled.text, "Saving off, to inbox.") {
		t.Fatalf("settings after disabling saving = %+v", disabled)
	}
	h.wantMessage(101, NASWritesOffReply)
}

// A user with no writable root must not be offered a switch that cannot do anything.
func TestSettingsHidesSavingSwitchWithoutAWritableRoot(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{nasRoots: nasRootsFor(t, false)})
	h.start()
	h.provision("alice", "account-alice")

	settings := h.requestSettings(1, 101)
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

	settings := h.requestSettings(1, 101)
	if !strings.Contains(settings.text, NASNoAccessLine) {
		t.Fatalf("settings for an unconfigured user = %+v", settings)
	}
	if slices.Contains(buttonData(settings.keyboard), "nas:writes-on") {
		t.Fatalf("a saving switch appeared for an unconfigured user: %+v", settings.keyboard)
	}
}
