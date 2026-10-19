package schedule

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
	promptExcerptRunes = 800
	promptSectionRunes = 10000
	// promptOmittedRunes keeps room for the line counting the jobs left out.
	promptOmittedRunes = 60
)

const promptSectionHeading = "Scheduled jobs: the user's enabled scheduled jobs and pending reminders, newest first. " +
	"Their instructions hold facts the user expects you to know, such as their stops, trains, routes, and what they care about. " +
	"When the user refers to something that fits a job, use these facts, and call schedule_get with the job's id for its full instructions when the excerpt is not enough. " +
	"Before asking the user about their plans, trips, or routine, read the job that covers them with schedule_get."

var (
	_ turn.Jobs = (*Service)(nil)
	_ turn.Jobs = PromptSection{}
)

// PromptSection is the jobs section of the system prompt over the repository alone, so a turn
// reads it without the scheduler.
type PromptSection struct {
	repository Repository
	zone       string
	logger     *slog.Logger
}

// NewPromptSection returns the section for jobs described in the zone.
func NewPromptSection(repository Repository, zone string, logger *slog.Logger) PromptSection {
	return PromptSection{repository: repository, zone: zone, logger: logger}
}

// PromptSection is the scheduler's own section, the same as the value's.
func (s *Service) PromptSection(ctx context.Context, principalID string) string {
	return NewPromptSection(s.repository, s.zone, s.logger).PromptSection(ctx, principalID)
}

// PromptSection lists the principal's enabled jobs and pending reminders for the system prompt,
// each with the start of its instructions. Jobs are ordered by creation rather than by last
// change, because every run moves a job's last change and would change the prompt with it.
func (s PromptSection) PromptSection(ctx context.Context, principalID string) string {
	jobs, err := s.repository.ListScheduledJobs(ctx, principalID)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "schedule.prompt.failed", "principal_id", principalID, "error", err)
		}
		return ""
	}
	jobs = slices.DeleteFunc(jobs, func(job Job) bool { return !job.Enabled })
	if len(jobs) == 0 {
		return ""
	}
	slices.SortStableFunc(jobs, func(a, b Job) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	section := promptSectionHeading
	length := utf8.RuneCountInString(section)
	listed := 0
	for _, job := range jobs {
		entry := "\n- " + s.promptEntry(job)
		entryLength := utf8.RuneCountInString(entry)
		if length+entryLength > promptSectionRunes-promptOmittedRunes {
			break
		}
		section += entry
		length += entryLength
		listed++
	}
	if omitted := len(jobs) - listed; omitted > 0 {
		section += fmt.Sprintf("\n%d more not listed; schedule_list shows them.", omitted)
	}
	return section
}

func (s PromptSection) promptEntry(job Job) string {
	if job.Type == TypeReminder {
		return fmt.Sprintf("id %s: reminder, %s: %s", job.ID, DescribeJob(job, s.zone), promptExcerpt(ReminderText(job.Config)))
	}
	return fmt.Sprintf("id %s: %q, %s: %s", job.ID, job.Name, DescribeJob(job, s.zone), promptExcerpt(ModelTurnPrompt(job.Config)))
}

func promptExcerpt(text string) string {
	return clip.Runes(strings.Join(strings.Fields(text), " "), promptExcerptRunes)
}
