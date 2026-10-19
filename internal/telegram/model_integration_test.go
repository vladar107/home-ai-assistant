package telegram

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const modelCatalogFile = `{"openai-codex":{"models":[
 {"id":"gpt-test","name":"Test","reasoning":true,"contextWindow":200000,"cost":{"Input":1,"Output":2},"thinkingLevelMap":{"off":null}},
 {"id":"gpt-6.1-sol","name":"Sol","reasoning":true,"contextWindow":1000000,"cost":{"Input":5,"Output":10},"thinkingLevelMap":{"off":null,"minimal":null,"xhigh":"xhigh"}},
 {"id":"gpt-7-terra","name":"Terra","reasoning":true,"contextWindow":400000,"cost":{"Input":1,"Output":4}},
 {"id":"codex-7-terra","name":"Terra Code","reasoning":true,"contextWindow":400000,"cost":{"Input":1,"Output":4}}
]}}`

type cachedUsage struct{ unavailable map[string]bool }

func (cachedUsage) Usage(context.Context, string) (chatgpt.Usage, error) {
	panic("/model must not read usage live")
}

func (c cachedUsage) CachedUsage(string, time.Duration) (chatgpt.Usage, bool) {
	available := map[string]bool{}
	for id, off := range c.unavailable {
		available[id] = !off
	}
	return chatgpt.Usage{ModelAvailable: available}, true
}

func newModelHarness(t *testing.T, catalog bool) (*conversationHarness, *controlledRuntime) {
	t.Helper()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true})
	h.provision("alice", "acct-alice")
	if catalog {
		if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(modelCatalogFile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.service.model.Usage = cachedUsage{unavailable: map[string]bool{"gpt-7-terra": true}}
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	return h, runtime
}

// say runs one turn in a tab (0 for the main view) and returns the model and effort it ran on.
func (h *conversationHarness) say(runtime *controlledRuntime, updateID, threadID int64) agentruntime.ModelEffort {
	h.t.Helper()
	if threadID > 0 {
		h.push(tabMessage(updateID, threadID, "hello"))
	} else {
		h.push(textUpdate(updateID, 101, "hello"))
	}
	call := receive(h.t, h.ctx, runtime.calls)
	call.answer <- agentruntime.RunResult{Response: "hi"}
	if answer := h.nextMessage(); answer.threadID != threadID {
		h.t.Fatalf("answer = %+v, want tab %d", answer, threadID)
	}
	// The answer is sent before the turn is marked delivered, and a message in between is refused
	// as busy.
	h.waitState(updateID, "delivered")
	return agentruntime.ModelEffort{Model: call.request.Model, Effort: call.request.Effort}
}

func (h *conversationHarness) modelCommand(updateID int64, text string) sentMessage {
	h.t.Helper()
	h.push(textUpdate(updateID, 101, text))
	return h.nextMessage()
}

func (h *conversationHarness) modelPress(updateID, messageID int64, data string) editedMessage {
	h.t.Helper()
	h.push(callbackUpdate(updateID, 101, messageID, data))
	h.wantAnswer(updateID)
	return h.nextEdit()
}

func TestModelListsTheCatalogWithTheStateOfTheLane(t *testing.T) {
	t.Parallel()
	h, _ := newModelHarness(t, true)
	reply := h.modelCommand(1, "/model")
	if reply.replyTo != requestMessageID(1) {
		t.Fatalf("reply = %+v, want it to quote the request", reply)
	}
	for _, want := range []string{
		"Here: gpt-test, effort medium (the default)",
		"Background tasks: gpt-6.1-sol, effort medium",
		"gpt-test: Test, 200k context, lowest cost. Effort: minimal, low, medium, high (current)",
		"gpt-6.1-sol: Sol, 1M context, 5x cost. Effort: low, medium, high, xhigh\n",
		"codex-7-terra: Terra Code, 400k context, 2x cost. Effort: off, minimal, low, medium, high\n",
		"gpt-7-terra: Terra, 400k context, 2x cost. Effort: off, minimal, low, medium, high (not available on your plan right now)",
	} {
		if !strings.Contains(reply.text, want) {
			t.Fatalf("/model reply lacks %q:\n%s", want, reply.text)
		}
	}
	want := []string{"mdl:m:gpt-test", "mdl:m:gpt-6.1-sol", "mdl:m:gpt-7-terra", "mdl:m:codex-7-terra", "mdl:d"}
	if data := buttonData(reply.keyboard); !slices.Equal(data, want) {
		t.Fatalf("/model buttons = %v, want %v", data, want)
	}
	for _, data := range buttonData(reply.keyboard) {
		if len(data) > maximumCallbackDataBytes {
			t.Fatalf("callback data %q is %d bytes", data, len(data))
		}
	}
}

func TestModelButtonsChooseAModelThenItsLevels(t *testing.T) {
	t.Parallel()
	h, runtime := newModelHarness(t, true)
	list := h.modelCommand(1, "/model")

	levels := h.modelPress(2, list.messageID, "mdl:m:gpt-6.1-sol")
	if !strings.Contains(levels.text, "Choose the effort for gpt-6.1-sol") || levels.messageID != list.messageID {
		t.Fatalf("level page = %+v", levels)
	}
	want := []string{"mdl:e:gpt-6.1-sol:low", "mdl:e:gpt-6.1-sol:medium", "mdl:e:gpt-6.1-sol:high", "mdl:e:gpt-6.1-sol:xhigh", "mdl:l", "mdl:d"}
	if data := buttonData(levels.keyboard); !slices.Equal(data, want) {
		t.Fatalf("level buttons = %v, want %v", data, want)
	}

	applied := h.modelPress(3, list.messageID, "mdl:e:gpt-6.1-sol:xhigh")
	if applied.text != "Saved: gpt-6.1-sol, effort xhigh, in this conversation until /new." || !slices.Equal(buttonData(applied.keyboard), []string{"mdl:l"}) {
		t.Fatalf("applied page = %+v", applied)
	}
	if ran := h.say(runtime, 4, 0); ran != (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "xhigh"}) {
		t.Fatalf("turn ran on %v", ran)
	}

	back := h.modelPress(5, list.messageID, "mdl:l")
	if !strings.Contains(back.text, "Here: gpt-6.1-sol, effort xhigh") || len(back.keyboard) == 0 {
		t.Fatalf("list after back = %+v", back)
	}
	reset := h.modelPress(6, list.messageID, "mdl:d")
	if reset.text != "Back on the default: gpt-test, effort medium." || !slices.Equal(buttonData(reset.keyboard), []string{"mdl:l"}) {
		t.Fatalf("page after defaults = %+v", reset)
	}
	if ran := h.say(runtime, 7, 0); ran != (agentruntime.ModelEffort{Model: "gpt-test", Effort: "medium"}) {
		t.Fatalf("turn after defaults ran on %v", ran)
	}
}

