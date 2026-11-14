package telegram

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/routes"
)

var RouteGoneReply = locale.T("This route no longer exists.", "Этого маршрута больше нет.")

// RouteController is implemented by the routes service; nil leaves routes out of /jobs.
type RouteController interface {
	Routes(context.Context, string) ([]routes.Route, error)
	Delete(context.Context, string, string) (bool, error)
}
