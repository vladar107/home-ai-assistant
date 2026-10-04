package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/task"
)

const (
	TasksTitle       = "Background tasks"
	NoTasksReply     = "You have no background tasks from the last week. Ask me for something long, such as going through a month of mail, and I will work on it in the background while we keep talking."
	TaskGoneReply    = "That task has already finished or was cancelled."
	TasksHelpLine    = " /tasks shows your background tasks."
	TaskPrivacyLine  = "A background task's instructions and report are kept on the home system for 30 days after it finishes, so you can ask about the report later; its working history, including what it read, stays in your local session history like other conversations."
	tasksPrefix      = "tasks"
	taskCancelAction = "cancel"
)

// TaskController is implemented by the task service; nil disables /tasks.
type TaskController interface {
	Tasks(ctx context.Context, principalID string) ([]task.Task, error)
	Cancel(ctx context.Context, principalID, taskID string) (task.Task, bool, error)
	Now() time.Time
}

// SetTasks adds /tasks, its Cancel buttons, and the background task line to /help and /privacy;
// call it before Start.
func (s *Service) SetTasks(tasks TaskController) {
	s.tasks = tasks
}

// TaskMessenger keeps each task's status message, with the Cancel button that /tasks also uses.
type TaskMessenger struct {
	Client *Client
}

func (m TaskMessenger) SendStatus(ctx context.Context, chatID, replyTo int64, text, taskID string) (int64, error) {
	message, err := m.Client.sendMessage(ctx, chatID, replyTo, text, taskKeyboard(taskID, "Cancel"))
	if err != nil {
		return 0, err
	}
	if message.MessageID <= 0 {
		return 0, fmt.Errorf("Telegram sendMessage returned no message ID")
	}
	return message.MessageID, nil
}

func (m TaskMessenger) EditStatus(ctx context.Context, chatID, messageID int64, text, taskID string, open bool) error {
	var keyboard InlineKeyboard
	if open {
		keyboard = taskKeyboard(taskID, "Cancel")
	}
	return m.Client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
}

func taskKeyboard(taskID, label string) InlineKeyboard {
	return InlineKeyboard{{{Text: label, CallbackData: taskCancelCallback(taskID)}}}
}

func taskCancelCallback(taskID string) string {
	return tasksPrefix + ":" + taskCancelAction + ":" + taskID
}

// parseTaskCallback accepts only the fixed vocabulary, tasks:cancel:<task id>.
func parseTaskCallback(data string) (string, bool) {
	if len(data) > maximumCallbackDataBytes {
		return "", false
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != tasksPrefix || parts[1] != taskCancelAction || parts[2] == "" {
		return "", false
	}
	return parts[2], true
}

// renderTasks lists the principal's open and recent tasks, numbered like their Cancel buttons.
func renderTasks(ctx context.Context, principalID string, tasks TaskController) (string, InlineKeyboard, error) {
	listed, err := tasks.Tasks(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(listed) == 0 {
		return NoTasksReply, nil, nil
	}
	now := tasks.Now()
	lines := []string{TasksTitle, ""}
	var keyboard InlineKeyboard
	for index, listedTask := range listed {
		number := strconv.Itoa(index + 1)
		lines = append(lines, fmt.Sprintf("%s. %s\n%s", number, listedTask.Title, task.Summary(listedTask, now)))
		if listedTask.Open() {
			keyboard = append(keyboard, taskKeyboard(listedTask.ID, "Cancel "+number)...)
		}
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

// processTaskPress cancels the principal's own task. A press on the task's status message leaves
// that message to the task service, which shows the outcome there; a press on a /tasks list
// refreshes the list.
func (s *Service) processTaskPress(ctx context.Context, callback *CallbackQuery, principalID, taskID string) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "tasks.cancel"}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	cancelled, changed, err := s.tasks.Cancel(ctx, principalID, taskID)
	if err != nil {
		return processingResult{}, "tasks_action", err
	}
	if changed && cancelled.StatusMessageID == messageID {
		return result, "", nil
	}
	var text string
	var keyboard InlineKeyboard
	if !changed && !strings.HasPrefix(callback.Message.Text, TasksTitle) {
		text = strings.TrimSpace(callback.Message.Text)
		if text == "" {
			text = TaskGoneReply
		} else {
			text += "\n\n" + TaskGoneReply
		}
	} else if text, keyboard, err = renderTasks(ctx, principalID, s.tasks); err != nil {
		return processingResult{}, "render_tasks", err
	}
	err = s.client.EditKeyboard(ctx, chatID, messageID, text, keyboard)
	if err != nil && isPermanentAPIError(err) {
		err = s.client.SendKeyboard(ctx, chatID, 0, text, keyboard)
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
