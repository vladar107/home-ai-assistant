package wiring

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/connectors/google"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

type tripScanChecker struct {
	engine *turn.Engine
}

func (c tripScanChecker) RunCheck(ctx context.Context, principalID, message string) (string, string, error) {
	reply, err := c.engine.RunCheck(ctx, principalID, message)
	return reply.Text, reply.ErrorKind, err
}

// tripScanState tells the Trips scan, the check for trip updates, and trip_list what Google and
// the web can be read and whether the scan is on. google stays nil until the connector is set up,
// and while it is nil nothing can be read.
type tripScanState struct {
	schedules *schedule.Service
	google    *google.Service
	// web is nil while the web connector is not configured.
	web turn.WebSearch
}

func (s *tripScanState) MailReadable(ctx context.Context, principalID string) (bool, error) {
	return s.readable(ctx, principalID, func(status google.AccountStatus) bool {
		return status.MailGranted && status.MailEnabled
	})
}

func (s *tripScanState) CalendarReadable(ctx context.Context, principalID string) (bool, error) {
	return s.readable(ctx, principalID, func(status google.AccountStatus) bool {
		return status.CalendarGranted && status.CalendarEnabled
	})
}

func (s *tripScanState) WebReadable(ctx context.Context, principalID string) (bool, error) {
	if s.web == nil {
		return false, nil
	}
	return s.web.WebSearchEnabled(ctx, principalID)
}

// ScanEnabled is true while the principal's Trips job exists and is enabled.
func (s *tripScanState) ScanEnabled(ctx context.Context, principalID string) (bool, error) {
	jobs, err := s.schedules.Jobs(ctx, principalID)
	if err != nil {
		return false, err
	}
	id := schedule.TripsJobID(principalID)
	for _, job := range jobs {
		if job.ID == id && job.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// ScanOff is true while the principal has no enabled Trips job and a Google account whose mail or
// calendar can be read, the test the Trips section in /settings uses.
func (s *tripScanState) ScanOff(ctx context.Context, principalID string) (bool, error) {
	if enabled, err := s.ScanEnabled(ctx, principalID); err != nil || enabled {
		return false, err
	}
	return s.readable(ctx, principalID, func(status google.AccountStatus) bool {
		return status.MailGranted && status.MailEnabled || status.CalendarGranted && status.CalendarEnabled
	})
}

func (s *tripScanState) readable(ctx context.Context, principalID string, test func(google.AccountStatus) bool) (bool, error) {
	if s.google == nil {
		return false, nil
	}
	statuses, err := s.google.Status(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
		if !status.NeedsReconnect && test(status) {
			return true, nil
		}
	}
	return false, nil
}
