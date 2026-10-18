package wiring

import (
	"context"
	"maps"
	"slices"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
	"github.com/vladar107/home-ai-assistant/internal/tools"
	"github.com/vladar107/home-ai-assistant/internal/watch"
)

// Connector is what one integration adds to the application: its lifecycle, the tools it offers,
// the executor of its confirmed actions, the kinds of watch it checks, and its on/off page. Every
// field is optional and typed, and each is set inside the integration's own configuration check,
// so a connector that is not configured adds nothing rather than a typed nil.
type Connector struct {
	Module   module.Module
	Tools    []tools.Provider
	Executor approval.Executor
	Watches  map[string]watch.Evaluator
	Switch   *SwitchSettings
}

// SwitchSettings is what an integration's on/off page in /settings needs beyond the page itself.
type SwitchSettings struct {
	Switch integration.Switch
	// Allowed limits the page to some principals; nil shows it to everyone.
	Allowed func(principalID string) bool
	// Exposure is the home's check of which devices are exposed; nil for the others.
	Exposure func(context.Context) (checked bool, conflicts []string)
}

// register adds a connector to every part of the application it takes part in.
func (a *Assembly) register(ctx context.Context, c Connector) error {
	if c.Module != nil {
		if err := a.Registry.Register(c.Module); err != nil {
			return err
		}
	}
	for _, provider := range c.Tools {
		if err := a.Tools.Register(provider); err != nil {
			return err
		}
	}
	if c.Executor != nil {
		if err := a.Approvals.RegisterExecutor(c.Executor); err != nil {
			return err
		}
	}
	if err := a.registerWatches(ctx, c.Watches); err != nil {
		return err
	}
	if c.Switch != nil {
		a.SwitchPages = append(a.SwitchPages, *c.Switch)
	}
	return nil
}

// registerWatches offers watches of the given kinds, while the watch service runs; a test's
// evaluator replaces the connector's.
func (a *Assembly) registerWatches(ctx context.Context, evaluators map[string]watch.Evaluator) error {
	if a.Watches == nil || len(evaluators) == 0 {
		return nil
	}
	kinds := slices.Sorted(maps.Keys(evaluators))
	for _, kind := range kinds {
		evaluator := evaluators[kind]
		if replaced, found := a.in.WatchEvaluators[kind]; found {
			evaluator = replaced
		}
		if err := a.Watches.Register(kind, evaluator); err != nil {
			return err
		}
		a.watchKinds[kind] = true
	}
	a.in.Telemetry.ZeroWatchOutcomes(ctx, a.principals, kinds)
	return nil
}

// registerTestWatches offers the kinds a test gave an evaluator for that no connector offered.
func (a *Assembly) registerTestWatches(ctx context.Context) error {
	added := make(map[string]watch.Evaluator)
	for kind, evaluator := range a.in.WatchEvaluators {
		if !a.watchKinds[kind] {
			added[kind] = evaluator
		}
	}
	return a.registerWatches(ctx, added)
}
