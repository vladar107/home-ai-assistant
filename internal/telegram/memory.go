package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

const (
	MemoryTitle        = "Memory"
	NoMemoryReply      = "I have nothing remembered about you. Tell me in conversation, for example: remember that I prefer short answers."
	MemoryGoneReply    = "That entry no longer exists."
	MemoryOffNote      = "Memory is off: nothing here is used until you turn it on in /settings."
	MemoryClearedNote  = "Forgot everything."
	MemoryOnLine       = "Memory: on (the default). I keep the preferences and facts you ask me to remember, or that I learn in conversation, and use them in every conversation, scheduled run, and background task. See and delete them with /memory."
	MemoryOffLine      = "Memory: off. I do not use or add to what I remembered; the saved entries are kept until you delete them in /memory."
	MemoryLearnOnLine  = "Learn from 👎: on (the default). When you mark a reply 👎 and the mistake would recur, I may propose an instruction to avoid it; it is kept only if you confirm."
	MemoryLearnOffLine = "Learn from 👎: off. A 👎 only makes me look at that reply again."
	MemoryPrivacyLine  = "Memory, including the procedures you kept, is kept until you delete it in /memory and goes to your model with every request while it is on; a procedure's steps go only when I read it. " +
		"For the weekly review I count each day how often each tool's calls succeeded or failed, never what they were asked or answered, and delete the counts after 30 days."

	MemoryProceduresOnLine  = "Learn procedures: on (the default). After a request that took several steps, I may offer to keep how it was done as a procedure, which I read before similar requests; it is kept only if you confirm."
	MemoryProceduresOffLine = "Learn procedures: off. I offer no new procedures; the ones you kept are still used until you delete them in /memory."

	MemoryReviewOnLine      = "Weekly review: on. Every Sunday at 18:00 I look back at the week: the replies you marked 👎, the tools that failed, and your instructions and procedures. I offer at most three changes and two backlog items as cards; nothing changes unless you confirm. Press Review now to look at once. Uses your model subscription."
	MemoryReviewOffLine     = "Weekly review: off (the default). Turn it on and every Sunday evening I look back at the week and offer what is worth learning, at most three changes and two backlog items, as cards; nothing changes unless you confirm. Uses your model subscription."
	MemoryReviewingNote     = "Reviewing the past week now; anything worth learning arrives as cards."
	MemoryReviewBusyNote    = "A review is already running; anything worth learning arrives as cards."
	MemoryReviewTooManyNote = "You already have the maximum number of scheduled jobs. Remove one in /jobs, then turn the weekly review on."

	memorySectionPrefix = "memory"
	memoryOnAction      = "on"
	memoryOffAction     = "off"
	learnOnAction       = "learn_on"
	learnOffAction      = "learn_off"
	proceduresOnAction  = "procedures_on"
	proceduresOffAction = "procedures_off"
	reviewOnAction      = "review_on"
	reviewOffAction     = "review_off"
	reviewNowAction     = "review_now"

	learnedMarker     = " (learned)"
	proceduresHeading = "Procedures"

	memoryCallbackPrefix  = "mem"
	memoryDeleteAction    = "del"
	procedureDeleteAction = "pdel"
	memoryPageAction      = "page"
	memoryAskAction       = "ask"
	memoryClearAction     = "clear"
	memoryBackAction      = "back"

	memoriesPerPage = 8
	deletesPerRow   = 4
)

// MemoryController is implemented by the memory service; nil leaves /memory out. Its /settings
// switch and /privacy line are NewMemorySection's.
type MemoryController interface {
	Entries(context.Context, string) ([]memory.Entry, error)
	Remove(context.Context, string, string) (bool, error)
	Clear(context.Context, string) (int, error)
	Enabled(context.Context, string) (bool, error)
	SetEnabled(context.Context, string, bool) error
	// LearnsFromFeedback is false while memory is off.
	LearnsFromFeedback(context.Context, string) (bool, error)
	SetLearnFromFeedback(context.Context, string, bool) error
	Procedures(context.Context, string) ([]memory.Procedure, error)
	RemoveProcedure(context.Context, string, string) (bool, error)
	// LearnsProcedures is false while memory is off.
	LearnsProcedures(context.Context, string) (bool, error)
	SetLearnProcedures(context.Context, string, bool) error
}

