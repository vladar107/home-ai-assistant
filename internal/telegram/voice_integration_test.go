package telegram

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/chatgptsim"
	"github.com/vladar107/home-ai-assistant/internal/testutil/codexauth"
)

func TestVoiceMessageBecomesATurnFromItsTranscript(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	h.start()

	recording := []byte("OggS pretend opus frames")
	h.addFile("voice-1", "voice/file_1.oga", recording)
	update := voiceUpdate(1, 101)
	update.Message.Caption = "about dinner"
	h.push(update)

	call := receive(t, h.ctx, runtime.calls)
	if want := voiceMarker + "\nhello from a voice note\n\nabout dinner"; call.request.Message != want {
		t.Fatalf("turn message = %q, want %q", call.request.Message, want)
	}
	requests := sim.Requests()
	if len(requests) != 1 {
		t.Fatalf("ChatGPT requests = %d, want 1", len(requests))
	}
	sent := requests[0]
	if sent.Authorization != "Bearer "+codexauth.AccessToken("acct-alice") || sent.AccountID != "acct-alice" {
		t.Fatalf("transcription credential = %q for %q, want alice's own", sent.Authorization, sent.AccountID)
	}
	// Cloudflare challenges Go's default User-Agent on the transcription path.
	if strings.HasPrefix(sent.UserAgent, "Go-http-client") {
		t.Fatalf("transcription User-Agent = %q, want the application's own", sent.UserAgent)
	}
	if !bytes.Equal(sent.Audio, recording) || sent.FileName != "voice.ogg" || sent.MimeType != "audio/ogg" {
		t.Fatalf("transcription upload = %q as %q (%d bytes), want voice.ogg as audio/ogg with the recording", sent.FileName, sent.MimeType, len(sent.Audio))
	}

	call.answer <- agentruntime.RunResult{Response: "pasta it is"}
	h.wantReply(101, 1, "pasta it is")
}

