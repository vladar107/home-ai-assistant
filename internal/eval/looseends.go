package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// looseEndNudger sends a nudge to the recording stub as a reply, so a run_job step's checks read
// it: a line per item with its kind, title, counterpart, and local due time, and for a sent promise
// the day the user wrote again in its thread.
type looseEndNudger struct {
	chat     *Chat
	location *time.Location
}

func (n looseEndNudger) Nudge(_ context.Context, _ string, items []looseend.DueItem) (string, schedule.Sent, error) {
	lines := []string{"Loose ends"}
	for _, item := range items {
		line := fmt.Sprintf("• %s: %s", item.Kind, item.Title)
		if item.Counterpart != "" {
			line += " (" + item.Counterpart + ")"
		}
		line += ", by " + item.DueAt.In(n.location).Format(ClockLayout)
		if !item.WroteAgain.IsZero() {
			line += "; you wrote again on " + item.WroteAgain.In(n.location).Format(time.DateOnly)
		}
		lines = append(lines, line)
	}
	text := strings.Join(lines, "\n")
	return text, schedule.Sent{MessageID: n.chat.reply(0, text)}, nil
}

// seedLooseEnds starts the principal's Loose ends scan when the world has the Loose ends job or a
// loose_ends section, from read_from with nothing to backfill when the section gives it, and tracks
// the section's items.
func (w *World) seedLooseEnds(ctx context.Context, hasJob bool) error {
	var section looseEndsSection
	hasSection, err := w.section(SectionLooseEnds, &section)
	if err != nil || (!hasJob && !hasSection) {
		return err
	}
	service := w.Assembly.LooseEnds
	if service == nil {
		return errors.New("loose_ends: the world has no Loose ends service")
	}
	if _, err := service.StartScan(ctx, PrincipalID); err != nil {
		return fmt.Errorf("loose_ends: %w", err)
	}
	if section.ReadFrom != "" {
		from, err := w.parseLocal(section.ReadFrom)
		if err != nil {
			return fmt.Errorf("loose_ends.read_from: %w", err)
		}
		if err := service.SaveScanState(ctx, PrincipalID, looseend.ScanState{ReadUntil: from.UTC()}); err != nil {
			return fmt.Errorf("loose_ends.read_from: %w", err)
		}
	}
	for i, seed := range section.Items {
		tracked, err := service.Track(ctx, PrincipalID, seed.draft())
		if err != nil || tracked.Outcome != looseend.OutcomeCreated {
			return fmt.Errorf("loose_ends.items[%d]: %s %s: %w", i, tracked.Outcome, tracked.Reason, err)
		}
	}
	return nil
}
