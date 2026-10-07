// Package withdrawal offers the model the tool that withdraws its own pending proposals. It is
// apart from the approval package, which the tool broker's tests reach through judgment.
package withdrawal

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolWithdraw = "proposal_withdraw"

	guidance = "proposal_withdraw withdraws proposals of yours that are still waiting for the user's confirmation, such as one you notice is wrong, instead of asking the user to press Reject. " +
		"Give the item numbers, counting the waiting batch's proposals from 1 in the order you made them, or all; the result says how the remaining items are numbered now. " +
		"The user's proposal message shows what was withdrawn; an item the user already confirmed cannot be withdrawn."
)

var definition = tools.Definition{
	Name:        toolWithdraw,
	Description: "Withdraw your own proposals that are still waiting for the user's confirmation, by item number or all of them.",
	Risk:        tools.RiskWrite,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"items":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"integer","minimum":1,"maximum":10},"description":"Item numbers in the waiting batch, 1 for the first proposal."},` +
		`"all":{"type":"boolean","description":"Withdraw every item still waiting instead."}},` +
		`"additionalProperties":false}`),
}

// Withdrawer is the approval service as the tool sees it.
type Withdrawer interface {
	// Withdraw withdraws the numbered pending items of the principal's waiting bundle, or all of
	// them, returning the text for the model and whether anything was withdrawn.
	Withdraw(ctx context.Context, principalID string, items []int, all bool) (string, bool, error)
}

// Provider offers proposal_withdraw. It is a write tool, so unattended runs, which cannot propose,
// do not get it.
type Provider struct {
	Withdrawer Withdrawer
}

var _ tools.Provider = Provider{}

func (p Provider) Contribute(context.Context, string) (tools.Contribution, error) {
	return tools.Contribution{WriteGuidance: guidance, Definitions: []tools.Definition{definition}}, nil
}

type arguments struct {
	Items []int `json:"items"`
	All   bool  `json:"all"`
}

func (p Provider) Execute(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	if call.Name != toolWithdraw {
		return tools.Result{Text: "unknown approval tool", IsError: true}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	var given arguments
	if err := decoder.Decode(&given); err != nil {
		return tools.Result{Text: "the arguments do not match the tool's schema", IsError: true}, nil
	}
	if given.All == (len(given.Items) > 0) {
		return tools.Result{Text: "Give either items or all; nothing was withdrawn.", IsError: true}, nil
	}
	text, withdrawn, err := p.Withdrawer.Withdraw(ctx, principalID, given.Items, given.All)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Text: text, IsError: !withdrawn}, nil
}
