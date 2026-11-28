package wiring

import (
	"context"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/looseend"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

// looseEndService is the Loose ends service that the chat tools, the scan, and the daily job share,
// built once, in the household's zone.
func (a *Assembly) looseEndService() (*looseend.Service, error) {
	if a.LooseEnds != nil {
		return a.LooseEnds, nil
	}
	runTime, err := looseEndRunTime()
	if err != nil {
		return nil, fmt.Errorf("initialize loose ends: %w", err)
	}
	service, err := looseend.NewService(looseend.Config{Location: a.location, RunTime: runTime, Now: a.in.Core.Now}, a.in.Store, a.in.Logger)
	if err != nil {
		return nil, fmt.Errorf("initialize loose ends: %w", err)
	}
	a.LooseEnds = service
	return service, nil
}

// looseEndRunTime is the local time of day the Loose ends job runs at, read from its cron
// expression so the nudge rules and the job cannot drift apart.
func looseEndRunTime() (time.Duration, error) {
	run, err := schedule.ParseSchedule(schedule.LooseEndsCron, "UTC")
	if err != nil {
		return 0, err
	}
	midnight := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	return run.Next(midnight.Add(-time.Nanosecond)).Sub(midnight), nil
}

// buildLooseEndTools offers the Loose ends chat tools while the principal has Loose ends on, and
// has the content intake name loose_end_track among its follow-ups then.
func (a *Assembly) buildLooseEndTools(ctx context.Context) error {
	service, err := a.looseEndService()
	if err != nil {
		return err
	}
	offered, err := looseend.NewTools(service, a.Approvals, a.in.Logger)
	if err != nil {
		return fmt.Errorf("initialize loose end tools: %w", err)
	}
	if err := a.register(ctx, Connector{Tools: []tools.Provider{offered}, Executor: offered}); err != nil {
		return err
	}
	a.Intake.SetLooseEnds(a.Switches.Toggle(looseend.Switch))
	return nil
}

// buildLooseEnds registers the daily Loose ends job when the inputs can send nudges. The job runs
// the scan after its nudges; tests may replace the scan through the inputs.
func (a *Assembly) buildLooseEnds(scheduler *schedule.Service) error {
	in := a.in
	service, err := a.looseEndService()
	if err != nil {
		return err
	}
	if in.LooseEnds == nil {
		return nil
	}
	scanner := in.LooseEndScanner
	if scanner == nil {
		scan, err := a.looseEndScanner(service)
		if err != nil {
			return fmt.Errorf("initialize the Loose ends scan: %w", err)
		}
		scanner = scan
	}
	job, err := service.JobType(looseend.JobConfig{
		Switch: a.Switches.Toggle(looseend.Switch), Nudger: in.LooseEnds(service), Scanner: scanner, Threads: looseEndMail{a: a}, Reports: scheduler, Now: scheduler.Now,
	})
	if err != nil {
		return fmt.Errorf("initialize the Loose ends job: %w", err)
	}
	return scheduler.Register(job)
}
