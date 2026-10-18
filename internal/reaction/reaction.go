// Package reaction offers the model the tool that reacts to the user's message with an emoji, so an
// acknowledgement can be answered without a reply.
package reaction

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolReact = "react"

	guidance = "react sets an emoji reaction on the user's message. React rarely. " +
		"When the message needs no answer, such as thanks, ok, got it, another acknowledgement, or only an emoji, react and then reply with exactly " +
		prompt.NothingToAdd + " and nothing else; that reply is never sent. " +
		"Never answer with only an emoji as text; use react for that. " +
		"When the message agrees to something you offered or answers your question, do it instead. " +
		"Add a reaction to a real reply only when it adds warmth, and never react instead of doing what the user asked."

	resultText = "The reaction is set on the user's message. If it is the whole answer, reply with exactly " +
		prompt.NothingToAdd + " and nothing else; otherwise reply as usual."
)

// Outcomes of an emoji-only reply turned into a reaction.
const (
	// ConversionReacted is a reply whose emoji became the reaction.
	ConversionReacted = "reacted"
	// ConversionDropped is a reply left unsent because the turn had already reacted.
	ConversionDropped = "dropped"
)

// emojis is the curated subset of Telegram's bot reactions, spelled as the Bot API lists them: the
// heart is U+2764 without a variation selector.
var emojis = []string{"👍", "👌", "❤", "🔥", "🎉", "🤝", "🫡", "🤔", "😁", "🙏"}

// Emojis returns the reactions the tool may set, for creating their telemetry series.
func Emojis() []string {
	return slices.Clone(emojis)
}

var definition = func() tools.Definition {
	enum, err := json.Marshal(emojis)
	if err != nil {
		panic(err)
	}
	return tools.Definition{
		Name:        toolReact,
		Description: "React to the user's message with one emoji; a later call replaces the reaction.",
		// A write, so a run nobody watches does not get it.
		Risk: tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"emoji":{"type":"string","enum":` + string(enum) + `,"description":"The reaction."}},` +
			`"required":["emoji"],"additionalProperties":false}`),
	}
}()

// Origins finds the chat and Telegram message of the principal's conversation request.
type Origins interface {
	TaskOrigin(ctx context.Context, principalID, requestID string) (int64, int64, bool, error)
}

// Reactor sets the bot's reaction on a Telegram message.
type Reactor interface {
	SetMessageReaction(ctx context.Context, chatID, messageID int64, emoji string) error
}

// Telemetry counts the reactions the bot sets.
type Telemetry interface {
	RecordReaction(ctx context.Context, principal, emoji string)
	// RecordReactionConversion counts an emoji-only reply turned into a reaction, by outcome.
	RecordReactionConversion(ctx context.Context, principal, outcome string)
}

// Provider offers react on chat turns that answer a message the user wrote, and turns such a
// turn's emoji-only reply into a reaction.
type Provider struct {
	origins   Origins
	reactor   Reactor
	telemetry Telemetry
}

var (
	_ tools.Provider         = (*Provider)(nil)
	_ conversation.Reactions = (*Provider)(nil)
)

func NewProvider(origins Origins, reactor Reactor, telemetry Telemetry) (*Provider, error) {
	if origins == nil {
		return nil, fmt.Errorf("reaction origins are required")
	}
	if reactor == nil {
		return nil, fmt.Errorf("reaction reactor is required")
	}
	if telemetry == nil {
		return nil, fmt.Errorf("reaction telemetry is required")
	}
	return &Provider{origins: origins, reactor: reactor, telemetry: telemetry}, nil
}

func (p *Provider) Contribute(ctx context.Context, _ string) (tools.Contribution, error) {
	if _, chat := outbox.ChatRequest(tools.TurnFrom(ctx)); !chat {
		return tools.Contribution{}, nil
	}
	if run := tools.RunFrom(ctx); run == nil || !run.UserMessage() {
		return tools.Contribution{}, nil
	}
	return tools.Contribution{Guidance: guidance, Definitions: []tools.Definition{definition}}, nil
}

type arguments struct {
	Emoji string `json:"emoji"`
}

func (p *Provider) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if call.Name != toolReact {
		return tools.Result{Text: "This tool is not available.", IsError: true}, nil
	}
	var given arguments
	if err := tools.DecodeArguments(call.Arguments, &given); err != nil || !slices.Contains(emojis, given.Emoji) {
		return tools.Result{Text: "the arguments do not match the tool's schema", IsError: true}, nil
	}
	set, err := p.set(ctx, principalID, given.Emoji)
	if err != nil {
		return tools.Result{}, err
	}
	if !set {
		return tools.Result{Text: "There is no message to react to.", IsError: true}, nil
	}
	return tools.Result{Text: resultText}, nil
}

// ReplyAsReaction reports whether a chat turn's emoji-only reply may go unsent: the turn has
// already reacted, or the reply's first emoji is now the reaction on the user's message.
func (p *Provider) ReplyAsReaction(ctx context.Context, principalID, emoji string) (bool, error) {
	run := tools.RunFrom(ctx)
	if run == nil || !run.UserMessage() {
		return false, nil
	}
	if run.Reacted() {
		p.telemetry.RecordReactionConversion(ctx, principalID, ConversionDropped)
		return true, nil
	}
	emoji = normalize(emoji)
	if !slices.Contains(emojis, emoji) {
		return false, nil
	}
	set, err := p.set(ctx, principalID, emoji)
	if err != nil || !set {
		return false, err
	}
	p.telemetry.RecordReactionConversion(ctx, principalID, ConversionReacted)
	return true, nil
}

// set reacts to the message the call's chat turn answers, and reports false when there is none.
func (p *Provider) set(ctx context.Context, principalID, emoji string) (bool, error) {
	requestID, chat := outbox.ChatRequest(tools.TurnFrom(ctx))
	if !chat {
		return false, nil
	}
	chatID, messageID, found, err := p.origins.TaskOrigin(ctx, principalID, requestID)
	if err != nil || !found {
		return false, err
	}
	if err := p.reactor.SetMessageReaction(ctx, chatID, messageID, emoji); err != nil {
		return false, err
	}
	if run := tools.RunFrom(ctx); run != nil {
		run.MarkReacted()
	}
	p.telemetry.RecordReaction(ctx, principalID, emoji)
	return true, nil
}

// normalize spells an emoji the way the curated set does: Telegram's reactions take neither
// variation selectors nor skin tones.
func normalize(emoji string) string {
	return strings.Map(func(r rune) rune {
		if r == '︎' || r == '️' || (r >= 0x1F3FB && r <= 0x1F3FF) {
			return -1
		}
		return r
	}, emoji)
}
