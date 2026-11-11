package telegram

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

const (
	// widgetDataLimit is the most chat data Telegram keeps with a widget message, in bytes.
	widgetDataLimit = 1024
	// WidgetWorkingAnswer answers a Confirm or Reject tap, which the widget shows until the outcome
	// replaces its actions.
	WidgetWorkingAnswer = "Working on it…"
	// WidgetIgnoredAnswer answers a tap that names nothing the user can act on.
	WidgetIgnoredAnswer = "This no longer does anything."
)

// widgetTemplate is the only program a widget runs. It draws the message's chat data, a widgetData,
// and sends the data of the action tapped, which is what the button it stands for would send.
//
//go:embed widget.html
var widgetTemplate string

// widgetData is what a proposal widget shows: the proposal's text, whose first line is its title,
// where it stands, and either the actions still open or none once it is resolved.
type widgetData struct {
	Text    string         `json:"text"`
	Status  string         `json:"status,omitempty"`
	Actions []widgetAction `json:"actions,omitempty"`
}

type widgetAction struct {
	Label string `json:"label"`
	Data  string `json:"data"`
}

func proposalActions(buttons approval.Buttons) []widgetAction {
	actions := []widgetAction{
		{Label: "Confirm", Data: buttons.Confirm},
		{Label: "Reject", Data: buttons.Reject},
	}
	if buttons.Preview != "" {
		actions = append(actions, widgetAction{Label: "Preview", Data: buttons.Preview})
	}
	return actions
}

// encodeWidgetData renders a proposal's text and status as the user reads them, with its actions,
// and reports whether the data fits Telegram's limit whole.
func encodeWidgetData(text, status string, actions []widgetAction) (string, bool) {
	data := marshalWidgetData(widgetData{Text: plainWidgetText(text), Status: plainWidgetText(status), Actions: actions})
	return data, len(data) <= widgetDataLimit
}

