// Package task runs background tasks: a chat turn hands a long request to a sub-agent, which runs
// as its own Pi process beside the chat, outside the principal's turn slot, and reports back as a
// reply to the message that asked for it. Each task is a durable row with its own Pi session, so
// it resumes after a restart.
package task

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// ErrTooManyTasks means the principal already has a running task and a full queue.
var ErrTooManyTasks = errors.New("too many open background tasks")

// Task is one background task as stored.
type Task struct {
	ID          string
	PrincipalID string
	ChatID      int64
	// ThreadID is the Telegram tab the task was asked for in; its report goes back there.
	ThreadID         int64
	RequestMessageID int64
	Title            string
	Brief            string
	SessionID        string
	Status           string
	Partial          bool
	ErrorKind        string
	Result           string
	StatusMessageID  int64
	// Attempts counts the times the task's run was started; more than one means it resumed after
	// a restart.
	Attempts    int
	Progress    Progress
	Deadline    time.Time
	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	DeliveredAt time.Time
	// DeliveredChunks is how many of the report's chunks an earlier delivery attempt already sent.
	DeliveredChunks int
	// Model and Effort are the pair the task runs on, copied when it was created; empty on a task
	// from before they were stored, until its first run settles them.
	Model  string
	Effort string
	// ModelFallback says why the task runs on the chat default instead of the task default; empty
	// when it did not fall back.
	ModelFallback string
}

// Open reports whether the task has yet to finish.
func (t Task) Open() bool { return t.Status == StatusQueued || t.Status == StatusRunning }

// Progress is what a task's runs used so far. ToolCalls counts every lookup, web searches
// included.
type Progress struct {
	// ToolCalls counts every lookup, web searches included; the broker's budget covers only the
	// calls that are not web searches, which run inside the runtime.
	ToolCalls    int
	WebSearches  int
	ModelCalls   int
	PromptTokens int64
	OutputTokens int64
}

// Outcome is how a task's run ended.
type Outcome struct {
	Status    string
	Partial   bool
	Result    string
	ErrorKind string
	Progress  Progress
}

type Repository interface {
	// CreateTask stores a queued task unless the principal already has maximumOpen open tasks,
	// in which case it returns ErrTooManyTasks.
	// It returns how many of the principal's tasks were already open.
	CreateTask(ctx context.Context, task Task, maximumOpen int) (int, error)
	// TaskOrigin returns the chat and Telegram message of the principal's conversation request.
	TaskOrigin(ctx context.Context, principalID, requestID string) (int64, int64, bool, error)
	SetTaskStatusMessage(ctx context.Context, taskID string, messageID int64) error
	// NextTask returns the principal's next piece of work: a finished task still to deliver, then
	// a running one to resume, then the oldest queued one.
	NextTask(ctx context.Context, principalID string) (Task, bool, error)
	// StartTask moves a queued task to running, or counts another attempt of a running one, and
	// returns it. A task that is no longer open is not started.
	StartTask(ctx context.Context, taskID string, deadline, now time.Time) (Task, bool, error)
	SaveTaskProgress(ctx context.Context, taskID string, progress Progress, now time.Time) error
	// FinishTask ends a running task; a task cancelled meanwhile is left as it is.
	FinishTask(ctx context.Context, taskID string, outcome Outcome, now time.Time) (bool, error)
	// CancelTask ends one of the principal's open tasks without a report.
	CancelTask(ctx context.Context, principalID, taskID string, now time.Time) (Task, bool, error)
	MarkTaskDelivered(ctx context.Context, taskID string, now time.Time) error
	// SaveTaskDeliveryProgress records how many of an undelivered report's chunks are in the chat.
	SaveTaskDeliveryProgress(ctx context.Context, taskID string, chunks int, now time.Time) error
	// RecoverTasks fails each task a previous process left running after it had already been
	// started maximumAttempts times, and returns the principals with work left.
	RecoverTasks(ctx context.Context, maximumAttempts int, failure string, now time.Time) ([]string, error)
	// ListTasks returns the principal's open tasks and those finished since the given time.
	ListTasks(ctx context.Context, principalID string, since time.Time) ([]Task, error)
	LoadTask(ctx context.Context, principalID, taskID string) (Task, bool, error)
	// TakeTaskNotices returns the principal's tasks delivered since the given time that no chat
	// turn has been told about yet, and marks them told.
	TakeTaskNotices(ctx context.Context, principalID string, since time.Time) ([]Task, error)
	PruneTasks(ctx context.Context, before time.Time) error
	// SetTaskModel stores the pair of a task that has none yet; a task that has one is left as it is.
	SetTaskModel(ctx context.Context, taskID, model, effort, fallback string) error
}

