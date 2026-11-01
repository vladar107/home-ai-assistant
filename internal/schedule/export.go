package schedule

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

const (
	jobsTable    = "scheduled_jobs"
	storedLayout = "2006-01-02T15:04:05.000Z"
	onceLayout   = "2006-01-02 15:04"
)

// Exporter is the scheduled jobs and reminders section of a user's export: every job that has not
// finished, with its anchor, condition, and destination. A one-time job that has already run is
// not in it, and one whose time passed by the restore is skipped.
func (s *Service) Exporter() export.Exporter { return exporter{service: s} }

type exporter struct{ service *Service }

func (exporter) Section() export.Section {
	return export.Section{Name: "jobs", Label: "Scheduled jobs and reminders", Version: 1, Tables: []export.Table{{Name: jobsTable}}}
}

func (exporter) Keep(_ string, row export.Row) bool { return row.Null("finished_at") }

// Restored has the scheduler look for due jobs and read the new anchors at once.
func (e exporter) Restored(context.Context, string) {
	e.service.Wake()
	e.service.WakeAnchors()
}

// Plan checks each job as saving it would, after following its anchor's trip or place and its
// destination's trip or project to what they are once restored. A check-in reminder takes the ID
// its restored trip and booking give it, so the trips loop finds it as its own.
func (e exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	current, err := plan.Current(ctx, jobsTable)
	if err != nil {
		return err
	}
	counts := map[bool]int{}
	for _, row := range current {
		if row.Null("finished_at") {
			counts[!row.Null("run_at")]++
		}
	}
	plan.Limit(export.Limit{Table: jobsTable, Where: "finished_at IS NULL AND run_at IS NULL", Max: MaximumJobsPerPrincipal})
	plan.Limit(export.Limit{Table: jobsTable, Where: "finished_at IS NULL AND run_at IS NOT NULL", Max: MaximumOneTimeJobsPerPrincipal})
	rows := tables[jobsTable]
	wanted := make([]string, 0, len(rows))
	for _, row := range rows {
		id := row.Text("id")
		if reminder, found := plan.Resolve(export.RefTripReminder, id); found {
			id = reminder
		}
		wanted = append(wanted, id)
	}
	assigned, err := plan.Assign(ctx, jobsTable, wanted)
	if err != nil {
		return err
	}
	for index, row := range rows {
		title := export.Quote(row.Text("name"))
		assignment := assigned[wanted[index]]
		if assignment.Present {
			plan.Present()
			continue
		}
		if plan.Dropped(export.RefTripReminder, row.Text("id")) {
			plan.Skip(title, "the booking it reminds about is not restored")
			continue
		}
		write, reason := e.restored(plan, row)
		if reason != "" {
			plan.Skip(title, reason)
			continue
		}
		oneTime := !row.Null("run_at")
		limit := MaximumJobsPerPrincipal
		if oneTime {
			limit = MaximumOneTimeJobsPerPrincipal
		}
		if counts[oneTime] >= limit {
			plan.Skip(title, "you already have the most jobs of its kind there may be")
			continue
		}
		write["id"] = assignment.ID
		plan.Add(jobsTable, write)
		if text := jobText(row.Text("job_type"), write.Text("config")); text != "" {
			plan.AddingText(title, text)
		} else {
			plan.Adding(title)
		}
		counts[oneTime]++
	}
	return nil
}

