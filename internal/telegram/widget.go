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

// widgetData is what a proposal widget shows: the proposal's text and either the actions still
// open, or no actions once the text carries the outcome.
type widgetData struct {
	Text    string         `json:"text"`
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

// encodeWidgetData renders a proposal's text as the user reads it, shortened in the middle until
// the data fits Telegram's limit, and reports false when even no text would not fit.
func encodeWidgetData(text string, actions []widgetAction) (string, bool) {
	shown := captionText(renderMarkup(text))
	encode := func(limit int) (string, bool) {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(widgetData{Text: clipMiddle(shown, limit), Actions: actions}); err != nil {
			return "", false
		}
		data := strings.TrimSuffix(encoded.String(), "\n")
		return data, len(data) <= widgetDataLimit
	}
	longest := longestFit(utf8.RuneCountInString(shown), func(limit int) bool {
		_, fits := encode(limit)
		return fits
	})
	if longest < 0 {
		return "", false
	}
	return encode(longest)
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

// widgetCaption is the caption of a widget showing text: the rendered text when it fits, and
// otherwise the text the user would read, shortened in the middle, with clipped set.
func widgetCaption(text string) (caption string, clipped bool) {
	if utf16Length(captionText(renderMarkup(text))) <= captionLimit {
		return text, false
	}
	shown := captionText(renderMarkup(text))
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

// sendWidget posts text as a proposal widget with the given actions and the text as its caption.
// It reports false, sending nothing, when the text does not fit a caption or the data does not fit
// Telegram's limit.
func (c *Client) sendWidget(ctx context.Context, chatID int64, text string, actions []widgetAction) (Message, bool, error) {
	if !c.CaptionFits(text) {
		return Message{}, false, nil
	}
	data, fits := encodeWidgetData(text, actions)
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
		setCaption(params, text, false)
		var err error
		message, err = execute[Message](c, ctx, "sendWidget", params)
		if hasUnparsableEntities(err) {
			setCaption(params, captionText(renderMarkup(text)), true)
			message, err = execute[Message](c, ctx, "sendWidget", params)
		}
		return err
	})
	return message, true, err
}

// editWidget shows text on a proposal widget: as its caption, and as its data with the actions
// given, or with none once the text is an outcome. The data is sent whatever became of the caption,
// so the widget never keeps actions the caption says are spent. A text too long for a caption is
// clipped there and sent in full as a reply to the widget, once: a caption already showing it is
// not modified and sends no second reply. The first error is returned.
func (c *Client) editWidget(ctx context.Context, chatID, messageID int64, text string, actions []widgetAction) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	caption, clipped := widgetCaption(text)
	setCaption(params, caption, clipped)
	_, captionErr := execute[json.RawMessage](c, ctx, "editMessageCaption", params)
	if hasUnparsableEntities(captionErr) {
		setCaption(params, captionText(renderMarkup(caption)), true)
		_, captionErr = execute[json.RawMessage](c, ctx, "editMessageCaption", params)
	}
	changed := captionErr == nil
	if isNotModified(captionErr) {
		captionErr = nil
	}
	var replyErr error
	if clipped && changed {
		for _, chunk := range conversation.SplitReply(text) {
			if replyErr = c.SendReply(ctx, chatID, messageID, chunk); replyErr != nil {
				break
			}
		}
	}
	var dataErr error
	if data, fits := encodeWidgetData(text, actions); fits {
		dataErr = c.editWidgetData(ctx, chatID, messageID, data)
	}
	if captionErr == nil && isPermanentAPIError(dataErr) {
		// The caption already shows the text; reporting the refusal would send it once more.
		dataErr = nil
	}
	for _, err := range []error{captionErr, replyErr, dataErr} {
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) editWidgetData(ctx context.Context, chatID, messageID int64, data string) error {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
		"chat_data":  {data},
	}
	_, err := execute[json.RawMessage](c, ctx, "editMessageWidgetData", params)
	if isNotModified(err) {
		return nil
	}
	return err
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