// NewMemorySection is the page that turns the principal's memory, learning from 👎, learning
// procedures, and the weekly review on and off.
func NewMemorySection(memory MemoryController, schedules ScheduleController, logger *slog.Logger, telemetry Telemetry) Section {
	return memorySection{memory: memory, schedules: schedules, logger: logger, telemetry: telemetryOrNoop(telemetry)}
}

type memorySection struct {
	memory    MemoryController
	schedules ScheduleController
	logger    *slog.Logger
	telemetry Telemetry
}

func (m memorySection) prefix() string { return memorySectionPrefix }

func (m memorySection) helpTopic() string { return "memory" }

func (m memorySection) privacy(context.Context, string) ([]string, error) {
	return []string{MemoryPrivacyLine}, nil
}

func (m memorySection) summary(ctx context.Context, principalID string) (settingsSummary, error) {
	enabled, err := m.memory.Enabled(ctx, principalID)
	if err != nil {
		return settingsSummary{}, err
	}
	return settingsSummary{name: "Memory", state: onOff(enabled)}, nil
}

func (m memorySection) render(ctx context.Context, principalID string) (settingsView, error) {
	enabled, err := m.memory.Enabled(ctx, principalID)
	if err != nil {
		return settingsView{}, err
	}
	if enabled {
		learn, err := m.memory.LearnsFromFeedback(ctx, principalID)
		if err != nil {
			return settingsView{}, err
		}
		view := settingsView{
			lines:   []string{MemoryOnLine, MemoryLearnOffLine},
			buttons: []InlineButton{{Text: "Turn off memory", CallbackData: memorySectionPrefix + ":" + memoryOffAction}, {Text: "Learn from 👎", CallbackData: memorySectionPrefix + ":" + learnOnAction}},
		}
		if learn {
			view.lines[1] = MemoryLearnOnLine
			view.buttons[1] = InlineButton{Text: "Stop learning from 👎", CallbackData: memorySectionPrefix + ":" + learnOffAction}
		}
		procedures, err := m.memory.LearnsProcedures(ctx, principalID)
		if err != nil {
			return settingsView{}, err
		}
		if procedures {
			view.lines = append(view.lines, MemoryProceduresOnLine)
			view.rows = [][]InlineButton{{{Text: "Stop learning procedures", CallbackData: memorySectionPrefix + ":" + proceduresOffAction}}}
		} else {
			view.lines = append(view.lines, MemoryProceduresOffLine)
			view.rows = [][]InlineButton{{{Text: "Learn procedures", CallbackData: memorySectionPrefix + ":" + proceduresOnAction}}}
		}
		review, err := m.reviewing(ctx, principalID)
		if err != nil {
			return settingsView{}, err
		}
		if review {
			view.lines = append(view.lines, MemoryReviewOnLine)
			view.rows = append(view.rows, []InlineButton{
				{Text: "Review now", CallbackData: memorySectionPrefix + ":" + reviewNowAction},
				{Text: "Turn off weekly review", CallbackData: memorySectionPrefix + ":" + reviewOffAction},
			})
		} else {
			view.lines = append(view.lines, MemoryReviewOffLine)
			view.rows = append(view.rows, []InlineButton{{Text: "Turn on weekly review", CallbackData: memorySectionPrefix + ":" + reviewOnAction}})
		}
		return view, nil
	}
	return settingsView{
		lines:   []string{MemoryOffLine},
		buttons: []InlineButton{{Text: "Turn on memory", CallbackData: memorySectionPrefix + ":" + memoryOnAction}},
	}, nil
}