func TestModelButtonsRefuseWhatTheCatalogDoesNotOffer(t *testing.T) {
	t.Parallel()
	h, _ := newModelHarness(t, true)
	list := h.modelCommand(1, "/model")

	if edit := h.modelPress(2, list.messageID, "mdl:e:gpt-6.1-sol:minimal"); !strings.Contains(edit.text, "gpt-6.1-sol on effort minimal is not available") ||
		!strings.Contains(edit.text, "Here: gpt-test, effort medium (the default)") {
		t.Fatalf("refused level page = %s", edit.text)
	}
	if edit := h.modelPress(3, list.messageID, "mdl:m:ghost"); !strings.Contains(edit.text, "ghost is no longer available") {
		t.Fatalf("unknown model page = %s", edit.text)
	}
	h.push(callbackUpdate(4, 101, list.messageID, "mdl:e:gpt-test"), callbackUpdate(5, 101, list.messageID, "mdl:x"))
	h.wantAnswer(4)
	h.wantAnswer(5)
	h.nextEdit()
	h.nextEdit()
}

func TestModelTypedForms(t *testing.T) {
	t.Parallel()
	h, runtime := newModelHarness(t, true)
	update := int64(0)
	next := func() int64 { update += 2; return update }
	typed := func(text string) string {
		reply := h.modelCommand(next(), text)
		return reply.text
	}
	ranOn := func() agentruntime.ModelEffort { return h.say(runtime, next(), 0) }

	if reply := typed("/model sol high"); reply != "This conversation now runs on gpt-6.1-sol, effort high, until /new." {
		t.Fatalf("/model sol high = %q", reply)
	}
	if ran := ranOn(); ran != (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "high"}) {
		t.Fatalf("ran on %v", ran)
	}
	if reply := typed("/model low"); !strings.Contains(reply, "gpt-6.1-sol, effort low") {
		t.Fatalf("/model low = %q", reply)
	}
	if ran := ranOn(); ran != (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "low"}) {
		t.Fatalf("ran on %v", ran)
	}
	if reply := typed("/model GPT-TEST"); !strings.Contains(reply, "gpt-test, effort low") {
		t.Fatalf("a model without an effort keeps the one in use: %q", reply)
	}
	if reply := typed("/model default"); !strings.Contains(reply, "back on the default: gpt-test, effort medium") {
		t.Fatalf("/model default = %q", reply)
	}
	if ran := ranOn(); ran != (agentruntime.ModelEffort{Model: "gpt-test", Effort: "medium"}) {
		t.Fatalf("ran on %v", ran)
	}
}

