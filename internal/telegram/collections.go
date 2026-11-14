package telegram

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/vladar107/home-ai-assistant/internal/collection"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	collectionCallbackPrefix = "col"
	collectionListAction     = "list"
	collectionOpenAction     = "open"
	collectionRecordDelete   = "rdel"
	collectionAskAction      = "ask"
	collectionDeleteAction   = "cdel"

	recordsPerPage = 8
	// recordLineUnits keeps a page within Telegram's 4096 UTF-16 units: a 60-character title, a
	// 300-character description, and 8 records of this many units each leave room to spare.
	recordLineUnits = 400
	opensPerRow     = 4
)

var (
	CollectionsTitle         = locale.T("Collections", "Коллекции")
	NoRecordsLine            = locale.T("No records yet.", "Записей пока нет.")
	collectionPagedTitle     = locale.T("%s (page %d of %d)", "%s (страница %d из %d)")
	collectionRecordsCount   = locale.N("%d record", "%d records", "%d запись", "%d записи", "%d записей")
	collectionOpenButton     = locale.T("Open %s", "Открыть %s")
	collectionDeleteButton   = locale.T("Delete collection", "Удалить коллекцию")
	collectionAllButton      = locale.T("‹ All collections", "‹ Все коллекции")
	collectionConfirmButton  = locale.T("Yes, delete it", "Да, удалить")
	collectionDeleteQuestion = locale.N(
		"Delete the collection %[2]s and its %[1]d record? This cannot be undone.",
		"Delete the collection %[2]s and its %[1]d records? This cannot be undone.",
		"Удалить коллекцию %[2]s и все её записи (%[1]d)? Это нельзя отменить.",
		"Удалить коллекцию %[2]s и все её записи (%[1]d)? Это нельзя отменить.",
		"Удалить коллекцию %[2]s и все её записи (%[1]d)? Это нельзя отменить.")
	NoCollectionsReply       = locale.T("You keep no collections. Ask me in conversation, for example: keep a list of things I lend out, with who and when.", "У тебя нет коллекций. Попроси меня в разговоре, например: веди список вещей, которые я даю на время, с указанием, кому и когда.")
	CollectionGoneReply      = locale.T("That collection no longer exists.", "Этой коллекции больше нет.")
	CollectionRecordGoneLine = locale.T("That record no longer exists.", "Этой записи больше нет.")
	CollectionDeletedNote    = locale.T("Deleted the collection.", "Коллекция удалена.")
)

// CollectionController is implemented by the collection service; nil leaves /collections out.
type CollectionController interface {
	Collections(context.Context, string) ([]collection.Collection, error)
	Collection(ctx context.Context, principalID, collectionID string) (collection.Collection, []collection.Record, bool, error)
	RemoveRecord(ctx context.Context, principalID, recordID string) (string, bool, error)
	DeleteCollection(ctx context.Context, principalID, collectionID string) (bool, error)
	RecordLine(collection.Collection, collection.Record) string
}

// renderCollections lists the principal's collections with their record counts and a button to
// open each.
func renderCollections(ctx context.Context, principalID string, controller CollectionController) (string, InlineKeyboard, error) {
	collections, err := controller.Collections(ctx, principalID)
	if err != nil {
		return "", nil, err
	}
	if len(collections) == 0 {
		return NoCollectionsReply.Format(ctx), nil, nil
	}
	lines := []string{CollectionsTitle.Format(ctx)}
	var keyboard InlineKeyboard
	var opens []InlineButton
	for index, found := range collections {
		number := strconv.Itoa(index + 1)
		lines = append(lines, number+". "+found.Name+": "+collectionRecordsCount.Format(ctx, found.Records))
		opens = append(opens, InlineButton{Text: collectionOpenButton.Format(ctx, number), CallbackData: collectionCallback(collectionOpenAction, found.ID, 1)})
		if len(opens) == opensPerRow {
			keyboard, opens = append(keyboard, opens), nil
		}
	}
	if len(opens) > 0 {
		keyboard = append(keyboard, opens)
	}
	return strings.Join(lines, "\n"), keyboard, nil
}

// renderCollection lists one page of a collection's records, oldest first, with Delete buttons
// numbered like the list. A page past the end shows the last one; false means the principal has no
// such collection.
func renderCollection(ctx context.Context, principalID string, controller CollectionController, collectionID string, page int) (string, InlineKeyboard, bool, error) {
	found, records, exists, err := controller.Collection(ctx, principalID, collectionID)
	if err != nil || !exists {
		return "", nil, false, err
	}
	pages := max((len(records)+recordsPerPage-1)/recordsPerPage, 1)
	page = min(max(page, 1), pages)
	title := found.Name
	if pages > 1 {
		title = collectionPagedTitle.Format(ctx, found.Name, page, pages)
	}
	lines := []string{title}
	if found.Description != "" {
		lines = append(lines, found.Description)
	}
	lines = append(lines, "")
	if len(records) == 0 {
		lines = append(lines, NoRecordsLine.Format(ctx))
	}
	var keyboard InlineKeyboard
	var deletes []InlineButton
	first := (page - 1) * recordsPerPage
	for index, record := range records[first:min(first+recordsPerPage, len(records))] {
		number := strconv.Itoa(first + index + 1)
		lines = append(lines, number+". "+clipUnits(controller.RecordLine(found, record), recordLineUnits))
		deletes = append(deletes, InlineButton{Text: deleteNumberButton.Format(ctx, number), CallbackData: collectionCallback(collectionRecordDelete, record.ID, page)})
		if len(deletes) == deletesPerRow {
			keyboard, deletes = append(keyboard, deletes), nil
		}
	}
	if len(deletes) > 0 {
		keyboard = append(keyboard, deletes)
	}
	var navigation []InlineButton
	if page > 1 {
		navigation = append(navigation, InlineButton{Text: listPreviousButton.Format(ctx), CallbackData: collectionCallback(collectionOpenAction, found.ID, page-1)})
	}
	if page < pages {
		navigation = append(navigation, InlineButton{Text: listNextButton.Format(ctx), CallbackData: collectionCallback(collectionOpenAction, found.ID, page+1)})
	}
	if len(navigation) > 0 {
		keyboard = append(keyboard, navigation)
	}
	keyboard = append(keyboard, []InlineButton{
		{Text: collectionDeleteButton.Format(ctx), CallbackData: collectionCallback(collectionAskAction, found.ID, page)},
		{Text: collectionAllButton.Format(ctx), CallbackData: collectionCallback(collectionListAction, "", 1)},
	})
	return strings.Join(lines, "\n"), keyboard, true, nil
}