func (m memorySection) act(ctx context.Context, principalID string, _ int64, action string) (settingsOutcome, error) {
	switch action {
	case memoryOnAction, memoryOffAction:
		enabled := action == memoryOnAction
		if err := m.memory.SetEnabled(ctx, principalID, enabled); err != nil {
			return settingsOutcome{}, err
		}
		m.telemetry.ContextLogger(ctx, m.logger).InfoContext(ctx, "memory.changed", "principal_id", principalID, "enabled", enabled)
		return settingsOutcome{}, nil
	case learnOnAction, learnOffAction:
		learn := action == learnOnAction
		if err := m.memory.SetLearnFromFeedback(ctx, principalID, learn); err != nil {
			return settingsOutcome{}, err
		}
		m.telemetry.ContextLogger(ctx, m.logger).InfoContext(ctx, "memory.learning.changed", "principal_id", principalID, "learn", learn)
		return settingsOutcome{}, nil
	case proceduresOnAction, proceduresOffAction:
		learn := action == proceduresOnAction
		if err := m.memory.SetLearnProcedures(ctx, principalID, learn); err != nil {
			return settingsOutcome{}, err
		}
		m.telemetry.ContextLogger(ctx, m.logger).InfoContext(ctx, "memory.procedures.changed", "principal_id", principalID, "learn", learn)
		return settingsOutcome{}, nil
	case reviewOnAction, reviewOffAction, reviewNowAction:
		// The review needs memory on, so its buttons do nothing while memory is off.
		enabled, err := m.memory.Enabled(ctx, principalID)
		if err != nil || !enabled {
			return settingsOutcome{}, err
		}
		return m.actOnReview(ctx, principalID, action)
	default:
		return settingsOutcome{}, errUnknownSettingsAction
	}
}

// reviewing reports whether the principal's weekly review job exists and is enabled.
func (m memorySection) reviewing(ctx context.Context, principalID string) (bool, error) {
	jobs, err := m.schedules.Jobs(ctx, principalID)
	if err != nil {
		return false, err
	}
	id := schedule.ReviewJobID(principalID)
	for _, job := range jobs {
		if job.ID == id {
			return job.Enabled, nil
		}
	}
	return false, nil
}

// actOnReview creates or resumes the principal's weekly review job, deletes it, or queues one run of
// it. Turning it on runs nothing at once; Review now does.
func (m memorySection) actOnReview(ctx context.Context, principalID, action string) (settingsOutcome, error) {
	id := schedule.ReviewJobID(principalID)
	logger := m.telemetry.ContextLogger(ctx, m.logger)
	switch action {
	case reviewOnAction:
		jobs, err := m.schedules.Jobs(ctx, principalID)
		if err != nil {
			return settingsOutcome{}, err
		}
		for _, job := range jobs {
			if job.ID != id {
				continue
			}
			if !job.Enabled {
				if _, err := m.schedules.SetEnabled(ctx, principalID, id, true); err != nil {
					return settingsOutcome{}, err
				}
				logger.InfoContext(ctx, "memory.review.changed", "principal_id", principalID, "enabled", true)
			}
			return settingsOutcome{}, nil
		}
		prepared, err := m.schedules.Prepare(principalID, schedule.ReviewName, schedule.ReviewType,
			schedule.Timing{Cron: schedule.ReviewCron, Zone: m.schedules.DefaultZone()}, memory.ReviewConfigJSON())
		if err != nil {
			return settingsOutcome{}, err
		}
		created, err := m.schedules.Create(ctx, id, prepared)
		if err != nil {
			if errors.Is(err, schedule.ErrTooManyJobs) {
				return settingsOutcome{note: MemoryReviewTooManyNote}, nil
			}
			return settingsOutcome{}, err
		}
		if created {
			logger.InfoContext(ctx, "memory.review.changed", "principal_id", principalID, "enabled", true)
		}
		return settingsOutcome{}, nil
	case reviewOffAction:
		deleted, err := m.schedules.Delete(ctx, principalID, id)
		if err != nil {
			return settingsOutcome{}, err
		}
		if deleted {
			logger.InfoContext(ctx, "memory.review.changed", "principal_id", principalID, "enabled", false)
		}
		return settingsOutcome{}, nil
	default:
		reviewing, err := m.reviewing(ctx, principalID)
		if err != nil || !reviewing {
			return settingsOutcome{}, err
		}
		// A review already queued or running is left to finish, so two never share the subscription.
		busy, err := m.schedules.Busy(ctx, principalID, id)
		if err != nil {
			return settingsOutcome{}, err
		}
		if busy {
			return settingsOutcome{note: MemoryReviewBusyNote}, nil
		}
		queued, err := m.schedules.RunNow(ctx, principalID, id, pressTriggerFrom(ctx))
		if err != nil || !queued {
			return settingsOutcome{}, err
		}
		logger.InfoContext(ctx, "memory.review.queued", "principal_id", principalID)
		return settingsOutcome{note: MemoryReviewingNote}, nil
	}
}

