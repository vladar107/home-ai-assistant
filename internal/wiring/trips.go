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

// tripScanState tells the Trips scan and trip_list what Google can be read. google stays nil
// until the connector is set up, and while it is nil nothing can be read.
type tripScanState struct {
	schedules *schedule.Service
	google    *google.Service
}

func (s *tripScanState) MailReadable(ctx context.Context, principalID string) (bool, error) {
	return s.readable(ctx, principalID, func(status google.AccountStatus) bool {
		return status.MailGranted && status.MailEnabled
	})
}

// ScanOff is true while the principal has no enabled Trips job and a Google account whose mail or
// calendar can be read, the test the Trips section in /settings uses.
func (s *tripScanState) ScanOff(ctx context.Context, principalID string) (bool, error) {
	jobs, err := s.schedules.Jobs(ctx, principalID)
	if err != nil {
		return false, err
	}
	id := schedule.TripsJobID(principalID)
	for _, job := range jobs {
		if job.ID == id && job.Enabled {
			return false, nil
		}
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
