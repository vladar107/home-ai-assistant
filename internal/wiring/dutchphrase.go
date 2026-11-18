package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/dutchphrase"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

// phraseChecker runs the Dutch phrase's turn on the background task model, or on the chat default
// when the principal's subscription does not offer it, as a task created now would.
type phraseChecker struct {
	engine     *turn.Engine
	models     modelcatalog.Reader
	usage      modelcatalog.CachedUsage
	task, chat agentruntime.ModelEffort
	logger     *slog.Logger
}

func (c phraseChecker) RunPhrase(ctx context.Context, principalID, message string) (string, string, error) {
	model, _, err := modelcatalog.Effective(ctx, c.models, c.usage, principalID, c.task, c.chat)
	if err != nil {
		c.logger.WarnContext(ctx, "dutchphrase.model_catalog.failed", "principal_id", principalID, "error", err)
	}
	reply, err := c.engine.RunReview(ctx, principalID, message, model)
	return reply.Text, reply.ErrorKind, err
}

// headsUpPhrase gives Heads-up the Dutch phrase while the principal has its switch on.
type headsUpPhrase struct {
	toggle  integration.Toggle
	service *dutchphrase.Service
}

func (p headsUpPhrase) Enabled(ctx context.Context, principalID string) (bool, error) {
	return p.toggle.Enabled(ctx, principalID)
}

func (p headsUpPhrase) Phrase(ctx context.Context, principalID string, scheduledFor time.Time, location *time.Location, notes string, manual bool) (string, error) {
	return p.service.Phrase(ctx, principalID, scheduledFor, location, notes, manual)
}

// buildDutchPhrase gives the scheduler the Dutch phrase that ends the first Heads-up of each day.
func (a *Assembly) buildDutchPhrase(engine *turn.Engine) error {
	service, err := dutchphrase.NewService(dutchphrase.Config{
		Repository: a.in.Store, Preferences: a.in.Store, Logger: a.in.Logger, Now: a.in.Core.Now,
		Checker: phraseChecker{engine: engine, models: a.Models, usage: a.ChatGPT, task: a.TaskModel, chat: a.ChatModel, logger: a.in.Logger},
	})
	if err != nil {
		return err
	}
	a.Scheduler.SetHeadsUpPhrase(headsUpPhrase{toggle: a.Switches.Toggle(dutchphrase.Switch), service: service})
	return nil
}