// Why a task runs on the chat default instead of the task default.
const (
	FallbackCatalog = modelcatalog.FallbackCatalog
	FallbackUsage   = modelcatalog.FallbackUsage
)

// Chats resolves the principal's own private chat; a principal without one gets no task run.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// Messenger keeps a task's one status message, with a Cancel button while the task is open.
type Messenger interface {
	SendStatus(ctx context.Context, chatID, replyTo int64, text, taskID string) (int64, error)
	EditStatus(ctx context.Context, chatID, messageID int64, text, taskID string, open bool) error
}

// Telemetry records one span and one set of measurements per run of a task.
type Telemetry interface {
	StartTask(ctx context.Context, principal string) (context.Context, func(outcome string, modelCalls, toolCalls int, promptTokens, outputTokens int64))
}

var (
	queuedStatus       = locale.T("Queued: %s\nIt starts when your current background task is done.", "В очереди: %s\nЗапустится, когда закончится твоя текущая фоновая задача.")
	runningStatus      = locale.T("Working on: %s\n%s so far, %s.", "Работаю над: %s\nПрошло %s, %s.")
	doneStatus         = locale.T("%s: %s\nTook %s, %s. The report is below.", "%s: %s\nЗаняло %s, %s. Отчёт ниже.")
	doneLabel          = locale.T("Done", "Готово")
	partialLabel       = locale.T("Stopped at its limit", "Достигнут лимит")
	failedStatus       = locale.T("Failed: %s", "Не удалось: %s")
	cancelledStatus    = locale.T("Cancelled: %s", "Отменено: %s")
	startingStatus     = locale.T("Starting: %s", "Запускаю: %s")
	queuedSummary      = locale.T("queued", "в очереди")
	runningSummary     = locale.T("running for %s, %s", "выполняется уже %s, %s")
	partialSummary     = locale.T("partial report, finished %s ago", "частичный отчёт, завершено %s назад")
	doneSummary        = locale.T("done, finished %s ago", "готово, завершено %s назад")
	failedSummary      = locale.T("failed %s ago", "ошибка %s назад")
	cancelledSummary   = locale.T("cancelled %s ago", "отменено %s назад")
	underAMinute       = locale.T("under a minute", "меньше минуты")
	minutesDuration    = locale.T("%d min", "%d мин")
	hoursDuration      = locale.T("%d h", "%d ч")
	hoursMinutesLength = locale.T("%d h %d min", "%d ч %d мин")
	lookupCount        = locale.N("%d lookup", "%d lookups", "%d запрос", "%d запроса", "%d запросов")
	reportHeading      = locale.T("**Background task: %s**", "**Фоновая задача: %s**")
	partialNote        = locale.T("Partial report: the task reached its limit before it finished.", "Частичный отчёт: задача достигла лимита, не закончив работу.")
)

