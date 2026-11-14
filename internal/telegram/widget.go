package telegram

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/clip"
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

// widgetKind names a kind of widget, which is the program it runs and the data that program reads.
// A widget message keeps the program it was sent with, so a kind's data must stay readable by the
// views already sent.
type widgetKind string

const (
	proposalWidget widgetKind = "proposal"
	changesWidget  widgetKind = "changes"
)

//go:embed widgets
var widgetFiles embed.FS

// widgetTemplates is the program each kind of widget runs: the shared base with the kind's view in
// it, built once when the program starts. No part of it comes from a conversation.
var widgetTemplates = map[widgetKind]string{
	proposalWidget: composeWidget(proposalWidget),
	changesWidget:  composeWidget(changesWidget),
}

// composeWidget puts a kind's view into the base: its styles from widgets/<kind>.css, its markup
// from <kind>.html, and its script from <kind>.js. The base holds the theme styles and the status
// line under the view, and its script reads the message's data and redraws when it changes. The
// view's script runs inside the base's and defines draw(data), which draws the data; its controls
// call request(data, after), which disables every button, shows that the tap is being worked on,
// sends the data, shows the answer or words the failure, and ignores an answer that arrives after a
// redraw. After an answer the controls come back when after is controlsReturn, as after a Preview;
// with dataChanges the widget waits for its data to change, as an answer that acts changes it, and
// redraws after 20 seconds if it does not.
func composeWidget(kind widgetKind) string {
	read := func(name string) string {
		content, err := widgetFiles.ReadFile("widgets/" + name)
		if err != nil {
			panic(fmt.Sprintf("widget %s: %v", kind, err))
		}
		return string(content)
	}
	base := read("base.html")
	slots := []struct{ marker, extension string }{{"{{styles}}", ".css"}, {"{{markup}}", ".html"}, {"{{script}}", ".js"}}
	replacements := make([]string, 0, 2*len(slots))
	for _, slot := range slots {
		if strings.Count(base, slot.marker) != 1 {
			panic(fmt.Sprintf("widget base must hold %s once", slot.marker))
		}
		part := read(string(kind) + slot.extension)
		// A closing tag inside a view's styles or script would end the base's block early.
		if lower := strings.ToLower(part); slot.extension != ".html" && (strings.Contains(lower, "</script") || strings.Contains(lower, "</style")) {
			panic(fmt.Sprintf("widget %s%s must not hold a closing script or style tag", kind, slot.extension))
		}
		replacements = append(replacements, slot.marker, strings.TrimSuffix(part, "\n"))
	}
	return strings.NewReplacer(replacements...).Replace(base)
}

// plainWidgetText is text as a message would show it, without the formatting, which a widget does
// not draw.
func plainWidgetText(text string) string {
	return captionText(renderMarkup(text))
}

func marshalWidgetData(data any) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	// A widget's data is made of strings, slices, and structs of them, whose encoding cannot fail.
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

// sendWidget posts a widget of the kind with its data, which must fit widgetDataLimit, and its
// caption line under it. It never carries an inline keyboard, which makes some Telegram apps show a
// widget message as unsupported.
func (c *Client) sendWidget(ctx context.Context, chatID int64, kind widgetKind, data, caption string) (Message, error) {
	template, found := widgetTemplates[kind]
	if !found {
		return Message{}, fmt.Errorf("unknown widget kind %q", kind)
	}
	params := url.Values{
		"chat_id":       {strconv.FormatInt(chatID, 10)},
		"html":          {template},
		"chat_data":     {data},
		"allow_queries": {"true"},
	}
	var message Message
	err := c.inTab(ctx, chatID, true, func(ctx context.Context) error {
		params.Del("message_thread_id")
		setThread(ctx, params)
		var err error
		message, err = executeCaptioned[Message](c, ctx, "sendWidget", params, caption)
		return err
	})
	return message, err
}

// editWidget shows a widget's caption line and, unless data is empty, its data, which is sent
// whatever became of the caption. The widget keeps the program it was sent with. It reports whether
// the data changed, and returns the first error.
func (c *Client) editWidget(ctx context.Context, chatID, messageID int64, caption, data string) (bool, error) {
	params := url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	_, captionErr := executeCaptioned[json.RawMessage](c, ctx, "editMessageCaption", params, caption)
	if isNotModified(captionErr) {
		captionErr = nil
	}
	var changed bool
	var dataErr error
	if data != "" {
		changed, dataErr = c.editWidgetData(ctx, chatID, messageID, data)
	}
	if captionErr != nil {
		return changed, captionErr
	}
	return changed, dataErr
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
