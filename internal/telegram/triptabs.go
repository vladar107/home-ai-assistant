package telegram

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
)

const (
	// tripTabIcon is the emoji of a trip tab's icon, used when Telegram's icon set offers it.
	tripTabIcon = "✈️"
	// tripTabFallbackName names the tab of a trip whose title leaves nothing a tab can show.
	tripTabFallbackName = "Trip"
)

var _ TripFeeds = (*Tabs)(nil)

// Summarize keeps a trip's tab in step after a write to the trip: a planned trip without a tab gets
// one, its summary is edited, or sent and pinned again when it is gone, and the tab is renamed when
// the title changed. A failure is logged and left to the next write.
func (s *Tabs) Summarize(ctx context.Context, principalID string, chatID int64, tripID string) {
	s.summarize(ctx, thread.ObjectTrip, principalID, chatID, tripID)
}

// tripKind is the trips as their tabs see them. A trip keeps its tab whatever becomes of it, so
// its end notice lands there; only a planned trip that has not ended is given one.
func tripKind(trips TripReader) objectKind {
	return objectKind{
		kind: thread.ObjectTrip, icon: tripTabIcon, fallbackName: tripTabFallbackName,
		event: "telegram.trip_tab", idKey: "trip_id",
		load: func(ctx context.Context, principalID, id string) (tabObject, bool, error) {
			found, known, err := trips.Trip(ctx, principalID, id)
			if err != nil || !known {
				return tabObject{}, known, err
			}
			now := trips.Now()
			return tabObject{
				ID: found.ID, Title: found.Title, SummaryMessageID: found.SummaryMessageID,
				Open:    found.Status == trip.StatusPlanned && !found.Ended(now),
				Summary: trip.Summary(found, now),
			}, true, nil
		},
		setSummary: trips.SetSummaryMessage,
	}
}
