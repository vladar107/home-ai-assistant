package conversation

import (
	"context"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

type NoTools struct{}

func (NoTools) ToolsFor(context.Context, string) ([]agentruntime.ToolDefinition, error) {
	return nil, nil
}
