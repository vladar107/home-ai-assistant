package telegram

import (
	"slices"
	"strings"
	"testing"
	"time"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

// coreToolNames are the tools every chat turn is offered without an integration, in the order the
// application registers them: pictures, withdrawing a proposal, reactions, jobs and reminders,
// memory, trips, and the backlog.
var coreToolNames = []string{
	"generate_image", "proposal_withdraw", "react",
	"schedule_list", "schedule_get", "reminder_set", "reminder_cancel", "schedule_propose", "schedule_update", "schedule_remove",
	"memory_add", "memory_replace", "memory_remove",
	"trip_list", "trip_add", "trip_update",
	"backlog_add", "backlog_list", "backlog_update",
}

// chatTools is a chat turn's tool list: a test's own tools first, then the core tools, then the
// integrations' tools, and app_help last.
func chatTools(own []string, integrations ...string) []string {
	return slices.Concat(own, coreToolNames, integrations, []string{"app_help"})
}

// privacyLines is the /privacy answer of a conversation with these lines below the opening
// paragraph. Attachments, jobs, voice, and memory are always among them, as the application always
// runs them.
func privacyLines(lines ...string) string {
	return ConversationPrivacyReply + privacyItem + modelUsagePrivacyLine + privacyItem + strings.Join(lines, privacyItem)
}

// onlyCoreTools tells whether a turn carries the core tools alone, as one without any granted
// integration does.
func onlyCoreTools(set agentruntime.ToolSet) bool {
	return slices.Equal(toolNames(set), chatTools(nil))
}

// /help and an unknown command list exactly what is registered, plus the commands answered only
// when typed.
func TestHelpListsTheRegisteredCommands(t *testing.T) {
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{schedules: newTestClock(t, mondayMorning)})
	h.start()
	registration := receive(t, h.ctx, h.registrations)

	want := HelpHeading
	for _, command := range append(registration.commands, typedCommands...) {
		want += "\n/" + command.Command + ": " + command.Description
	}
	want += "\n" + BacklogHelpLine + "\n\n" + HelpConversationLine
	if !strings.Contains(want, "\n/jobs: ") {
		t.Fatalf("registered commands = %+v, want /jobs among them", registration.commands)
	}
	h.push(textUpdate(1, 101, "/help"), textUpdate(2, 101, "/unknown"))
	h.wantReply(101, 1, want)
	h.wantReply(101, 2, want)
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
