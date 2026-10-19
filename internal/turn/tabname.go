package turn

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	// tabNameRunes is the longest name the bot gives a tab.
	tabNameRunes = 32
	// tabNameInputRunes caps each side of the exchange a tab is named from.
	tabNameInputRunes = 1000
)

// errInvalidTabName means the model's answer was not a usable name.
var errInvalidTabName = errors.New("the model did not answer with a usable tab name")

// TabName is what the model chose for a tab. Icon is one of the offered emoji, or "" when it chose
// none of them.
type TabName struct {
	Name string
	Icon string
}

// NameTab chooses a short name for a tab, in the language of its conversation, and one of icons, from
// the conversation's first message and reply. Like RewriteAlert it runs on the principal's own
// subscription with no tools, memory, or web search and a fresh session, and it does not take the
// principal's turn slot. A name that is not one short line is an error; an icon outside icons is no
// icon.
func (e *Engine) NameTab(ctx context.Context, principalID, message, reply string, icons []string) (TabName, error) {
	connected, err := e.gate.Ready(ctx, principalID)
	if err != nil {
		return TabName{}, err
	}
	if !connected {
		return TabName{}, errNotConnected
	}
	sessionID, err := NewID()
	if err != nil {
		return TabName{}, err
	}
	iconLine := "Choose no icon: leave \"icon\" empty."
	if len(icons) > 0 {
		iconLine = "For \"icon\", choose the one of these emoji that fits the topic best, or leave it empty when none fits: " + strings.Join(icons, " ")
	}
	result, err := e.runtime.Run(ctx, agentruntime.RunRequest{
		PrincipalID: principalID,
		SessionID:   sessionID,
		Message: "[system] Name the chat tab that holds the conversation below. The name says what the conversation is about in at most " +
			"three words and " + strconv.Itoa(tabNameRunes) + " characters, in the language the user writes in, on one line, with no emoji, quotes, or links. " +
			iconLine + "\nReply with only a JSON object: {\"name\":\"…\",\"icon\":\"…\"}\n\n" +
			"User:\n" + capRunes(message, tabNameInputRunes) + "\n\nAssistant:\n" + capRunes(reply, tabNameInputRunes),
	}, discardEvents{})
	if err != nil {
		return TabName{}, err
	}
	return parseTabName(result.Final, icons)
}

// parseTabName reads the model's answer, a single JSON object, which may come in a code fence.
func parseTabName(answer string, icons []string) (TabName, error) {
	answer = strings.TrimSpace(answer)
	if fenced, found := strings.CutPrefix(answer, "```"); found {
		fenced = strings.TrimPrefix(fenced, "json")
		answer = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(fenced), "```"))
	}
	decoder := json.NewDecoder(strings.NewReader(answer))
	decoder.DisallowUnknownFields()
	var chosen struct {
		Name *string `json:"name"`
		Icon string  `json:"icon"`
	}
	if err := decoder.Decode(&chosen); err != nil || chosen.Name == nil || decoder.More() {
		return TabName{}, errInvalidTabName
	}
	name := strings.TrimSpace(*chosen.Name)
	if !validTabName(name) {
		return TabName{}, errInvalidTabName
	}
	named := TabName{Name: name}
	if icon := plainEmoji(chosen.Icon); icon != "" {
		for _, offered := range icons {
			if plainEmoji(offered) == icon {
				named.Icon = offered
				break
			}
		}
	}
	return named, nil
}

// domainLike is a bare domain such as example.com, which Telegram shows as a link.
var domainLike = regexp.MustCompile(`[\p{L}\p{N}]\.\p{L}{2,}`)

// validTabName accepts one line of plain text. The reply a name is chosen from may carry text from
// outside, and the name appears in messages where Telegram links domains and mentions, so those,
// invisible formatting such as bidi overrides and zero-width characters, and line separators are
// refused.
func validTabName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > tabNameRunes {
		return false
	}
	if strings.ContainsFunc(name, func(r rune) bool {
		return r != ' ' && (!unicode.IsPrint(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp))
	}) {
		return false
	}
	lower := strings.ToLower(name)
	return !strings.Contains(lower, "@") && !domainLike.MatchString(lower) &&
		!strings.Contains(lower, "://") && !strings.Contains(lower, "www.") && !strings.Contains(lower, "t.me/")
}

// plainEmoji drops the variation selector, which Telegram and models add or leave out freely.
func plainEmoji(emoji string) string {
	return strings.ReplaceAll(strings.TrimSpace(emoji), "\uFE0F", "")
}

func capRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}