// restored is the row to store for a job of the file, in the form saving the job stores, or why it
// is skipped.
func (e exporter) restored(plan *export.Planning, row export.Row) (export.Row, string) {
	s, now := e.service, plan.Now()
	if !row.Null("finished_at") {
		return nil, "it has already run"
	}
	_, createdOK := row.Time("created_at")
	_, updatedOK := row.Time("updated_at")
	nextRun, nextOK := row.Time("next_run_at")
	if !createdOK || !updatedOK || !nextOK {
		return nil, "it could not be read"
	}
	write := row.Clone()
	handler, anchor, condition, err := SplitConfig(json.RawMessage(row.Text("config")))
	if err != nil {
		return nil, err.Error()
	}
	if anchor != nil && anchor.Kind == AnchorTrip {
		target, found := plan.Resolve(export.RefTrip, anchor.TripID)
		if !found {
			return nil, "the trip it runs relative to is not in the file or here"
		}
		anchor.TripID = target
	}
	if anchor != nil && anchor.Kind == AnchorPlace {
		target, found := plan.Resolve(export.RefPlace, anchor.PlaceID)
		if !found {
			return nil, "the place it runs on is not in the file or here"
		}
		anchor.PlaceID = target
	}
	registered, known := s.types[row.Text("job_type")]
	if !known {
		return nil, "that kind of job is not available"
	}
	if condition != nil {
		if s.conditions == nil {
			return nil, "conditions are not available here, because watches are off"
		}
		spec, err := s.conditions.ValidateCondition(condition.Spec, registered.Light)
		if err != nil {
			return nil, err.Error()
		}
		condition.Spec = spec
	}
	config, err := JoinConfig(handler, anchor, condition)
	if err != nil {
		return nil, err.Error()
	}
	if destination := row.Text("destination"); destination != "" {
		if tripID, isTrip := thread.TripOf(destination); isTrip {
			target, found := plan.Resolve(export.RefTrip, tripID)
			if !found {
				return nil, "the trip whose tab it reports to is not in the file or here"
			}
			write["destination"] = thread.TripDestination(target)
		} else if projectID, isProject := thread.ProjectOf(destination); isProject {
			target, found := plan.Resolve(export.RefProject, projectID)
			if !found {
				return nil, "the project whose tab it reports to is not in the file or here"
			}
			write["destination"] = thread.ProjectDestination(target)
		}
	}
	zone := row.Text("zone")
	timing := Timing{Zone: zone}
	switch {
	case anchor != nil:
	case !row.Null("cron"):
		timing.Cron = row.Text("cron")
	case !row.Null("run_at"):
		runAt, ok := row.Time("run_at")
		location, err := LoadZone(zone)
		if !ok || err != nil {
			return nil, "its time could not be read"
		}
		if !runAt.After(now) {
			return nil, "its time has passed"
		}
		timing.At = runAt.In(location).Format(onceLayout)
	default:
		return nil, "its time could not be read"
	}
	prepared, err := s.Prepare(plan.PrincipalID(), row.Text("name"), row.Text("job_type"), timing, config)
	if err != nil {
		return nil, err.Error()
	}
	job := prepared.Job
	write["name"], write["zone"], write["config"] = job.Name, job.Zone, string(job.Config)
	switch {
	case anchor == nil && job.Cron != "":
		// A recurring job runs from its next occurrence after the restore, never catching up on the
		// ones it missed while it was gone.
		write["cron"], write["next_run_at"] = job.Cron, job.NextRunAt.UTC().Format(storedLayout)
	case anchor == nil:
		write["run_at"], write["next_run_at"] = job.RunAt.UTC().Format(storedLayout), job.NextRunAt.UTC().Format(storedLayout)
	case anchor.Repeats() && !nextRun.After(now):
		// The anchor's resolver, or the next arrival, finds a repeating anchor's next occurrence.
		write["next_run_at"] = now.AddDate(100, 0, 0).UTC().Format(storedLayout)
		write["occurrence_key"] = nil
	case !anchor.Repeats() && !nextRun.After(now):
		return nil, "its time has passed"
	}
	if anchor != nil && anchor.Kind == AnchorPlace {
		plan.Guard(export.RefPlace, anchor.PlaceID)
	}
	return write, ""
}

// jobText is what a job asks the model or tells the user, which a restore preview shows in full,
// with the condition it waits for. An agent condition is a model turn's instruction, so it is shown
// from its own check, never from the description stored beside it.
func jobText(jobType, config string) string {
	handler, anchor, condition, err := SplitConfig(json.RawMessage(config))
	if err != nil {
		return ""
	}
	var fields struct {
		Prompt string `json:"prompt"`
		Text   string `json:"text"`
	}
	_ = json.Unmarshal(handler, &fields)
	var lines []string
	switch jobType {
	case TypeModelTurn:
		if fields.Prompt != "" {
			lines = append(lines, fields.Prompt)
		}
	case TypeReminder:
		if fields.Text != "" {
			lines = append(lines, fields.Text)
		}
	}
	if anchor != nil && len(lines) > 0 {
		lines = append(lines, "Runs "+anchor.Describe()+".")
	}
	if condition != nil {
		var spec struct {
			Kind  string `json:"kind"`
			Check *struct {
				Instruction string `json:"instruction"`
				Condition   string `json:"condition"`
			} `json:"check"`
		}
		_ = json.Unmarshal(condition.Spec, &spec)
		if spec.Kind == "agent" && spec.Check != nil {
			lines = append(lines, "Only if a model turn, told: "+spec.Check.Instruction, "finds: "+spec.Check.Condition)
		} else {
			lines = append(lines, "Only if: "+condition.Description)
		}
	}
	return strings.Join(lines, "\n")
}
