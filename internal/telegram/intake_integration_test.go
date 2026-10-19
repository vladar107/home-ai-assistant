package telegram

import (
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/chatgptsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/googlesim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

// A message the conversation refuses as invalid is answered and its update committed, so the
// poller moves on to the next one.
func TestInvalidMessageIsAnsweredAndThePollerMovesOn(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, strings.Repeat("a", 4097)))
	h.wantReply(101, 1, InvalidMessageReply)

	h.push(textUpdate(2, 101, "are you there?"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "are you there?" {
		t.Fatalf("runtime received %q, want the message after the refused one", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: "here"}
	h.wantReply(101, 2, "here")
}

// The Notion token guard covers captions and transcripts too, without a photo or recording ever
// completing a pending intake.
func TestNotionTokenGuardCoversCaptionsAndTranscripts(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	notionSim := notionsim.New(t)
	notionSim.AddToken(aliceNotionToken, aliceWorkspace)
	chatSim := chatgptsim.New(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{notion: notionSim, notionChatConnect: true, chatGPT: chatSim})
	h.provision("alice", "acct-alice-1111")
	h.start()

	h.requestSettings(1, 101)
	h.openSettingsPage(2, 101, settingsMessageID(1), "accounts")
	h.press(3, 101, settingsMessageID(1), "notion:connect")
	h.wantMessage(101, NotionConnectInstructions)

	picture := []byte("\xff\xd8\xff\xe0 a screenshot")
	h.addFile("photo-1", "photos/file_1.jpg", picture)
	h.push(photoUpdate(4, 101, aliceNotionToken, "photo-1", int64(len(picture))))
	if deleted := h.nextDelete(); deleted.chatID != 101 || deleted.messageID != requestMessageID(4) {
		t.Fatalf("captioned photo deletion = %+v", deleted)
	}
	h.wantMessage(101, NotionTokenGuardReply)
	if _, exists := h.notionTokenFile("alice"); exists {
		t.Fatal("a photo caption completed the pending intake")
	}

	h.addFile("voice-1", "voice/file_1.oga", []byte("OggS a spoken token"))
	chatSim.SetTranscript("secret_spokenToken1")
	h.push(voiceUpdate(5, 101))
	if deleted := h.nextDelete(); deleted.messageID != requestMessageID(5) {
		t.Fatalf("transcribed token deletion = %+v", deleted)
	}
	h.wantMessage(101, NotionTokenGuardReply)

	chatSim.SetTranscript("here is my token")
	captioned := voiceUpdate(6, 101)
	captioned.Message.Caption = "ntn_captionToken9"
	h.push(captioned)
	if deleted := h.nextDelete(); deleted.messageID != requestMessageID(6) {
		t.Fatalf("voice caption token deletion = %+v", deleted)
	}
	h.wantMessage(101, NotionTokenGuardReply)
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)

	// The intake is still pending for the text message it asked for.
	h.push(textUpdate(7, 101, aliceNotionToken))
	h.nextDelete()
	h.wantMessage(101, NotionConnectedReply(aliceStatus))
	h.assertNotionSecretHidden(aliceNotionToken, "secret_spokenToken1", "ntn_captionToken9")
}

func TestGoogleSignInAddressInACaptionIsGuarded(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{google: googleSimulator(t)})
	h.provision("alice", "account-alice")
	h.start()

	picture := []byte("\xff\xd8\xff\xe0 a browser window")
	h.addFile("photo-1", "photos/file_1.jpg", picture)
	h.push(photoUpdate(1, 101, googlesim.RedirectURL+"?state=x&code=4/caption", "photo-1", int64(len(picture))))
	if deleted := h.nextDelete(); deleted.chatID != 101 || deleted.messageID != requestMessageID(1) {
		t.Fatalf("captioned photo deletion = %+v", deleted)
	}
	h.wantMessage(101, GoogleSignInGuardReply)
	expectNoRuntimeCall(t, runtime, 50*time.Millisecond)
}
