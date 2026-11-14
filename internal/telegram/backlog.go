package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/backlog"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	BacklogExportHead = "# Assistant backlog"

	backlogExportArgument = "export"

	backlogCallbackPrefix = "bl"
	backlogDoneAction     = "done"
	backlogDeleteAction   = "del"
	backlogPageAction     = "page"

	backlogItemsPerPage = 8
)

var (
	BacklogTitle      = locale.T("Backlog", "Бэклог")
	backlogPagedTitle = locale.T("Backlog (page %d of %d)", "Бэклог (страница %d из %d)")
	backlogItemLine   = locale.T("%s. %s (%s)", "%s. %s (%s)")
	backlogToday      = locale.T("today", "сегодня")
	backlogDaysAgo    = locale.N("%d day ago", "%d days ago", "%d день назад", "%d дня назад", "%d дней назад")
	backlogDoneButton = locale.T("Done %s", "Готово %s")
	backlogExportHint = locale.T("Send /backlog export for the full text.", "Отправьте /backlog export, чтобы получить полный текст.")
	NoBacklogReply    = locale.T("Your backlog is empty. Tell me in conversation, for example: add to the backlog that you should answer weather questions with a chart.", "Бэклог пуст. Расскажите мне в разговоре, например: добавь в бэклог, что на вопросы о погоде нужно отвечать с графиком.")
	BacklogGoneReply  = locale.T("That item is no longer open.", "Этот пункт больше не открыт.")
	BacklogHelpLine   = locale.T("/backlog export sends your ideas to paste into a development session.", "/backlog export присылает ваши идеи, чтобы вставить их в сессию разработки.")
)

// BacklogController is implemented by the backlog service; nil leaves /backlog out.
type BacklogController interface {
	Items(context.Context, string) ([]backlog.Item, error)
	MarkDone(context.Context, string, string) (bool, error)
	Remove(context.Context, string, string) (bool, error)
	Now() time.Time
	Day(time.Time) string
}

// commandArgument is the first word after a command, lower-cased, or "" without one.
func commandArgument(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return ""
	}
	return strings.ToLower(fields[1])
}

