// Package reaction offers the model the tool that reacts to the user's message with an emoji, so an
// acknowledgement can be answered without a reply.
package reaction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolReact = "react"

	guidance = "react sets an emoji reaction on the user's message. React rarely. " +
		"When the message needs no answer, such as thanks, ok, got it, another acknowledgement, or only an emoji, react and then reply with exactly " +
		conversation.NothingToAdd + " and nothing else; that reply is never sent. " +
		"When the message agrees to something you offered or answers your question, do it instead. " +
		"Add a reaction to a real reply only when it adds warmth, and never react instead of doing what the user asked."

	resultText = "The reaction is set on the user's message. If it is the whole answer, reply with exactly " +
		conversation.NothingToAdd + " and nothing else; otherwise reply as usual."
)

// emojis is the curated subset of Telegram's bot reactions, spelled as the Bot API lists them: the
// heart is U+2764 without a variation selector.
var emojis = []string{"👍", "👌", "\u2764", "🔥", "🎉", "🤝", "🫡", "🤔", "😁", "🙏"}

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
}

// Provider offers react on chat turns that answer a message the user wrote.
type Provider struct {
	origins   Origins
	reactor   Reactor
	telemetry Telemetry
}

var _ tools.Provider = (*Provider)(nil)

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
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	var given arguments
	if err := decoder.Decode(&given); err != nil || !slices.Contains(emojis, given.Emoji) {
		return tools.Result{Text: "the arguments do not match the tool's schema", IsError: true}, nil
	}
	requestID, chat := outbox.ChatRequest(tools.TurnFrom(ctx))
	if !chat {
		return tools.Result{Text: "There is no message to react to.", IsError: true}, nil
	}
	chatID, messageID, found, err := p.origins.TaskOrigin(ctx, principalID, requestID)
	if err != nil {
		return tools.Result{}, err
	}
	if !found {
		return tools.Result{Text: "There is no message to react to.", IsError: true}, nil
	}
	if err := p.reactor.SetMessageReaction(ctx, chatID, messageID, given.Emoji); err != nil {
		return tools.Result{}, err
	}
	p.telemetry.RecordReaction(ctx, principalID, given.Emoji)
	return tools.Result{Text: resultText}, nil
}