// memoryItem is one numbered line of /memory: an entry, or a procedure by its title. remove is the
// callback action that deletes it.
type memoryItem struct {
	id      string
	heading string
	text    string
	learned bool
	remove  string
}

// memoryItems are the principal's entries, instructions first, and then their procedures.
func memoryItems(ctx context.Context, principalID string, controller MemoryController) ([]memoryItem, error) {
	entries, err := controller.Entries(ctx, principalID)
	if err != nil {
		return nil, err
	}
	procedures, err := controller.Procedures(ctx, principalID)
	if err != nil {
		return nil, err
	}
	headings := map[string]string{memory.KindInstruction: "Instructions", memory.KindFact: "Facts"}
	items := make([]memoryItem, 0, len(entries)+len(procedures))
	for _, entry := range entries {
		items = append(items, memoryItem{id: entry.ID, heading: headings[entry.Kind], text: entry.Text, learned: entry.Source == memory.SourceLearned, remove: memoryDeleteAction})
	}
	for _, procedure := range procedures {
		items = append(items, memoryItem{id: procedure.ID, heading: proceduresHeading, text: procedure.Title, learned: procedure.Source == memory.SourceLearned, remove: procedureDeleteAction})
	}
	return items, nil
}

// renderMemory lists one page of the principal's entries, instructions first, and then their
// procedures, with Delete buttons numbered like the list. A page past the end shows the last one.
func renderMemory(ctx context.Context, principalID string, controller MemoryController, page int) (string, InlineKeyboard, error) {
	entries, err := memoryItems(ctx, principalID, controller)
	if err != nil {
		return "", nil, err
	}
	enabled, err := controller.Enabled(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(entries) == 0 {
		if !enabled {
			return NoMemoryReply + "\n\n" + MemoryOffNote, nil, nil
		}
		return NoMemoryReply, nil, nil
	}
	pages := (len(entries) + memoriesPerPage - 1) / memoriesPerPage
	page = min(max(page, 1), pages)
	title := MemoryTitle
	if pages > 1 {
		title += fmt.Sprintf(" (page %d of %d)", page, pages)
	}
	lines := []string{title}
	if !enabled {
		lines = append(lines, MemoryOffNote)
	}
	var keyboard InlineKeyboard
	var deletes []InlineButton
	first := (page - 1) * memoriesPerPage
	heading := ""
	for index, entry := range entries[first:min(first+memoriesPerPage, len(entries))] {
		if entry.heading != heading {
			heading = entry.heading
			lines = append(lines, "", heading)
		}
		number := strconv.Itoa(first + index + 1)
		line := number + ". " + entry.text
		if entry.learned {
			line += learnedMarker
		}
		lines = append(lines, line)
		deletes = append(deletes, InlineButton{Text: "Delete " + number, CallbackData: memoryCallback(entry.remove, entry.id, page)})
		if len(deletes) == deletesPerRow {
			keyboard, deletes = append(keyboard, deletes), nil
		}
	}
	if len(deletes) > 0 {
		keyboard = append(keyboard, deletes)
	}
	var navigation []InlineButton
	if page > 1 {
		navigation = append(navigation, InlineButton{Text: "‹ Previous", CallbackData: memoryCallback(memoryPageAction, "", page-1)})
	}
	if page < pages {
		navigation = append(navigation, InlineButton{Text: "Next ›", CallbackData: memoryCallback(memoryPageAction, "", page+1)})
	}
	if len(navigation) > 0 {
		keyboard = append(keyboard, navigation)
	}
	keyboard = append(keyboard, []InlineButton{{Text: "Forget everything", CallbackData: memoryCallback(memoryAskAction, "", page)}})
	return strings.Join(lines, "\n"), keyboard, nil
}

func renderMemoryConfirmation(ctx context.Context, principalID string, controller MemoryController, page int) (string, InlineKeyboard, error) {
	entries, err := memoryItems(ctx, principalID, controller)
	if err != nil {
		return "", nil, err
	}
	if len(entries) == 0 {
		return renderMemory(ctx, principalID, controller, page)
	}
	return fmt.Sprintf("Forget all %d items? I will not know any of it in later conversations, and this cannot be undone.", len(entries)),
		InlineKeyboard{{
			{Text: "Yes, forget everything", CallbackData: memoryCallback(memoryClearAction, "", page)},
			{Text: "Cancel", CallbackData: memoryCallback(memoryBackAction, "", page)},
		}}, nil
}

type memoryPress struct {
	action string
	id     string
	page   int
}

// memoryCallback is mem:<action>:<page> for every action but a delete, which is
// mem:del:<entry id>:<page>, or mem:pdel:<procedure id>:<page> for a procedure.
func memoryCallback(action, id string, page int) string {
	if id == "" {
		return memoryCallbackPrefix + ":" + action + ":" + strconv.Itoa(page)
	}
	return memoryCallbackPrefix + ":" + action + ":" + id + ":" + strconv.Itoa(page)
}

// parseMemoryCallback accepts only the fixed vocabulary, so a stale or forged button does nothing
// beyond refreshing the list.
func parseMemoryCallback(data string) (memoryPress, bool) {
	if len(data) > maximumCallbackDataBytes {
		return memoryPress{}, false
	}
	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != memoryCallbackPrefix {
		return memoryPress{}, false
	}
	press := memoryPress{action: parts[1]}
	switch press.action {
	case memoryDeleteAction, procedureDeleteAction:
		if len(parts) != 4 || parts[2] == "" {
			return memoryPress{}, false
		}
		press.id = parts[2]
	case memoryPageAction, memoryAskAction, memoryClearAction, memoryBackAction:
		if len(parts) != 3 {
			return memoryPress{}, false
		}
	default:
		return memoryPress{}, false
	}
	page, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return memoryPress{}, false
	}
	press.page = page
	return press, true
}