// StatusText is the status message of a task at the given moment, in the context's language.
func StatusText(ctx context.Context, task Task, now time.Time) string {
	switch task.Status {
	case StatusQueued:
		return queuedStatus.Format(ctx, task.Title)
	case StatusRunning:
		return runningStatus.Format(ctx, task.Title, ElapsedFor(ctx, task.StartedAt, now), lookups(ctx, task.Progress.ToolCalls))
	case StatusDone:
		label := doneLabel.Format(ctx)
		if task.Partial {
			label = partialLabel.Format(ctx)
		}
		return doneStatus.Format(ctx, label, task.Title, ElapsedFor(ctx, task.StartedAt, task.FinishedAt), lookups(ctx, task.Progress.ToolCalls))
	case StatusFailed:
		return failedStatus.Format(ctx, task.Title)
	default:
		return cancelledStatus.Format(ctx, task.Title)
	}
}

// StartingText is the status message of a task that starts at once.
func StartingText(ctx context.Context, title string) string { return startingStatus.Format(ctx, title) }

// Summary is one task's line in a list, in English: its state, how long it has run, and its
// lookups. SummaryFor writes it in the context's language.
func Summary(task Task, now time.Time) string { return SummaryFor(context.Background(), task, now) }

// SummaryFor is Summary in the context's language.
func SummaryFor(ctx context.Context, task Task, now time.Time) string {
	switch task.Status {
	case StatusQueued:
		return queuedSummary.Format(ctx)
	case StatusRunning:
		return runningSummary.Format(ctx, ElapsedFor(ctx, task.StartedAt, now), lookups(ctx, task.Progress.ToolCalls))
	case StatusDone:
		if task.Partial {
			return partialSummary.Format(ctx, ElapsedFor(ctx, task.FinishedAt, now))
		}
		return doneSummary.Format(ctx, ElapsedFor(ctx, task.FinishedAt, now))
	case StatusFailed:
		return failedSummary.Format(ctx, ElapsedFor(ctx, task.FinishedAt, now))
	default:
		return cancelledSummary.Format(ctx, ElapsedFor(ctx, task.FinishedAt, now))
	}
}

// Elapsed is the time between two moments as Duration renders it; an unknown start is no time.
func Elapsed(from, to time.Time) string { return ElapsedFor(context.Background(), from, to) }

// ElapsedFor is Elapsed in the context's language.
func ElapsedFor(ctx context.Context, from, to time.Time) string {
	if from.IsZero() {
		return DurationFor(ctx, 0)
	}
	return DurationFor(ctx, to.Sub(from))
}

// Duration renders a duration in whole minutes, as a status message updated once a minute can. It
// is English, for what the model reads; DurationFor writes it in the context's language.
func Duration(duration time.Duration) string { return DurationFor(context.Background(), duration) }

// DurationFor is Duration in the context's language.
func DurationFor(ctx context.Context, duration time.Duration) string {
	minutes := int(duration / time.Minute)
	switch {
	case minutes < 1:
		return underAMinute.Format(ctx)
	case minutes < 60:
		return minutesDuration.Format(ctx, minutes)
	case minutes%60 == 0:
		return hoursDuration.Format(ctx, minutes/60)
	default:
		return hoursMinutesLength.Format(ctx, minutes/60, minutes%60)
	}
}

func lookups(ctx context.Context, count int) string { return lookupCount.Format(ctx, count) }

// ReportText is what the report message says: the title, whether the report is partial, and the
// result. The results the service and the turn engine write for a failed task are stored in
// English for the model and shown in the context's language.
func ReportText(ctx context.Context, task Task) string {
	heading := reportHeading.Format(ctx, task.Title)
	if task.Partial {
		heading += "\n" + partialNote.Format(ctx)
	}
	result := strings.TrimSpace(task.Result)
	switch task.Result {
	case LimitReply.In(locale.English):
		result = LimitReply.Format(ctx)
	case InterruptedTwiceText.In(locale.English):
		result = InterruptedTwiceText.Format(ctx)
	default:
		for _, reply := range []locale.Text{turn.TimeoutReply, turn.UnavailableReply, turn.QuotaReply, turn.ReconnectReply, turn.NotConnectedReply} {
			if task.Result == reply.In(locale.English) {
				result = reply.Format(ctx)
			}
		}
	}
	return heading + "\n\n" + result
}
