package telegram

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/routes"
)

const RouteGoneReply = "This route no longer exists."

// RouteController is implemented by the routes service; nil leaves routes out of /jobs.
type RouteController interface {
	Routes(context.Context, string) ([]routes.Route, error)
	Delete(context.Context, string, string) (bool, error)
}
