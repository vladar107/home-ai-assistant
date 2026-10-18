package judgment

import (
	"context"
	"sort"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/turnstate"
)

const (
	// hintConfidence is the least confidence at which a suggested tool is passed on; calibrate it
	// with the labelled run in plan 0029.
	hintConfidence     = 0.7
	hintNone           = "none"
	hintDescriptionMax = 300
	// maxHintTools keeps the option set, with "none", within what one Choice question accepts.
	maxHintTools = 254
)

// ToolHinter names the tool a message most likely needs.
type ToolHinter struct {
	client *Client
	optIn  OptIn
}

func NewToolHinter(client *Client, optIn OptIn) *ToolHinter {
	return &ToolHinter{client: client, optIn: optIn}
}

// Hint returns the name of the most likely tool, or "" when the principal has not opted in, no
// tool clearly fits, or the answer is not confident enough. earlier is the conversation just before
// message. answered reports whether TypeSafe was asked and gave an answer, so a turn can tell a
// hint that saw no fitting tool from one that never ran.
func (h *ToolHinter) Hint(ctx context.Context, principalID, message string, earlier []turnstate.Line, tools []agentruntime.ToolDefinition) (name string, answered bool, err error) {
	enabled, err := h.optIn.JudgmentsEnabled(ctx, principalID)
	if err != nil || !enabled || len(tools) == 0 {
		return "", false, err
	}
	sorted := append([]agentruntime.ToolDefinition(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if len(sorted) > maxHintTools {
		sorted = sorted[:maxHintTools]
	}
	options := map[string]string{hintNone: "No listed tool is needed."}
	for _, tool := range sorted {
		if tool.Name == hintNone {
			continue
		}
		options[tool.Name] = clip.Runes(tool.Description, hintDescriptionMax)
	}
	state := map[string]Redacted{"message": Redact(message)}
	if exchange := redactExchange(earlier); !exchange.Empty() {
		state["earlier"] = exchange
	}
	answers, err := h.client.Ask(ctx, "hint", state,
		map[string]Question{"tool": Choice(
			"Which one tool would the assistant most likely need to use to handle `message`? "+
				"`earlier`, when present, is the conversation just before it: read a short follow-up such as 'yes' or 'do it' as answering the assistant's last line there. "+
				"Pick none when the message is conversation, a follow-up that needs no lookup, or general knowledge.",
			options)})
	if err != nil {
		return "", false, err
	}
	name, confidence, ok := answers.Choice("tool")
	if !ok {
		return "", false, nil
	}
	if name == hintNone || confidence < hintConfidence {
		h.client.recordDecision(ctx, principalID, "hint", "none")
		return "", true, nil
	}
	h.client.recordDecision(ctx, principalID, "hint", "hinted")
	return name, true, nil
}
