package trip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/schedule"
)

// ReminderID is the ID of a booking's check-in reminder, derived so that syncing is idempotent.
func ReminderID(tripID, bookingID string) string {
	sum := sha256.Sum256([]byte("trip-reminder\x00" + tripID + "\x00" + bookingID))
	return hex.EncodeToString(sum[:8])
}

type scheduleReminders struct {
	schedules *schedule.Service
}

// NewScheduleReminders keeps check-in reminders as one-time reminder.text jobs.
func NewScheduleReminders(schedules *schedule.Service) Reminders {
	return scheduleReminders{schedules: schedules}
}

func (r scheduleReminders) SetReminder(ctx context.Context, principalID, id string, at time.Time, zone, text string) error {
	location, err := schedule.LoadZone(zone)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReminderRefused, err)
	}
	config, err := schedule.ReminderConfig(text)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReminderRefused, err)
	}
	target := at.Truncate(time.Minute).UTC()
	prepared, err := r.schedules.Prepare(principalID, schedule.ReminderName(text), schedule.TypeReminder,
		schedule.Timing{At: target.In(location).Format("2006-01-02 15:04"), Zone: zone}, config)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReminderRefused, err)
	}
	// A wall-clock time the autumn change repeats is read as the earlier of its two instants.
	if !prepared.Job.RunAt.Equal(target) {
		prepared.Job.RunAt, prepared.Job.NextRunAt, prepared.Schedule.Once = target, target, target
	}
	for attempt := 0; attempt < 2; attempt++ {
		jobs, err := r.schedules.Jobs(ctx, principalID)
		if err != nil {
			return err
		}
		found := false
		for _, job := range jobs {
			if job.ID != id {
				continue
			}
			found = true
			updated, err := r.schedules.Update(ctx, job, job.UpdatedAt, prepared)
			if err != nil {
				return err
			}
			if updated {
				return nil
			}
		}
		if found {
			continue
		}
		created, err := r.schedules.Create(ctx, id, prepared)
		if err != nil || created {
			return err
		}
		// A finished reminder with this ID is still kept; replace it with the new one.
		if _, err := r.schedules.Delete(ctx, principalID, id); err != nil {
			return err
		}
		if _, err := r.schedules.Create(ctx, id, prepared); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("the reminder kept changing while it was being moved")
}

func (r scheduleReminders) RemoveReminder(ctx context.Context, principalID, id string) error {
	_, err := r.schedules.Delete(ctx, principalID, id)
	return err
}