func TestModelTypedRefusalsListTheOptions(t *testing.T) {
	t.Parallel()
	h, runtime := newModelHarness(t, true)
	update := int64(0)
	typed := func(text string) string {
		update += 2
		return h.modelCommand(update, text).text
	}
	for text, wants := range map[string][]string{
		"/model nonesuch":      {`I do not know the model "nonesuch"`, "gpt-test", "gpt-6.1-sol", "gpt-7-terra", "codex-7-terra"},
		"/model terra":         {`"terra" matches several models: `, "gpt-7-terra", "codex-7-terra"},
		"/model sol minimal":   {"gpt-6.1-sol does not support effort minimal. It supports: low, medium, high, xhigh."},
		"/model sol loud":      {`"loud" is not an effort level`, "low, medium, high, xhigh"},
		"/model sol low high":  {"Use /model &lt;model&gt; [effort]", "Models: gpt-test, gpt-6.1-sol"},
		"/model sol low extra": {"Effort levels: off, minimal, low, medium, high, xhigh, max"},
		"/model xhigh":         {"does not support effort xhigh. It supports: minimal, low, medium, high."},
	} {
		reply := typed(text)
		for _, want := range wants {
			if !strings.Contains(reply, want) {
				t.Errorf("%s answered %q, want it to contain %q", text, reply, want)
			}
		}
	}
	update += 2
	if ran := h.say(runtime, update, 0); ran != (agentruntime.ModelEffort{Model: "gpt-test", Effort: "medium"}) {
		t.Fatalf("refusals changed the lane: ran on %v", ran)
	}
}

func TestModelChoiceBelongsToTheTab(t *testing.T) {
	t.Parallel()
	h, runtime := newModelHarness(t, true)
	if _, err := h.store.AddTab(h.ctx, "alice", 71, "Reports", thread.NamedByFixed); err != nil {
		t.Fatal(err)
	}

	h.push(tabMessage(3, 71, "/model sol"))
	reply := h.nextMessage()
	if reply.threadID != 71 || !strings.Contains(reply.text, "gpt-6.1-sol, effort medium") {
		t.Fatalf("typed form in a tab = %+v", reply)
	}
	h.push(tabMessage(4, 71, "/model"))
	list := h.nextMessage()
	if list.threadID != 71 || !strings.Contains(list.text, "Here: gpt-6.1-sol, effort medium (your choice, until /new)") {
		t.Fatalf("list in a tab = %+v", list)
	}
	if ran := h.say(runtime, 5, 71); ran.Model != "gpt-6.1-sol" {
		t.Fatalf("tab turn ran on %v", ran)
	}
	if ran := h.say(runtime, 6, 0); ran.Model != "gpt-test" {
		t.Fatalf("main view turn ran on %v, want the default", ran)
	}

	h.push(tabPress(7, 71, list.messageID, "mdl:e:gpt-test:high"))
	h.wantAnswer(7)
	if edit := h.nextEdit(); edit.text != "Saved: gpt-test, effort high, in this conversation until /new." {
		t.Fatalf("press in a tab = %+v", edit)
	}
	if ran := h.say(runtime, 8, 71); ran != (agentruntime.ModelEffort{Model: "gpt-test", Effort: "high"}) {
		t.Fatalf("tab turn after the press ran on %v", ran)
	}
	if ran := h.say(runtime, 9, 0); ran.Effort != "medium" {
		t.Fatalf("main view turn ran on %v", ran)
	}
}