func renderCollectionConfirmation(ctx context.Context, principalID string, controller CollectionController, collectionID string, page int) (string, InlineKeyboard, bool, error) {
	found, records, exists, err := controller.Collection(ctx, principalID, collectionID)
	if err != nil || !exists {
		return "", nil, false, err
	}
	return collectionDeleteQuestion.Format(ctx, len(records), found.Name),
		InlineKeyboard{{
			{Text: collectionConfirmButton.Format(ctx), CallbackData: collectionCallback(collectionDeleteAction, found.ID, page)},
			{Text: locale.Cancel.Format(ctx), CallbackData: collectionCallback(collectionOpenAction, found.ID, page)},
		}}, true, nil
}

// clipUnits cuts text to at most limit UTF-16 units, as Telegram counts a message, ending a cut
// text with an ellipsis.
func clipUnits(text string, limit int) string {
	if utf16Length(text) <= limit {
		return text
	}
	units := 0
	for index, r := range text {
		if units+utf16.RuneLen(r) > limit-1 {
			return strings.TrimRightFunc(text[:index], unicode.IsSpace) + "…"
		}
		units += utf16.RuneLen(r)
	}
	return text
}

type collectionPress struct {
	action string
	id     string
	page   int
}

// collectionCallback is col:list:<page> for the list, and col:<action>:<id>:<page> for an action
// on one collection, or on one record for a record's delete.
func collectionCallback(action, id string, page int) string {
	if id == "" {
		return collectionCallbackPrefix + ":" + action + ":" + strconv.Itoa(page)
	}
	return collectionCallbackPrefix + ":" + action + ":" + id + ":" + strconv.Itoa(page)
}

// parseCollectionCallback accepts only the fixed vocabulary, so a stale or forged button does
// nothing beyond refreshing the view.
func parseCollectionCallback(data string) (collectionPress, bool) {
	if len(data) > maximumCallbackDataBytes {
		return collectionPress{}, false
	}
	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != collectionCallbackPrefix {
		return collectionPress{}, false
	}
	press := collectionPress{action: parts[1]}
	switch press.action {
	case collectionOpenAction, collectionRecordDelete, collectionAskAction, collectionDeleteAction:
		if len(parts) != 4 || parts[2] == "" {
			return collectionPress{}, false
		}
		press.id = parts[2]
	case collectionListAction:
		if len(parts) != 3 {
			return collectionPress{}, false
		}
	default:
		return collectionPress{}, false
	}
	page, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return collectionPress{}, false
	}
	press.page = page
	return press, true
}

// processCollectionPress acts on the principal's own collections and refreshes the /collections
// message from current state, so a button on an outdated view cannot touch anything but the
// principal's own records.
func (s *Service) processCollectionPress(ctx context.Context, callback *CallbackQuery, principalID string, press collectionPress) (processingResult, string, error) {
	result := processingResult{event: "telegram.callback.handled", principalID: principalID, command: "collections." + press.action}
	chatID, messageID := callback.Message.Chat.ID, callback.Message.MessageID
	var followUp string
	collectionID := press.id
	var err error
	switch press.action {
	case collectionListAction:
		collectionID = ""
	case collectionRecordDelete:
		var removed bool
		if collectionID, removed, err = s.collections.RemoveRecord(ctx, principalID, press.id); err == nil && !removed {
			followUp = CollectionRecordGoneLine.Format(ctx)
		}
	case collectionDeleteAction:
		var deleted bool
		if deleted, err = s.collections.DeleteCollection(ctx, principalID, press.id); err == nil {
			followUp, collectionID = CollectionDeletedNote.Format(ctx), ""
			if !deleted {
				followUp = CollectionGoneReply.Format(ctx)
			}
		}
	}
	if err != nil {
		return processingResult{}, "collection_action", err
	}

	var text string
	var keyboard InlineKeyboard
	shown := false
	if collectionID != "" {
		render := renderCollection
		if press.action == collectionAskAction {
			render = renderCollectionConfirmation
		}
		if text, keyboard, shown, err = render(ctx, principalID, s.collections, collectionID, press.page); err != nil {
			return processingResult{}, "render_collections", err
		}
		if !shown && followUp == "" {
			followUp = CollectionGoneReply.Format(ctx)
		}
	}
	if !shown {
		if text, keyboard, err = renderCollections(ctx, principalID, s.collections); err != nil {
			return processingResult{}, "render_collections", err
		}
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
