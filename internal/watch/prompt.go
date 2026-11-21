package watch

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

const (
	promptEntryRunes   = 300
	promptSectionRunes = 4000
	// promptOmittedRunes keeps room for the line counting the watches left out.
	promptOmittedRunes = 60
)

const promptSectionHeading = "Watches: the user's enabled watches, oldest first. " +
	"They check departures, routes, disruptions, home devices, weather, and places in code and alert the user on changes; " +
	"use them as facts about what the user follows."

var _ turn.Section = PromptSection{}

// PromptSection is the watches section of the system prompt over the repository alone.
type PromptSection struct {
	repository Repository
	logger     *slog.Logger
}

// NewPromptSection returns the section over the principal's stored watches.
func NewPromptSection(repository Repository, logger *slog.Logger) PromptSection {
	return PromptSection{repository: repository, logger: logger}
}

// PromptSection lists the principal's enabled watches for the system prompt, each with its id and
// its definition as the user confirmed it. Watches are ordered by creation, so an edit keeps a
// watch in its place.
func (s PromptSection) PromptSection(ctx context.Context, principalID string) string {
	watches, err := s.repository.ListWatches(ctx, principalID)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "watch.prompt.failed", "principal_id", principalID, "error", err)
		}
		return ""
	}
	watches = slices.DeleteFunc(watches, func(watch Watch) bool { return !watch.Enabled })
	if len(watches) == 0 {
		return ""
	}
	slices.SortStableFunc(watches, func(a, b Watch) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	section := promptSectionHeading
	length := utf8.RuneCountInString(section)
	listed := 0
	for _, watch := range watches {
		entry := "\n- " + clip.Runes(fmt.Sprintf("id %s: %s", watch.ID, oneLine(Summary(watch))), promptEntryRunes)
		entryLength := utf8.RuneCountInString(entry)
		if length+entryLength > promptSectionRunes-promptOmittedRunes {
			break
		}
		section += entry
		length += entryLength
		listed++
	}
	if omitted := len(watches) - listed; omitted > 0 {
		section += fmt.Sprintf("\n%d more not listed; %s shows them.", omitted, toolList)
	}
	return section
}

func oneLine(text string) string {
	var lines []string
	for line := range strings.Lines(text) {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "; ")
}
