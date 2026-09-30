package judgment

import (
	"context"
	"sort"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
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
// tool clearly fits, or the answer is not confident enough.
func (h *ToolHinter) Hint(ctx context.Context, principalID, message string, tools []agentruntime.ToolDefinition) (string, error) {
	enabled, err := h.optIn.JudgmentsEnabled(ctx, principalID)
	if err != nil || !enabled || len(tools) == 0 {
		return "", err
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
		options[tool.Name] = truncateRunes(tool.Description, hintDescriptionMax)
	}
	answers, err := h.client.Ask(ctx, "hint",
		map[string]Redacted{"message": Redact(message)},
		map[string]Question{"tool": Choice(
			"Which one tool would the assistant most likely need to use to handle `message`? "+
				"Pick none when the message is conversation, a follow-up that needs no lookup, or general knowledge.",
			options)})
	if err != nil {
		return "", err
	}
	name, confidence, ok := answers.Choice("tool")
	if !ok {
		return "", nil
	}
	if name == hintNone || confidence < hintConfidence {
		h.client.recordDecision(ctx, principalID, "hint", "none")
		return "", nil
	}
	h.client.recordDecision(ctx, principalID, "hint", "hinted")
	return name, nil
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
