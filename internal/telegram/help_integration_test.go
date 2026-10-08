package telegram

import (
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

// onlyHelp tells whether a turn carries app_help alone, as one without any granted integration does.
func onlyHelp(set agentruntime.ToolSet) bool {
	return slices.Equal(toolNames(set), []string{"app_help"})
}

func TestAppHelpShowsTheSettingsPagesTheUserSees(t *testing.T) {
	runtime := newControlledRuntime()
	sim := notionsim.New(t)
	h := newConversationHarnessWithOptions(t, runtime, harnessOptions{
		ns: nsGateway(t), notion: sim, notionChatConnect: true, schedules: newTestClock(t, mondayMorning), notionIntakeTimeout: 300 * time.Millisecond,
	})
	h.provision("alice", "account-alice")
	h.start()

	h.requestSettings(1, 101)
	trains := h.openSettingsPage(2, 101, settingsMessageID(1), "tools")
	accounts := h.openSettingsPage(3, 101, settingsMessageID(1), "accounts")

	h.push(textUpdate(4, 101, "how do I connect Notion?"))
	call := receive(t, h.ctx, runtime.calls)
	if !slices.Contains(toolNames(call.request.Tools), "app_help") || !strings.Contains(call.request.Tools.Guidance, "call app_help") {
		t.Fatalf("tools = %v", toolNames(call.request.Tools))
	}
	help := func(topic string) agentruntime.ToolResult {
		t.Helper()
		return h.callTool(call, "app_help", `{"topic":"`+topic+`"}`)
	}

	train := help("trains")
	if train.IsError || !strings.Contains(train.Text, `Where: send /settings, then press "Everyday tools".`) || !strings.Contains(train.Text, trains.text) || !strings.Contains(train.Text, "Buttons on it: ") {
		t.Fatalf("trains help = %+v\npage = %s", train, trains.text)
	}
	notion := help("notion")
	if notion.IsError || !strings.Contains(notion.Text, accounts.text) || !strings.Contains(notion.Text, "Pressing Connect Notion sends: "+NotionConnectInstructions) {
		t.Fatalf("notion help = %+v\npage = %s", notion, accounts.text)
	}
	if commands := help("commands"); commands.IsError || !strings.Contains(commands.Text, "/jobs: Show and manage your reminders and scheduled jobs") || !strings.Contains(commands.Text, "/new: Start a fresh conversation") {
		t.Fatalf("commands help = %+v", commands)
	}
	if jobs := help("jobs"); jobs.IsError || !strings.Contains(jobs.Text, NoJobsReply) {
		t.Fatalf("jobs help = %+v", jobs)
	}
	if search := help("web_search"); search.IsError || !strings.Contains(search.Text, "each query is checked") || !strings.Contains(search.Text, "Confirm and Reject") {
		t.Fatalf("web search help = %+v", search)
	}
	if missing := help("buses"); !missing.IsError || !strings.Contains(missing.Text, "topic must be one of") {
		t.Fatalf("help on a section the user does not have = %+v", missing)
	}
	call.answer <- agentruntime.RunResult{Response: "Press Connect Notion."}
	h.wantReply(101, 4, "Press Connect Notion.")
}