// shortenWidgetData is encodeWidgetData shortening the text in the middle until the data fits, with
// the status whole, or with no text and the status shortened when the status alone does not fit.
// It reports whether anything was shortened, and false for fits only when the actions alone do not
// fit.
func shortenWidgetData(text, status string, actions []widgetAction) (data string, shortened, fits bool) {
	if data, fits := encodeWidgetData(text, status, actions); fits {
		return data, false, true
	}
	shownText, shownStatus := plainWidgetText(text), plainWidgetText(status)
	encode := func(text, status string) (string, bool) {
		data := marshalWidgetData(widgetData{Text: text, Status: status, Actions: actions})
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

// plainWidgetText is text as a message would show it, without the formatting, which a widget does
// not draw.
func plainWidgetText(text string) string {
	return captionText(renderMarkup(text))
}

func marshalWidgetData(data widgetData) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	// Encoding strings and a slice of them cannot fail.
	_ = encoder.Encode(data)
	return strings.TrimSuffix(encoded.String(), "\n")
}

// longestFit is the largest limit from 0 to most that fits, or -1 when none does. A longer limit
// never fits where a shorter one does not.
func longestFit(most int, fits func(limit int) bool) int {
	if !fits(0) {
		return -1
	}
	low, high := 0, most
	for low < high {
		middle := (low + high + 1) / 2
		if fits(middle) {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

// clipMiddle shortens text to at most limit runes by cutting out its middle, so both the start of a
// proposal and the status line at its end stay readable.
func clipMiddle(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit <= 0 {
		return ""
	}
	tail := (limit - 1) / 3
	head := limit - 1 - tail
	return strings.TrimRightFunc(string(runes[:head]), unicode.IsSpace) + "…" + strings.TrimLeftFunc(string(runes[len(runes)-tail:]), unicode.IsSpace)
}

// widgetCaption is a widget's caption line, rendered when it fits a caption, and otherwise as the
// user would read it, shortened in the middle, with plain set.
func widgetCaption(line string) (caption string, plain bool) {
	shown := captionText(renderMarkup(line))
	if utf16Length(shown) <= captionLimit {
		return line, false
	}
	longest := longestFit(captionLimit, func(limit int) bool {
		return utf16Length(clipMiddle(shown, limit)) <= captionLimit
	})
	return clipMiddle(shown, longest), true
}

// setCaption renders a caption as a message is rendered, or sets it as written when plain.
func setCaption(params url.Values, caption string, plain bool) {
	if plain {
		params.Set("caption", caption)
		params.Del("parse_mode")
		return
	}
	params.Set("caption", renderMarkup(caption))
	params.Set("parse_mode", "HTML")
}

// executeCaptioned calls method with a widget's caption line set, and once more with it plain when
// Telegram refuses its entities.
func executeCaptioned[T any](c *Client, ctx context.Context, method string, params url.Values, line string) (T, error) {
	caption, plain := widgetCaption(line)
	setCaption(params, caption, plain)
	result, err := execute[T](c, ctx, method, params)
	if !plain && hasUnparsableEntities(err) {
		setCaption(params, captionText(renderMarkup(caption)), true)
		result, err = execute[T](c, ctx, method, params)
	}
	return result, err
}

// sendWidget posts a proposal widget with the given actions, its text and status in the data and its
// caption line under it. It reports false, sending nothing, when the text and status do not fit the
// data whole.
func (c *Client) sendWidget(ctx context.Context, chatID int64, shown approval.Shown, actions []widgetAction) (Message, bool, error) {
	data, fits := encodeWidgetData(shown.Body, shown.Status, actions)
	if !fits {
		return Message{}, false, nil
	}
	params := url.Values{
		"chat_id":       {strconv.FormatInt(chatID, 10)},
		"html":          {widgetTemplate},
		"chat_data":     {data},
		"allow_queries": {"true"},
	}
	var message Message
	err := c.inTab(ctx, chatID, true, func(ctx context.Context) error {
		params.Del("message_thread_id")
		setThread(ctx, params)
		var err error
		message, err = executeCaptioned[Message](c, ctx, "sendWidget", params, shown.Caption)
		return err
	})
	return message, true, err
}

// editWidget shows a proposal on its widget: the caption line, and the text and status in the data
// with the actions given, or with none once the proposal is resolved. The data is sent whatever
// became of the caption. A text too long for the data is shortened there, with the status whole, and
// sent in full as a reply to the widget once: data already showing it is not modified and sends no
// second reply. The first error is returned.
func (c *Client) editWidget(ctx context.Context, chatID, messageID int64, shown approval.Shown, actions []widgetAction) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	_, captionErr := executeCaptioned[json.RawMessage](c, ctx, "editMessageCaption", params, shown.Caption)
	if isNotModified(captionErr) {
		captionErr = nil
	}
	var changed bool
	var dataErr error
	data, shortened, fits := shortenWidgetData(shown.Body, shown.Status, actions)
	if fits {
		changed, dataErr = c.editWidgetData(ctx, chatID, messageID, data)
	}
	var replyErr error
	if shortened && changed {
		for _, chunk := range conversation.SplitReply(shown.Text) {
			if replyErr = c.SendReply(ctx, chatID, messageID, chunk); replyErr != nil {
				break
			}
		}
	}
	for _, err := range []error{captionErr, dataErr, replyErr} {
		if err != nil {
			return err
		}
	}
	return nil
}

// editWidgetData replaces a widget's data and reports whether that changed it.
func (c *Client) editWidgetData(ctx context.Context, chatID, messageID int64, data string) (bool, error) {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
		"chat_data":  {data},
	}
	_, err := execute[json.RawMessage](c, ctx, "editMessageWidgetData", params)
	if isNotModified(err) {
		return false, nil
	}
	return err == nil, err
}

// AnswerWidgetQuery answers a widget's request with data the widget receives as its result.
// Telegram accepts the answer for 15 seconds after the tap.
func (c *Client) AnswerWidgetQuery(ctx context.Context, queryID, data string) error {
	params := url.Values{
		"widget_query_id": {queryID},
		"data":            {clip.Bytes(data, widgetDataLimit)},
	}
	_, err := execute[bool](c, ctx, "answerWidgetQuery", params)
	return err
}