func TestModelAsTheFirstMessageOfAFreshTabIsAnsweredInTheMainView(t *testing.T) {
	t.Parallel()
	h, runtime := newModelHarness(t, true)
	h.push(implicitTab(1, 70), tabMessage(2, 70, "/model sol low"))
	answer := h.nextMessage()
	if answer.threadID != 0 || answer.replyTo != 0 || !strings.Contains(answer.text, "gpt-6.1-sol, effort low") {
		t.Fatalf("answer = %+v, want it in the main view", answer)
	}
	if deleted := receive(t, h.ctx, h.tabs.deleted); deleted != 70 {
		t.Fatalf("deleted tab %d, want 70", deleted)
	}
	h.waitOffset(3)
	if ran := h.say(runtime, 3, 0); ran != (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "low"}) {
		t.Fatalf("main view turn ran on %v", ran)
	}
}

func TestModelOffersOnlyTheDefaultsWithoutACatalog(t *testing.T) {
	t.Parallel()
	h, runtime := newModelHarness(t, false)
	reply := h.modelCommand(1, "/model")
	if !strings.Contains(reply.text, "The model list is unavailable right now") {
		t.Fatalf("/model without a catalog = %s", reply.text)
	}
	if data := buttonData(reply.keyboard); !slices.Equal(data, []string{"mdl:m:gpt-test", "mdl:m:gpt-6.1-sol", "mdl:d"}) {
		t.Fatalf("buttons without a catalog = %v", data)
	}
	if reply := h.modelCommand(2, "/model gpt-6.1-sol"); !strings.Contains(reply.text, "gpt-6.1-sol, effort medium") {
		t.Fatalf("a default's pair is refused: %q", reply.text)
	}
	if reply := h.modelCommand(3, "/model gpt-6.1-sol high"); !strings.Contains(reply.text, "does not support effort high") {
		t.Fatalf("an unlisted pair is accepted: %q", reply.text)
	}
	if ran := h.say(runtime, 4, 0); ran != (agentruntime.ModelEffort{Model: "gpt-6.1-sol", Effort: "medium"}) {
		t.Fatalf("turn ran on %v", ran)
	}
}

func TestModelIsInTheMenuAndHelp(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true})
	h.provision("alice", "acct-alice")
	h.start()
	registration := receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)
	if !slices.Contains(registration.commands, BotCommand{Command: "model", Description: ModelCommandDescription}) {
		t.Fatalf("menu = %+v", registration.commands)
	}
	if help := h.modelCommand(1, "/help").text; !strings.Contains(help, "\n/model: "+ModelCommandDescription) {
		t.Fatalf("help = %s", help)
	}
}

// A catalog entry can come with no level Pi would run, which must be refused rather than crash
// the household's only poller.
func TestModelWithoutUsableLevelsIsRefused(t *testing.T) {
	t.Parallel()
	runtime := newControlledRuntime()
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{topics: true})
	h.provision("alice", "acct-alice")
	catalog := `{"openai-codex":{"models":[{"id":"gpt-none","name":"None","reasoning":true,"contextWindow":1000,` +
		`"thinkingLevelMap":{"off":null,"minimal":null,"low":null,"medium":null,"high":null}}]}}`
	if err := os.WriteFile(filepath.Join(h.profilesDir, "alice", "models-store.json"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	receive(t, h.ctx, h.registrations)
	receive(t, h.ctx, h.registrations)

	if reply := h.modelCommand(1, "/model gpt-none"); reply.text != "gpt-none offers no effort level I can use." {
		t.Fatalf("reply = %q", reply.text)
	}
	if reply := h.modelCommand(2, "/model"); !strings.Contains(reply.text, "gpt-none") {
		t.Fatalf("bare /model after the refusal = %q", reply.text)
	}
}
