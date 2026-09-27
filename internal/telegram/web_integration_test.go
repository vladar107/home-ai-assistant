package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/runtime/pi"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/testutil/pifixture"
)

// newWebFixtureRuntime is the fixture runtime with a stand-in search extension; the fixture never
// loads it, it only records the arguments naming it.
func newWebFixtureRuntime(t *testing.T, profilesDir string) (*pi.Runtime, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(t.TempDir(), "index.ts")
	if err := os.WriteFile(extension, []byte("export default function () {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := pi.New(t.Context(), pi.Config{
		Binary: binary, Provider: "fixture-provider", Model: "fixture-model:low",
		ExpectedVersion: pifixture.Version, ProfilesDir: profilesDir, WebSearchExtension: extension,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, extension
}

// latestReport reads what the fixture recorded for the principal's most recent turn.
func latestReport(t *testing.T, profilesDir, principal string) pifixture.Report {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(profilesDir, principal, "sessions", "*.fixture.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixture report for %s: %v", principal, err)
	}
	slices.SortFunc(paths, func(a, b string) int {
		infoA, errA := os.Stat(a)
		infoB, errB := os.Stat(b)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		return infoA.ModTime().Compare(infoB.ModTime())
	})
	data, err := os.ReadFile(paths[len(paths)-1])
	if err != nil {
		t.Fatal(err)
	}
	var report pifixture.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func extensions(report pifixture.Report) []string {
	var named []string
	for index, arg := range report.Args {
		if arg == "--extension" && index+1 < len(report.Args) {
			named = append(named, report.Args[index+1])
		}
	}
	return named
}

func assertToolLess(t *testing.T, report pifixture.Report) {
	t.Helper()
	if !slices.Contains(report.Args, "--no-tools") || len(extensions(report)) != 0 {
		t.Fatalf("turn without web access started with %v", report.Args)
	}
}

func TestWebAccessHiddenWhileDisabledGlobally(t *testing.T) {
	runtime := newControlledRuntime()
	h := newConversationHarness(t, runtime)
	h.provision("alice", "account-alice")
	h.start()

	settings := h.requestSettings(1, 101)
	if strings.Contains(settings.text, "Web access") || slices.ContainsFunc(buttonData(settings.keyboard), func(data string) bool {
		return strings.HasPrefix(data, webSectionPrefix+":")
	}) {
		t.Fatalf("settings showed web access while it is disabled: %+v", settings)
	}
	h.push(textUpdate(2, 101, "hello"))
	call := receive(t, h.ctx, runtime.calls)
	if call.request.WebSearch {
		t.Fatal("a turn asked for web search while web access is disabled")
	}
	call.answer <- agentruntime.RunResult{Response: "hi"}
	h.wantReply(101, 2, "hi")
}

func assertWebTurn(t *testing.T, report pifixture.Report, extension string) {
	t.Helper()
	if slices.Contains(report.Args, "--no-tools") || !slices.Contains(report.Args, "--no-builtin-tools") || !slices.Contains(report.Args, "--no-extensions") {
		t.Fatalf("web access turn flags = %v", report.Args)
	}
	named := extensions(report)
	if len(named) != 2 || filepath.Base(named[0]) != "bridge.ts" || named[1] != extension {
		t.Fatalf("extensions = %v, want the bridge and then the search extension", named)
	}
	var offered []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(report.Tools, &offered); err != nil {
		t.Fatalf("tools.json = %s: %v", report.Tools, err)
	}
	var names []string
	for _, tool := range offered {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "fetch_page") || !slices.Contains(names, "weather_forecast") {
		t.Fatalf("bridge tools = %v", names)
	}
}

func TestWebAccessSwitchControlsSearchAndWebTools(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), "profiles")
	runtime, extension := newWebFixtureRuntime(t, profiles)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{profilesDir: profiles, web: true})
	h.provision("alice", "account-alice")
	h.provision("bob", "account-bob")
	h.start()

	h.push(textUpdate(1, 101, "hello"))
	h.wantReply(101, 1, "reply: hello")
	h.waitState(1, "delivered")
	assertWebTurn(t, latestReport(t, profiles, "alice"), extension)
	h.push(textUpdate(2, 101, "/privacy"))
	h.wantReply(101, 2, ConversationPrivacyReply+" "+AttachmentPrivacyLine+" "+WebPrivacyLine)

	settings := h.requestSettings(3, 101)
	if !strings.Contains(settings.text, WebOnLine) || !slices.Contains(buttonData(settings.keyboard), "web:off") || slices.Contains(buttonData(settings.keyboard), "web:on") {
		t.Fatalf("settings by default = %+v", settings)
	}

	h.push(textUpdate(4, 101, "fixture:web-search"))
	h.wantReply(101, 4, "Searching.\n\nfound: synthetic search answer")
	h.waitState(4, "delivered")
	if logs := h.logs.String(); !strings.Contains(logs, "conversation.web_search.called") || !strings.Contains(logs, "principal_id=alice tool=web_search") {
		t.Fatalf("search call not logged: %s", logs)
	}
	assertWebTurn(t, latestReport(t, profiles, "alice"), extension)

	// The real guard refuses a private address before any connection is made.
	h.push(textUpdate(5, 101, `fixture:call fetch_page {"url":"http://127.0.0.1/"}`))
	refused := h.nextMessage()
	if refused.chatID != 101 || !strings.Contains(refused.text, "error:") || !strings.Contains(refused.text, "private network") {
		t.Fatalf("fetch of a private address = %+v", refused)
	}
	h.waitState(5, "delivered")

	edit := h.press(6, 101, settingsMessageID(3), "web:off")
	if !strings.Contains(edit.text, WebOffLine) || !slices.Contains(buttonData(edit.keyboard), "web:on") || slices.Contains(buttonData(edit.keyboard), "web:off") {
		t.Fatalf("settings after turning web access off = %+v", edit)
	}
	h.push(textUpdate(7, 101, "hello again"))
	h.wantReply(101, 7, "reply: hello again")
	h.waitState(7, "delivered")
	assertToolLess(t, latestReport(t, profiles, "alice"))
	h.push(textUpdate(8, 101, "/privacy"))
	h.wantReply(101, 8, ConversationPrivacyReply+" "+AttachmentPrivacyLine)

	h.push(textUpdate(9, 202, "hello"))
	h.wantReply(202, 9, "reply: hello")
	h.waitState(9, "delivered")
	assertWebTurn(t, latestReport(t, profiles, "bob"), extension)
	h.push(textUpdate(10, 202, "/privacy"))
	h.wantReply(202, 10, ConversationPrivacyReply+" "+AttachmentPrivacyLine+" "+WebPrivacyLine)

	h.requestSettings(11, 101)
	if edit := h.press(12, 101, settingsMessageID(11), "web:on"); !strings.Contains(edit.text, WebOnLine) || !slices.Contains(buttonData(edit.keyboard), "web:off") {
		t.Fatalf("settings after turning web access back on = %+v", edit)
	}
	h.push(textUpdate(13, 101, "hello once more"))
	h.wantReply(101, 13, "reply: hello once more")
	h.waitState(13, "delivered")
	assertWebTurn(t, latestReport(t, profiles, "alice"), extension)
}

func TestScheduledTurnCarriesWebAccess(t *testing.T) {
	runtime := newControlledRuntime()
	clock := newTestClock(t, mondayMorning)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{schedules: clock, web: true})
	h.provision("alice", "account-alice")
	h.start()
	job := h.createJob("alice", "00000000000000ee", "Weather", "0 9 * * *", "Weather and news.")

	clock.Set(t, "2026-09-28T09:00:05+02:00")
	h.scheduler.Wake()
	run := receive(t, h.ctx, runtime.calls)
	if names := toolNames(run.request.Tools); !run.request.WebSearch || !slices.Contains(names, "fetch_page") || !slices.Contains(names, "weather_forecast") {
		t.Fatalf("scheduled turn: web search %v, tools %v", run.request.WebSearch, names)
	}
	run.answer <- agentruntime.RunResult{Response: "Sunny."}
	h.wantMessage(101, "<b>Scheduled: Weather</b>\n\nSunny.")
	h.waitRun(job.ID, schedule.StatusDelivered)
}
