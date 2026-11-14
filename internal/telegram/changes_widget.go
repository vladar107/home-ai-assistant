package telegram

import (
	"context"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

var (
	changesWarning   = locale.T("⚠️ A marked change carries a warning; open it before confirming.", "⚠️ У отмеченного изменения есть предупреждение; откройте его, прежде чем подтверждать.")
	changesConfirm   = locale.T("Confirm %[1]s of %[2]s", "Подтвердить %[1]s из %[2]s")
	changesRejectAll = locale.T("Reject all", "Отклонить все")
)

// changesLabels are the words the changes view draws, in a language. The view puts the numbers of
// its Confirm button where {1} and {2} stand, and picks the count line of the number of changes.
func changesLabels(language locale.Language) map[string]any {
	words := make(map[string]string)
	for state, word := range approval.StateWords(language) {
		words[string(state)] = word
	}
	counts := make([]string, approval.MaximumBundleItems+1)
	for number := range counts {
		counts[number] = approval.ChangeCount.FormatIn(language, number)
	}
	return map[string]any{
		"words":     words,
		"counts":    counts,
		"warning":   changesWarning.In(language),
		"confirm":   changesConfirm.FormatIn(language, "{1}", "{2}"),
		"rejectAll": changesRejectAll.In(language),
		"preview":   locale.Preview.In(language),
	}
}

// changesTitleFloor is how many characters of a title the data keeps, or the whole title when it
// is shorter: a title cut below it is too short to tell its change apart.
const changesTitleFloor = 24

// changesData is what a changes widget shows: a proposal of several changes, each with its title,
// state, and once resolved its outcome, and the full texts when they fit beside the titles. Its
// keys are one letter each to keep the data small. Changes widgets already in chats read this
// shape, so it only grows: a view reads what it knows and ignores the rest, and a state code it
// does not know is drawn by its outcome line.
//
//	i  the approval ID the widget's taps name
//	h  the heading of an offered proposal
//	c  the changes, in the order the user sees them
//	s  where the proposal stands
//	p  true when Preview is offered
//	g  the language the widget draws its labels in, none for English
type changesData struct {
	ID      string        `json:"i"`
	Heading string        `json:"h,omitempty"`
	Changes []changesItem `json:"c"`
	Status  string        `json:"s,omitempty"`
	Preview bool          `json:"p,omitempty"`
	Lang    string        `json:"g,omitempty"`
}

// changesItem is one change of a changes widget.
//
//	t  the title, which may be shortened
//	s  the state code, as approval.ChangeState
//	w  true when the intent check warned about the change
//	o  what a done change says beyond its state word, or a failed change's whole line
//	x  the change's whole text, when every text fits the data
type changesItem struct {
	Title   string `json:"t"`
	State   string `json:"s"`
	Warned  bool   `json:"w,omitempty"`
	Outcome string `json:"o,omitempty"`
	Text    string `json:"x,omitempty"`
}

// changesDataFor builds the data of the proposal shown, as the user reads it.
func changesDataFor(shown approval.Shown, preview bool) changesData {
	data := changesData{
		ID: shown.ID, Heading: plainWidgetText(shown.Heading), Status: plainWidgetText(shown.Status), Preview: preview, Lang: widgetLanguage(shown.Language),
		Changes: make([]changesItem, len(shown.Changes)),
	}
	for index, change := range shown.Changes {
		item := changesItem{Title: plainWidgetText(change.Title), State: string(change.State), Warned: change.Warned, Text: plainWidgetText(change.Text)}
		item.Outcome = plainWidgetText(outcomeOf(change))
		data.Changes[index] = item
	}
	return data
}

// outcomeOf is what a done or failed change says beyond its state word. A done change's outcome
// line without its leading word, which the state says. A failed change's whole line, which is its
// advice when it has no outcome of its own, such as after a restart.
func outcomeOf(change approval.Change) string {
	switch change.State {
	case approval.ChangeDone:
		return change.Detail
	case approval.ChangeFailed:
		return change.Outcome
	default:
		return ""
	}
}

// encodeChangesData encodes the proposal shown for a changes widget and reports whether it fits
// Telegram's limit, and whether anything beyond titles was shortened to make it. The full texts
// travel when they fit beside whole titles. Otherwise titles are shortened alike until the data
// fits, down to changesTitleFloor characters or the whole title when it is shorter. Only then are
// outcomes shortened, and as a last resort the status and the heading, which the caller makes up
// for by sending the text; the data is always updated, since the caption carries the tally.
func encodeChangesData(shown approval.Shown, preview bool) (data string, shortened, fits bool) {
	full := changesDataFor(shown, preview)
	maxTitle, maxOutcome := 0, 0
	for _, item := range full.Changes {
		maxTitle = max(maxTitle, utf8.RuneCountInString(item.Title))
		maxOutcome = max(maxOutcome, utf8.RuneCountInString(item.Outcome))
	}
	maxStatus, maxHeading := utf8.RuneCountInString(full.Status), utf8.RuneCountInString(full.Heading)
	type limits struct{ title, outcome, status, heading int }
	whole := limits{maxTitle, maxOutcome, maxStatus, maxHeading}
	encode := func(at limits, texts bool) (string, bool) {
		built := full
		built.Status, built.Heading = clip.Runes(full.Status, at.status), clip.Runes(full.Heading, at.heading)
		built.Changes = make([]changesItem, len(full.Changes))
		for index, item := range full.Changes {
			item.Title = clip.Runes(item.Title, max(at.title, min(changesTitleFloor, utf8.RuneCountInString(item.Title))))
			item.Outcome = clip.Runes(item.Outcome, at.outcome)
			if !texts {
				item.Text = ""
			}
			built.Changes[index] = item
		}
		encoded := marshalWidgetData(built)
		return encoded, len(encoded) <= widgetDataLimit
	}
	for _, texts := range []bool{true, false} {
		if encoded, ok := encode(whole, texts); ok {
			return encoded, false, true
		}
	}
	if longest := longestFit(maxTitle, func(limit int) bool {
		_, ok := encode(limits{limit, maxOutcome, maxStatus, maxHeading}, false)
		return ok
	}); longest >= 0 {
		encoded, _ := encode(limits{longest, maxOutcome, maxStatus, maxHeading}, false)
		return encoded, false, true
	}
	steps := []func(limit int) limits{
		func(limit int) limits { return limits{0, limit, maxStatus, maxHeading} },
		func(limit int) limits { return limits{0, 0, limit, maxHeading} },
		func(limit int) limits { return limits{0, 0, 0, limit} },
	}
	most := []int{maxOutcome, maxStatus, maxHeading}
	for index, step := range steps {
		longest := longestFit(most[index], func(limit int) bool {
			_, ok := encode(step(limit), false)
			return ok
		})
		if longest >= 0 {
			encoded, _ := encode(step(longest), false)
			return encoded, true, true
		}
	}
	return "", false, false
}

// sendChangesWidget posts a changes widget with the proposal's changes in its data and its caption
// line under it. It reports false, sending nothing, when the data cannot fit whole even with every
// title at its floor: a list sent with its status or heading cut is not worth drawing.
func (c *Client) sendChangesWidget(ctx context.Context, chatID int64, shown approval.Shown, preview bool) (Message, bool, error) {
	data, shortened, fits := encodeChangesData(shown, preview)
	if !fits || shortened {
		return Message{}, false, nil
	}
	message, err := c.sendWidget(ctx, chatID, changesWidget, data, shown.Caption)
	return message, true, err
}

// editChangesWidget shows a proposal on its changes widget: the caption line and the changes in
// the data, with the controls the data says. An outcome shortened to fit is sent in full as a
// reply to the widget, as the text of the message, once: data already showing it is not modified
// and sends no second reply. The first error is returned.
func (c *Client) editChangesWidget(ctx context.Context, chatID, messageID int64, shown approval.Shown, preview bool) error {
	data, shortened, fits := encodeChangesData(shown, preview)
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
