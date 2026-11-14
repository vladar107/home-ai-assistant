package watch

import (
	"context"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// Tabs keeps an agent watch's tab in step with it after a write; with tabs on, an agent watch
// without a tab gets one with its summary pinned. A watch of another kind has no tab, and one that
// is gone keeps the tab it had. It logs its own failures.
type Tabs interface {
	WatchChanged(ctx context.Context, principalID, watchID string)
}

// Reader is what a watch's tab needs of the watches: one watch and the record of its summary
// message.
type Reader struct {
	repository Repository
}

// NewReader returns the reader over the repository.
func NewReader(repository Repository) Reader {
	return Reader{repository: repository}
}

// Watch finds one of the principal's watches of any kind.
func (r Reader) Watch(ctx context.Context, principalID, id string) (Watch, bool, error) {
	return r.repository.LoadWatch(ctx, principalID, id)
}

// SetSummaryMessage records the message that sums the watch up in its tab; zero forgets it.
func (r Reader) SetSummaryMessage(ctx context.Context, principalID, id string, messageID int64) error {
	return r.repository.SetWatchSummary(ctx, principalID, id, messageID)
}

// Summary is the text pinned in a watch's tab: its name, its definition as the user confirmed it,
// and the stored facts a change to the watch writes. A mute for today is left out, since it ends
// without a write.
func Summary(watch Watch) string { return SummaryIn(locale.English, watch) }

var (
	summaryPaused    = locale.T("Paused; resume it in /jobs.", "На паузе; возобнови его в /jobs.")
	summaryTraveling = locale.T("Pauses while you travel.", "Приостанавливается, пока ты в поездке.")
)

// SummaryIn is Summary in the given language.
func SummaryIn(language locale.Language, watch Watch) string {
	lines := []string{watch.LabelIn(language), watch.DescribeIn(language)}
	if !watch.Enabled {
		lines = append(lines, summaryPaused.In(language))
	}
	if watch.TravelPause {
		lines = append(lines, summaryTraveling.In(language))
	}
	return strings.Join(lines, "\n")
}

// changed tells the tabs about a write to a watch. Their own failures are logged by them.
func (s *Service) changed(ctx context.Context, principalID, watchID string) {
	if s.tabs != nil {
		s.tabs.WatchChanged(ctx, principalID, watchID)
	}
}

// Restored gives each of the principal's agent watches its tab once a restore added watches.
func (s *Service) Restored(ctx context.Context, principalID string) {
	if s.tabs == nil {
		return
	}
	watches, err := s.repository.ListWatches(ctx, principalID)
	if err != nil {
		s.logger.WarnContext(ctx, "watch.restore.failed", "principal_id", principalID, "stage", "tabs", "error", err)
		return
	}
	for _, watch := range watches {
		if watch.Kind == KindAgent {
			s.changed(ctx, principalID, watch.ID)
		}
	}
}
