package prompt

import (
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/localtime"
)

// The framing every turn nobody is watching shares, so they all say the same about the user and the
// tools they have.
const (
	unwatched = "The user is not watching this turn and cannot answer questions, and only read-only tools are available"
	readOnly  = ", so nothing can be created, changed, or saved. "
	// scheduledJob opens a scheduled run, which can start after its slot.
	scheduledJob = Marker + " This is the user's scheduled job %q, scheduled for %s; it can run late, so go by the current local time above. "
)

// ScheduledJob tells the model that nobody is waiting on a scheduled job's turn, so it does the
// task and reports instead of asking questions or offering changes it cannot make.
func ScheduledJob(name string, scheduledFor time.Time, location *time.Location, task string) string {
	return fmt.Sprintf(
		scheduledJob+unwatched+readOnly+
			"Never show a status, time, or count that no tool returned in this run, and never carry one over from an earlier run; "+
			"if a check failed, say which one, and leave a leg out rather than guess its status. "+
			"If the task asks to report only when something holds and it does not hold now, reply with exactly %s and nothing else, and nothing is sent; a check that failed is still reported. "+
			"Do the task below and reply with the report the user should receive.\n\n%s",
		name, localtime.Format(scheduledFor, location), NothingToAdd, task,
	)
}

// ScheduledScan frames a scheduled job whose reply the application reads rather than sends, so the
// task itself says how to answer.
func ScheduledScan(name string, scheduledFor time.Time, location *time.Location, task string) string {
	return fmt.Sprintf(scheduledJob+unwatched+". Do the task below.\n\n%s", name, localtime.Format(scheduledFor, location), task)
}

// WatchCheck frames one check of an agent watch; check says what to look at and how to answer. A
// watch's times are local to its own zone, which can differ from the household's.
func WatchCheck(name string, now time.Time, location *time.Location, check string) string {
	return fmt.Sprintf(
		Marker+" This is one check of the user's watch %q, whose times are local to its zone, where it is now %s. "+unwatched+readOnly+
			"Do the check below with the tools you have, and never report a value that no tool returned in this run.\n\n%s",
		name, localtime.Format(now, location), check,
	)
}

// BackgroundTask tells a background task's sub-agent what it is, so it works through the brief
// and reports instead of asking questions or offering changes it cannot make.
func BackgroundTask(title string, calls int, duration, brief string) string {
	return fmt.Sprintf(
		Marker+" You are a background task the user started from chat, titled %q. The user is not watching and cannot answer questions, "+
			"and you cannot see the chat, so the brief below is all you have. Only read-only tools are available, so nothing can be created, "+
			"changed, or saved; if something should change, say so in the report. Work through the brief thoroughly: you may make up to %d "+
			"tool calls and have up to %s. Then reply with the report the user should receive, leading with what they asked for.\n\n%s",
		title, calls, duration, brief,
	)
}

// BackgroundTaskLimit asks a background task that reached a limit for the report it has so far.
func BackgroundTaskLimit(limit string) string {
	return Marker + " This background task has reached its " + limit + ". Do not look anything else up: tool calls will be refused. " +
		"Write the report now from what you have found so far, and say plainly what you did not get to."
}

// BackgroundTaskResume continues a background task a restart interrupted.
const BackgroundTaskResume = Marker + " You were interrupted; continue the task and report when it is complete."
