package modelcatalog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/chatgpt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

const (
	maximumModelIDLength = 200
	// usageMaxAge is how old a cached usage read may be and still mark a model unavailable.
	usageMaxAge = 30 * time.Minute
)

// Why a choice runs on the chat default instead.
const (
	FallbackCatalog = "catalog"
	FallbackUsage   = "usage"
)

// CachedUsage is the last subscription usage read of a principal, never fetched on demand.
type CachedUsage interface {
	CachedUsage(principalID string, maxAge time.Duration) (chatgpt.Usage, bool)
}

// ValidChoice reports whether the pair is a well-formed model id with a level Pi accepts.
func ValidChoice(choice agentruntime.ModelEffort) bool {
	id := choice.Model
	if id == "" || len(id) > maximumModelIDLength || id != strings.TrimSpace(id) || !agentruntime.ValidEffort(choice.Effort) {
		return false
	}
	return !strings.ContainsFunc(id, unicode.IsControl)
}

// Offered returns the level the models run the choice on, and false when they do not offer the
// model or it does not run on the effort.
func Offered(models []Model, choice agentruntime.ModelEffort) (string, bool) {
	index := slices.IndexFunc(models, func(m Model) bool { return m.ID == choice.Model })
	if index < 0 {
		return "", false
	}
	return models[index].Canonical(choice.Effort)
}

// Effective is the pair a run that wants a model should use, and why it is the chat default when
// it is: FallbackCatalog when the principal's readable catalog lacks the model or its effort,
// FallbackUsage when the cached usage read marks the model unavailable. An unreadable catalog and
// a missing usage read are no reason to fall back, and models and usage may be nil. The error
// reports a catalog read that failed for another reason than ErrUnavailable, for the caller to
// log; the pair is then judged as with an unreadable catalog.
func Effective(ctx context.Context, models Reader, usage CachedUsage, principalID string, want, chat agentruntime.ModelEffort) (agentruntime.ModelEffort, string, error) {
	if want.Model == "" || want == chat || chat.Model == "" {
		return want, "", nil
	}
	var readErr error
	if models != nil {
		listed, err := models.Models(ctx, principalID)
		switch {
		case errors.Is(err, ErrUnavailable):
		case err != nil:
			readErr = err
		default:
			index := slices.IndexFunc(listed, func(m Model) bool { return m.ID == want.Model })
			if index < 0 || !listed[index].Supports(want.Effort) {
				return chat, FallbackCatalog, nil
			}
		}
	}
	if usage != nil {
		if read, ok := usage.CachedUsage(principalID, usageMaxAge); ok {
			if available, marked := read.ModelAvailable[want.Model]; marked && !available {
				return chat, FallbackUsage, readErr
			}
		}
	}
	return want, "", readErr
}
