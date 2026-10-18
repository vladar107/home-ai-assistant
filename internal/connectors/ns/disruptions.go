package ns

import (
	"context"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/transit"
)

// Disruptions lists what NS reports in effect at at, under the principal's own trains switch:
// calamities first, then disruptions, then engineering works. An empty station asks about the whole
// country; a zero at means now.
func (s *Service) Disruptions(ctx context.Context, principalID, stationQuery string, at time.Time) ([]transit.Disruption, error) {
	enabled, err := s.Enabled(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, &transit.RefusalError{Text: Switch.OffText(), Unavailable: true, Disabled: true}
	}
	if at.IsZero() {
		at = s.now()
	}
	at = at.In(s.location).Truncate(time.Minute)
	var found station
	if strings.TrimSpace(stationQuery) != "" {
		list, failure := s.stationList(ctx, principalID)
		if failure != nil {
			return nil, &transit.RefusalError{Text: failure.Text, Unavailable: true}
		}
		if found, failure = pickStation(list, "station", stationQuery); failure != nil {
			return nil, &transit.RefusalError{Text: failure.Text}
		}
	}
	response, err := s.fetchDisruptions(ctx, found, at)
	if err != nil {
		s.logger.InfoContext(ctx, "ns.request.failed", "principal_id", principalID, "tool", "disruptions", "error_class", errorClass(err))
		return nil, &transit.RefusalError{Text: modelText(err), Unavailable: true}
	}
	selected, spans := selectDisruptions(response, at)
	result := make([]transit.Disruption, 0, len(selected))
	for index, entry := range selected {
		item := transit.Disruption{ID: entry.ID, Kind: strings.ToLower(text(entry.Type)), Title: text(entry.Title)}
		start, end := entry.Start, entry.End
		if span := spans[index]; span != nil {
			if span.Situation != nil {
				item.Situation = text(span.Situation.Label)
			}
			if span.Cause != nil {
				item.Cause = text(span.Cause.Label)
			}
			var advices []string
			for _, advice := range span.Advices {
				advices = append(advices, text(advice))
			}
			item.Advice = joinPresent("; ", advices...)
			item.Period = text(span.Period)
			if span.Start != "" || span.End != "" {
				start, end = span.Start, span.End
			}
		}
		item.Start, _ = parseTime(start)
		item.End, _ = parseTime(end)
		if entry.ExpectedDuration != nil {
			item.ExpectedEnd = text(entry.ExpectedDuration.Description)
		}
		seen := make(map[string]bool)
		for _, section := range entry.PublicationSections {
			for _, affected := range section.Section.Stations {
				code := strings.TrimSpace(affected.StationCode)
				if code == "" || seen[code] {
					continue
				}
				seen[code] = true
				item.Stations = append(item.Stations, transit.AffectedStation{Code: code, Name: text(affected.Name)})
			}
		}
		result = append(result, item)
	}
	return result, nil
}
