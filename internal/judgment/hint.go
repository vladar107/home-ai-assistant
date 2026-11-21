package judgment

import (
	"context"
	"sort"
	"strings"

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
	maxHintTools   = 254
	routeCheckTool = "route_check"
)

// SavedRoutes names a principal's saved routes for the tool hint, one entry per route with its
// name and weekdays, such as "work (Mon–Fri)". An entry holds nothing else of the route.
type SavedRoutes interface {
	RouteNames(ctx context.Context, principalID string) ([]string, error)
}

// ToolHinter names the tool a message most likely needs.
type ToolHinter struct {
	client *Client
	optIn  OptIn
	routes SavedRoutes
}

// NewToolHinter returns a hinter; with nil routes the hint never sees saved routes.
func NewToolHinter(client *Client, optIn OptIn, routes SavedRoutes) *ToolHinter {
	return &ToolHinter{client: client, optIn: optIn, routes: routes}
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
	instructions := "Which one tool would the assistant most likely need to use to handle `message`? " +
		"`earlier`, when present, is the conversation just before it: read a short follow-up such as 'yes' or 'do it' as answering the assistant's last line there. " +
		"A message that says the tab it is in is the tab for something, such as 'this is the tab for the trip', needs the tool that moves that thing to this tab. "
	if _, offered := options[routeCheckTool]; offered {
		if routes := h.savedRoutes(ctx, principalID); !routes.Empty() {
			state["routes"] = routes
			instructions += "`routes` is the user's saved routes with their weekdays: a message about travelling where or when one of them goes needs " + routeCheckTool + ". "
		}
	}
	instructions += "Pick none when the message is conversation, a follow-up that needs no lookup, or general knowledge."
	answers, err := h.client.Ask(ctx, "hint", state, map[string]Question{"tool": Choice(instructions, options)})
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

// savedRoutes is the principal's redacted route names and weekdays, empty when there are none. A
// failed read leaves the routes out, so the hint still runs.
func (h *ToolHinter) savedRoutes(ctx context.Context, principalID string) Redacted {
	if h.routes == nil {
		return Redacted{}
	}
	names, err := h.routes.RouteNames(ctx, principalID)
	if err != nil {
		if ctx.Err() == nil {
			h.client.logger.WarnContext(ctx, "judgment.hint.routes_failed", "principal_id", principalID, "error", err)
		}
		return Redacted{}
	}
	return Redact(strings.Join(names, "; "))
}
