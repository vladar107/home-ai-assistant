package wiring

import (
	"context"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/placeholder"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

// PlaceholderRuntime wraps the model runtime in the placeholder layer over the store's vault.
// With hide false it still resolves tokens but hides nothing, as HOME_AGENT_PLACEHOLDERS=false
// asks.
func PlaceholderRuntime(store *sqlite.Store, inner agentruntime.AgentRuntime, hide bool, logger *slog.Logger) *placeholder.Runtime {
	return placeholder.NewRuntime(inner, placeholderVault{store: store}, hide, logger)
}

// placeholderText resolves the tokens in Pi session text for the conversation history.
type placeholderText struct {
	vault placeholderVault
}

func (t placeholderText) ResolveText(ctx context.Context, principalID, text string) (string, error) {
	return placeholder.New(t.vault, principalID).ResolveText(ctx, text)
}

// placeholderVault hands the store's placeholder vault to the placeholder layer.
type placeholderVault struct {
	store *sqlite.Store
}

func (v placeholderVault) Token(ctx context.Context, principalID string, kind placeholder.Kind, value string) (string, error) {
	return v.store.PlaceholderToken(ctx, principalID, string(kind), value)
}

func (v placeholderVault) Value(ctx context.Context, principalID string, kind placeholder.Kind, suffix string) (string, bool, error) {
	return v.store.PlaceholderValue(ctx, principalID, string(kind), suffix)
}

func (v placeholderVault) Entries(ctx context.Context, principalID string, kind placeholder.Kind) ([]placeholder.Entry, error) {
	stored, err := v.store.PlaceholderEntries(ctx, principalID, string(kind))
	if err != nil {
		return nil, err
	}
	entries := make([]placeholder.Entry, 0, len(stored))
	for _, entry := range stored {
		entries = append(entries, placeholder.Entry{Kind: placeholder.Kind(entry.Kind), Value: entry.Value, Suffix: entry.Suffix})
	}
	return entries, nil
}

// placeholderPlaces adds the calendar places the trip check reads to the principal's vault, so a
// trip update that quotes one reaches the model as a token.
type placeholderPlaces struct {
	vault placeholderVault
}

func (p placeholderPlaces) Declare(ctx context.Context, principalID string, places []string) error {
	return placeholder.New(p.vault, principalID).Declare(ctx, places)
}