func TestVoiceFailuresAreAnsweredWithoutATurn(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	h.start()
	h.addFile("voice-1", "voice/file_1.oga", []byte("OggS silence"))

	sim.SetTranscript("  ")
	h.push(voiceUpdate(1, 101))
	h.wantReply(101, 1, NoSpeechReply)

	sim.FailWith(http.StatusTooManyRequests)
	h.push(voiceUpdate(2, 101))
	h.wantReply(101, 2, UsageLimitReply)

	sim.FailWith(http.StatusBadGateway)
	h.push(voiceUpdate(3, 101))
	h.wantReply(101, 3, TranscriptionFailedReply)

	// None of them may have started a turn, which the next text message proves by arriving first.
	h.push(textUpdate(4, 101, "are you there?"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.Message != "are you there?" {
		t.Fatalf("runtime received %q, want the only message that could become a turn", call.request.Message)
	}
	call.answer <- agentruntime.RunResult{Response: "still here"}
	h.wantReply(101, 4, "still here")
}

// A turn that would pass the conversation's limit is refused once instead of being retried, which
// would transcribe the recording again and hold every later update.
func TestVoiceTurnThatWouldNotFitIsRefusedOnce(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	h.start()
	h.addFile("voice-1", "voice/file_1.oga", []byte("OggS a long story"))

	sim.SetTranscript(strings.Repeat("a", 4096))
	h.push(voiceUpdate(1, 101))
	h.wantReply(101, 1, TranscriptTooLongReply)

	transcript := strings.Repeat("words ", 600) + "end"
	sim.SetTranscript(transcript)
	captioned := voiceUpdate(2, 101)
	captioned.Message.Caption = strings.Repeat("c", 1000)
	h.push(captioned)
	h.wantReply(101, 2, CaptionTooLongReply)

	short := voiceUpdate(3, 101)
	short.Message.Caption = "for later"
	h.push(short)
	call := receive(t, h.ctx, runtime.calls)
	if want := voiceMarker + "\n" + transcript + "\n\nfor later"; call.request.Message != want {
		t.Fatalf("turn message = %q, want the fitting recording", call.request.Message)
	}
	if requests := sim.Requests(); len(requests) != 3 {
		t.Fatalf("ChatGPT requests = %d, want each recording transcribed once", len(requests))
	}
	call.answer <- agentruntime.RunResult{Response: "noted"}
	h.wantReply(101, 3, "noted")
}

func TestVoiceWithoutAConnectionIsNotTranscribed(t *testing.T) {
	sim := chatgptsim.New(t)
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{chatGPT: sim})
	h.start()
	h.addFile("voice-1", "voice/file_1.oga", []byte("OggS hello"))

	h.push(voiceUpdate(1, 101))
	h.wantReply(101, 1, "Your model subscription is not connected. Use /settings to connect it.")
	if requests := sim.Requests(); len(requests) != 0 {
		t.Fatalf("ChatGPT requests = %d, want none without the principal's own subscription", len(requests))
	}
}

func TestExpiredTokenIsRefreshedUnderPiLock(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	authPath := filepath.Join(h.profilesDir, "alice", "auth.json")
	expired := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"expired-access","refresh":%q,"expires":%d,"accountId":"acct-alice"},"other":{"type":"api_key","key":"kept"}}`,
		codexauth.RefreshToken("acct-alice"), time.Now().Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(authPath, []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.addFile("voice-1", "voice/file_1.oga", []byte("OggS hello"))

	h.push(voiceUpdate(1, 101))
	call := receive(t, h.ctx, runtime.calls)
	if sent := sim.Requests()[0]; sent.Authorization != "Bearer "+codexauth.AccessToken("acct-alice") {
		t.Fatalf("transcription used %q, want the refreshed token", sent.Authorization)
	}

	contents, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]map[string]any
	if err := json.Unmarshal(contents, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["openai-codex"]["refresh"] != codexauth.RotatedRefreshToken("acct-alice") || stored["other"]["key"] != "kept" {
		t.Fatalf("auth.json = %s, want the rotated refresh token beside the untouched record", contents)
	}
	if _, err := os.Stat(authPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}

	call.answer <- agentruntime.RunResult{Response: "heard you"}
	h.wantReply(101, 1, "heard you")
}

func TestGeneratedImageIsShownInTheChat(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	h.start()

	h.push(textUpdate(1, 101, "draw a lighthouse"))
	call := receive(t, h.ctx, runtime.calls)
	result := executeTool(t, h.ctx, call.request.Tools, "generate_image", `{"prompt":"a lighthouse at dusk"}`)
	if result.IsError || !strings.Contains(result.Text, "shown to the user") {
		t.Fatalf("generate_image = %+v, want the picture reported as shown", result)
	}
	photo := h.nextPhoto()
	if photo.chatID != 101 || !bytes.Equal(photo.data, chatgptsim.PNG) {
		t.Fatalf("photo = chat %d with %d bytes, want the generated picture in alice's chat", photo.chatID, len(photo.data))
	}

	body := sim.Requests()[0].Body
	if body["model"] != "gpt-test" || body["stream"] != true || body["store"] != false {
		t.Fatalf("image request = %v, want the carrier model without its thinking level, streamed and unstored", body)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["action"] != "generate" {
		t.Fatalf("image tool = %v, want a generate action", tool)
	}

	call.answer <- agentruntime.RunResult{Response: "here it is"}
	h.wantReply(101, 1, "here it is")
}

func TestImageEditsAPhotoTheUserSent(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	h.start()

	picture := []byte("\xff\xd8\xff\xe0 a garden photo")
	h.addFile("photo-1", "photos/file_1.jpg", picture)
	h.push(photoUpdate(1, 101, "make this a watercolor", "photo-1", int64(len(picture))))
	call := receive(t, h.ctx, runtime.calls)
	id := attachmentIDPattern.FindStringSubmatch(call.request.Message)[1]

	result := executeTool(t, h.ctx, call.request.Tools, "generate_image", `{"prompt":"this garden as a watercolor","reference_attachment_ids":["`+id+`"]}`)
	if result.IsError {
		t.Fatalf("generate_image = %+v, want success", result)
	}
	h.nextPhoto()

	body := sim.Requests()[0].Body
	content := body["input"].([]any)[0].(map[string]any)["content"].([]any)
	reference := content[1].(map[string]any)
	if reference["type"] != "input_image" || reference["image_url"] != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(picture) {
		t.Fatalf("reference = %v, want the user's photo as a data URL", reference)
	}
	if tool := body["tools"].([]any)[0].(map[string]any); tool["action"] != "edit" {
		t.Fatalf("image tool = %v, want an edit action", tool)
	}

	call.answer <- agentruntime.RunResult{Response: "done"}
	h.wantReply(101, 1, "done")
}

func TestRefusedImageIsReportedToTheModel(t *testing.T) {
	sim := chatgptsim.New(t)
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{chatGPT: sim})
	h.provision("alice", "acct-alice")
	h.start()
	sim.Refuse("I can't create that image.")

	h.push(textUpdate(1, 101, "draw something"))
	call := receive(t, h.ctx, runtime.calls)
	result := executeTool(t, h.ctx, call.request.Tools, "generate_image", `{"prompt":"something"}`)
	if !result.IsError || !strings.Contains(result.Text, "I can't create that image.") {
		t.Fatalf("generate_image = %+v, want the refusal passed on as an error", result)
	}

	call.answer <- agentruntime.RunResult{Response: "sorry"}
	h.wantReply(101, 1, "sorry")
}
