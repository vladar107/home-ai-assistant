package telegram

import (
	"context"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// widgetData is what a proposal widget shows: the proposal's text, whose first line is its title,
// where it stands, and either the actions still open or none once it is resolved. Proposal widgets
// already in chats read this shape, so it keeps it.
type widgetData struct {
	Text    string         `json:"text"`
	Status  string         `json:"status,omitempty"`
	Actions []widgetAction `json:"actions,omitempty"`
	// Lang is the language the widget draws its labels in, none for English.
	Lang string `json:"lang,omitempty"`
}

type widgetAction struct {
	Label string `json:"label"`
	Data  string `json:"data"`
}

func proposalActions(buttons approval.Buttons) []widgetAction {
	actions := []widgetAction{
		{Label: locale.Confirm.In(buttons.Language), Data: buttons.Confirm},
		{Label: locale.Reject.In(buttons.Language), Data: buttons.Reject},
	}
	if buttons.Preview != "" {
		actions = append(actions, widgetAction{Label: locale.Preview.In(buttons.Language), Data: buttons.Preview})
	}
	return actions
}

// encodeWidgetData renders a proposal's text and status as the user reads them, with its actions,
// and reports whether the data fits Telegram's limit whole.
func encodeWidgetData(language locale.Language, text, status string, actions []widgetAction) (string, bool) {
	data := marshalWidgetData(widgetData{Text: plainWidgetText(text), Status: plainWidgetText(status), Actions: actions, Lang: widgetLanguage(language)})
	return data, len(data) <= widgetDataLimit
}

// shortenWidgetData is encodeWidgetData shortening the text in the middle until the data fits, with
// the status whole, or with no text and the status shortened when the status alone does not fit.
// It reports whether anything was shortened, and false for fits only when the actions alone do not
// fit.
func shortenWidgetData(language locale.Language, text, status string, actions []widgetAction) (data string, shortened, fits bool) {
	if data, fits := encodeWidgetData(language, text, status, actions); fits {
		return data, false, true
	}
	shownText, shownStatus := plainWidgetText(text), plainWidgetText(status)
	encode := func(text, status string) (string, bool) {
		data := marshalWidgetData(widgetData{Text: text, Status: status, Actions: actions, Lang: widgetLanguage(language)})
		return data, len(data) <= widgetDataLimit
	}
	if longest := longestFit(utf8.RuneCountInString(shownText), func(limit int) bool {
		_, fits := encode(clipMiddle(shownText, limit), shownStatus)
		return fits
	}); longest >= 0 {
		data, _ := encode(clipMiddle(shownText, longest), shownStatus)
		return data, true, true
	}
	longest := longestFit(utf8.RuneCountInString(shownStatus), func(limit int) bool {
		_, fits := encode("", clipMiddle(shownStatus, limit))
		return fits
	})
	if longest < 0 {
		return "", false, false
	}
	data, _ = encode("", clipMiddle(shownStatus, longest))
	return data, true, true
}

// sendProposalWidget posts a proposal widget with the given actions, its text and status in the data
// and its caption line under it. It reports false, sending nothing, when the text and status do not
// fit the data whole.
func (c *Client) sendProposalWidget(ctx context.Context, chatID int64, shown approval.Shown, actions []widgetAction) (Message, bool, error) {
	data, fits := encodeWidgetData(shown.Language, shown.Body, shown.Status, actions)
	if !fits {
		return Message{}, false, nil
	}
	message, err := c.sendWidget(ctx, chatID, proposalWidget, data, shown.Caption)
	return message, true, err
}

// editProposalWidget shows a proposal on its widget: the caption line, and the text and status in
// the data with the actions given, or with none once the proposal is resolved. A text too long for
// the data is shortened there, with the status whole, and sent in full as a reply to the widget
// once: data already showing it is not modified and sends no second reply. The first error is
// returned.
func (c *Client) editProposalWidget(ctx context.Context, chatID, messageID int64, shown approval.Shown, actions []widgetAction) error {
	data, shortened, fits := shortenWidgetData(shown.Language, shown.Body, shown.Status, actions)
	if !fits {
		data = ""
	}
	changed, err := c.editWidget(ctx, chatID, messageID, shown.Caption, data)
	var replyErr error
	if shortened && changed {
		for _, chunk := range conversation.SplitReply(shown.Text) {
			if replyErr = c.SendReply(ctx, chatID, messageID, chunk); replyErr != nil {
				break
			}
		}
	}
	if err != nil {
		return err
	}
	return replyErr
}