// processMemoryPress acts on the principal's own entries and refreshes the /memory message from
// current state, so a button on an outdated list cannot touch anything but the principal's entry.
func (s *Service) processMemoryPress(ctx context.Context, callback *CallbackQuery, principalID string, press memoryPress) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "memory." + press.action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	var followUp string
	var err error
	switch press.action {
	case memoryDeleteAction:
		var removed bool
		if removed, err = s.memory.Remove(ctx, principalID, press.id); err == nil && !removed {
			followUp = MemoryGoneReply
		}
	case procedureDeleteAction:
		var removed bool
		if removed, err = s.memory.RemoveProcedure(ctx, principalID, press.id); err == nil && !removed {
			followUp = MemoryGoneReply
		}
	case memoryClearAction:
		if _, err = s.memory.Clear(ctx, principalID); err == nil {
			followUp = MemoryClearedNote
		}
	}
	if err != nil {
		return processingResult{}, "memory_action", err
	}

	render := renderMemory
	if press.action == memoryAskAction {
		render = renderMemoryConfirmation
	}
	text, keyboard, err := render(ctx, principalID, s.memory, press.page)
	if err != nil {
		return processingResult{}, "render_memory", err
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
	}
	if err == nil && followUp != "" {
		err = s.client.SendReply(ctx, chatID, 0, followUp)
	}
	if err != nil {
		if isPermanentAPIError(err) {
			result.event, result.reason, result.err = "telegram.callback.undeliverable", "send_rejected", err
			return result, "", nil
		}
		return processingResult{}, "send_message", err
	}
	return result, "", nil
}