// renderBacklog lists one page of the principal's open items, oldest first, each with Done and
// Delete buttons numbered like the list. A page past the end shows the last one.
func renderBacklog(ctx context.Context, principalID string, controller BacklogController, page int) (string, InlineKeyboard, error) {
	items, err := controller.Items(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(items) == 0 {
		return NoBacklogReply.Format(ctx), nil, nil
	}
	now := controller.Now()
	pages := (len(items) + backlogItemsPerPage - 1) / backlogItemsPerPage
	page = min(max(page, 1), pages)
	title := BacklogTitle.Format(ctx)
	if pages > 1 {
		title = backlogPagedTitle.Format(ctx, page, pages)
	}
	lines := []string{title, ""}
	var keyboard InlineKeyboard
	first := (page - 1) * backlogItemsPerPage
	for index, item := range items[first:min(first+backlogItemsPerPage, len(items))] {
		number := strconv.Itoa(first + index + 1)
		lines = append(lines, backlogItemLine.Format(ctx, number, item.Title, backlogAge(ctx, item.CreatedAt, now)))
		keyboard = append(keyboard, []InlineButton{
			{Text: backlogDoneButton.Format(ctx, number), CallbackData: backlogCallback(backlogDoneAction, item.ID, page)},
			{Text: deleteNumberButton.Format(ctx, number), CallbackData: backlogCallback(backlogDeleteAction, item.ID, page)},
		})
	}
	var navigation []InlineButton
	if page > 1 {
		navigation = append(navigation, InlineButton{Text: listPreviousButton.Format(ctx), CallbackData: backlogCallback(backlogPageAction, "", page-1)})
	}
	if page < pages {
		navigation = append(navigation, InlineButton{Text: listNextButton.Format(ctx), CallbackData: backlogCallback(backlogPageAction, "", page+1)})
	}
	if len(navigation) > 0 {
		keyboard = append(keyboard, navigation)
	}
	lines = append(lines, "", backlogExportHint.Format(ctx))
	return strings.Join(lines, "\n"), keyboard, nil
}

func backlogAge(ctx context.Context, created, now time.Time) string {
	days := int(now.Sub(created).Hours() / 24)
	if days < 1 {
		return backlogToday.Format(ctx)
	}
	return backlogDaysAgo.Format(ctx, days)
}

// backlogExport is the open items as Markdown, written to be pasted into a development session.
// Headings stay literal in Telegram, so a copied message keeps its structure.
func backlogExport(ctx context.Context, items []backlog.Item, day func(time.Time) string) string {
	if len(items) == 0 {
		return NoBacklogReply.Format(ctx)
	}
	var builder strings.Builder
	builder.WriteString(BacklogExportHead)
	for _, item := range items {
		fmt.Fprintf(&builder, "\n\n## %s\nAdded %s.", item.Title, day(item.CreatedAt))
		if item.Details != "" {
			builder.WriteString("\n\n" + item.Details)
		}
	}
	return builder.String()
}

// sendBacklogExport answers /backlog export in as many messages as the items need.
func (s *Service) sendBacklogExport(ctx context.Context, message *Message, principalID string) (processingResult, string, error) {
	items, err := s.backlog.Items(ctx, principalID)
	if err != nil {
		return processingResult{}, "handle_command", err
	}
	chunks := conversation.SplitReply(backlogExport(ctx, items, s.backlog.Day))
	for _, chunk := range chunks[:len(chunks)-1] {
		if err := s.client.SendReply(ctx, message.Chat.ID, message.MessageID, chunk); err != nil {
			if isPermanentAPIError(err) {
				return processingResult{event: "telegram.command.undeliverable", reason: "send_rejected", principalID: principalID, command: "backlog.export", err: err}, "", nil
			}
			return processingResult{}, "send_message", err
		}
	}
	return s.sendCommandReply(ctx, message, principalID, "backlog.export", chunks[len(chunks)-1], nil)
}

type backlogPress struct {
	action string
	id     string
	page   int
}

// backlogCallback is bl:page:<page>, or bl:<action>:<item id>:<page> for an action on an item.
func backlogCallback(action, id string, page int) string {
	if id == "" {
		return backlogCallbackPrefix + ":" + action + ":" + strconv.Itoa(page)
	}
	return backlogCallbackPrefix + ":" + action + ":" + id + ":" + strconv.Itoa(page)
}

// parseBacklogCallback accepts only the fixed vocabulary, so a stale or forged button does nothing
// beyond refreshing the list.
func parseBacklogCallback(data string) (backlogPress, bool) {
	if len(data) > maximumCallbackDataBytes {
		return backlogPress{}, false
	}
	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != backlogCallbackPrefix {
		return backlogPress{}, false
	}
	press := backlogPress{action: parts[1]}
	switch press.action {
	case backlogDoneAction, backlogDeleteAction:
		if len(parts) != 4 || parts[2] == "" {
			return backlogPress{}, false
		}
		press.id = parts[2]
	case backlogPageAction:
		if len(parts) != 3 {
			return backlogPress{}, false
		}
	default:
		return backlogPress{}, false
	}
	page, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return backlogPress{}, false
	}
	press.page = page
	return press, true
}

// processBacklogPress acts on the principal's own item and refreshes the /backlog message from
// current state, so a button on an outdated list cannot touch anything but the principal's item.
func (s *Service) processBacklogPress(ctx context.Context, callback *CallbackQuery, principalID string, press backlogPress) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "backlog." + press.action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	var changed bool
	var err error
	switch press.action {
	case backlogDoneAction:
		changed, err = s.backlog.MarkDone(ctx, principalID, press.id)
	case backlogDeleteAction:
		changed, err = s.backlog.Remove(ctx, principalID, press.id)
	}
	if err != nil {
		return processingResult{}, "backlog_action", err
	}
	var followUp string
	if press.action != backlogPageAction && !changed {
		followUp = BacklogGoneReply.Format(ctx)
	}

	text, keyboard, err := renderBacklog(ctx, principalID, s.backlog, press.page)
	if err != nil {
		return processingResult{}, "render_backlog", err
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
