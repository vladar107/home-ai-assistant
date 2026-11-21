package routes

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	promptEntryRunes   = 300
	promptSectionRunes = 4000
	// promptOmittedRunes keeps room for the line counting the routes left out.
	promptOmittedRunes = 60
)

const promptSectionHeading = "Saved routes: the user's saved journeys, oldest first. " +
	"When a question about getting somewhere matches a route's destination or weekday, call " + toolCheck + " with the route's name for that day before planning the trip with other tools; " +
	"report the legs it does not cover with other tools."

var _ turn.Section = PromptSection{}

// PromptSection is the routes section of the system prompt over the repository alone.
type PromptSection struct {
	repository Repository
	logger     *slog.Logger
}

// NewPromptSection returns the section over the principal's stored routes.
func NewPromptSection(repository Repository, logger *slog.Logger) PromptSection {
	return PromptSection{repository: repository, logger: logger}
}

// PromptSection lists the principal's saved routes for the system prompt, each with its weekdays,
// arrival target, and legs. Routes are ordered by creation, so an edit keeps a route in its place.
func (s PromptSection) PromptSection(ctx context.Context, principalID string) string {
	routes, err := s.repository.ListRoutes(ctx, principalID)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "routes.prompt.failed", "principal_id", principalID, "error", err)
		}
		return ""
	}
	if len(routes) == 0 {
		return ""
	}
	slices.SortStableFunc(routes, func(a, b Route) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	english := locale.With(ctx, locale.English)
	section := promptSectionHeading
	length := utf8.RuneCountInString(section)
	listed := 0
	for _, route := range routes {
		entry := "\n- " + clip.Runes(route.HeadingFor(english)+". Legs: "+route.OutlineFor(english), promptEntryRunes)
		entryLength := utf8.RuneCountInString(entry)
		if length+entryLength > promptSectionRunes-promptOmittedRunes {
			break
		}
		section += entry
		length += entryLength
		listed++
	}
	if omitted := len(routes) - listed; omitted > 0 {
		section += fmt.Sprintf("\n%d more not listed; %s shows them.", omitted, toolList)
	}
	return section
}
