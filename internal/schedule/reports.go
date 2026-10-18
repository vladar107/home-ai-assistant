package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolReports = "job_reports"

	reportRetention = 14 * 24 * time.Hour
	reportsKept     = 200
	// reportDigestWindow is how far back a turn that opens a conversation is told of reports.
	reportDigestWindow = 24 * time.Hour
	reportsNoticed     = 5
	reportExcerptRunes = 200
	reportsListed      = 20
	// reportResultBytes keeps a full report within the broker's result limit, with room for its
	// heading.
	reportResultBytes = 15 << 10
)

// Report is what a delivered run sent its owner. Its ID is the run's.
type Report struct {
	ID          string
	PrincipalID string
	JobID       string
	JobName     string
	JobType     string
	Text        string
	// Private is true when the text may retell Notion page content or NAS file contents.
	Private bool
	// External is true when the run read outside content, so the text may carry it.
	External    bool
	DeliveredAt time.Time
	// Messages are the Telegram messages the report was sent as, and Destination the job's
	// destination then; only saving uses them.
	Messages    []Sent
	Destination string
}

// Sent is one Telegram message a delivery went out as, with the tab it landed in: zero when it
// went to the main view, as a send to a tab that is gone falls back there.
type Sent struct {
	MessageID int64
	ThreadID  int64
}

const reportsGuidance = "Job reports: job_reports lists the reports the user's scheduled jobs and reminders sent them in the last two weeks, optionally for one job, " +
	"or returns one in full by its report id. Use it when the user refers to a report and the excerpt you were given is not enough."

var reportsDefinition = tools.Definition{
	Name:        toolReports,
	Description: "List the reports the user's scheduled jobs and reminders sent them recently, with excerpts, or return one report in full by its id.",
	Risk:        tools.RiskRead,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"report_id":{"type":"string","description":"A report id, to return that report in full."},` +
		`"job":{"type":"string","description":"A job id or name, to list only that job's reports."}},` +
		`"additionalProperties":false}`),
	// A report retells what its run read, such as web pages or mail.
	ThirdParty: true,
}

// saveReport keeps a delivered run's report. A failure only costs later turns the report, so it
// is logged rather than holding the run back, which would send the report again.
func (s *Service) saveReport(ctx context.Context, run Run, messages []Sent) {
	report := Report{
		ID:          run.ID,
		PrincipalID: run.PrincipalID,
		JobID:       run.JobID,
		JobName:     run.JobName,
		JobType:     run.JobType,
		Text:        reportText(run),
		Private:     run.Private,
		External:    run.External,
		DeliveredAt: s.now(),
		Messages:    messages,
		Destination: run.Destination,
	}
	if report.Text == "" {
		return
	}
	if err := s.repository.SaveJobReport(ctx, report, reportsKept); err != nil && ctx.Err() == nil {
		s.logger.WarnContext(ctx, "schedule.report.save_failed", "principal_id", run.PrincipalID, "job_id", run.JobID, "error", err)
	}
}

var _ conversation.Reports = (*Service)(nil)

// TurnReports lists, one line each, the reports the principal has not heard of, with the start of
// their text, and for a turn that opens a conversation the rest of the last day's, without it; the
// newest few are listed and the rest counted. A reply to a report's message gets that report in
// full. Only report text counts as outside content, so a digest of names and times alone does not
// mark every new conversation of the day as having read it. Failures leave the reports out.
func (s *Service) TurnReports(ctx context.Context, principalID string, turn conversation.ReportTurn) (string, bool, bool) {
	var replied Report
	var found bool
	if turn.ReplyToMessageID > 0 {
		var err error
		if replied, found, err = s.repository.JobReportByMessage(ctx, principalID, turn.ReplyToMessageID); err != nil {
			s.logReportFailure(ctx, principalID, "reply", err)
		}
	}
	taken, err := s.repository.TakeJobReports(ctx, principalID)
	if err != nil {
		s.logReportFailure(ctx, principalID, "notice", err)
	}
	reports := taken
	if turn.OpensSession {
		recent, err := s.repository.RecentJobReports(ctx, principalID, s.now().Add(-reportDigestWindow))
		if err != nil {
			s.logReportFailure(ctx, principalID, "digest", err)
		}
		for _, report := range recent {
			if !slices.ContainsFunc(reports, func(listed Report) bool { return listed.ID == report.ID }) {
				reports = append(reports, report)
			}
		}
		slices.SortStableFunc(reports, func(a, b Report) int { return a.DeliveredAt.Compare(b.DeliveredAt) })
	}
	if found {
		reports = slices.DeleteFunc(reports, func(report Report) bool { return report.ID == replied.ID })
	}
	var parts []string
	outside, private := false, false
	if len(reports) > 0 {
		earlier := max(len(reports)-reportsNoticed, 0)
		lines := []string{"Reports the user's scheduled jobs sent them, oldest first; call job_reports with a report id when the excerpt is not enough or there is none:"}
		for _, report := range reports[earlier:] {
			if !slices.ContainsFunc(taken, func(fresh Report) bool { return fresh.ID == report.ID }) {
				lines = append(lines, "- "+s.reportHeading(report))
				continue
			}
			lines = append(lines, "- "+s.reportLine(report))
			outside = outside || report.External
			private = private || report.Private
		}
		switch {
		case earlier == 1:
			lines = append(lines, "And 1 earlier report.")
		case earlier > 1:
			lines = append(lines, fmt.Sprintf("And %d earlier reports.", earlier))
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	if found {
		parts = append(parts, fmt.Sprintf("The user is replying to this report (report id %s) from %s, sent %s:\n%s",
			replied.ID, reportSource(replied), s.formatReportTime(replied.DeliveredAt), truncateRunes(replied.Text, maximumNoticeRunes)))
		outside = outside || replied.External
		private = private || replied.Private
	}
	return strings.Join(parts, "\n\n"), outside, private
}

func (s *Service) logReportFailure(ctx context.Context, principalID, stage string, err error) {
	if ctx.Err() == nil {
		s.logger.WarnContext(ctx, "schedule.report.read_failed", "principal_id", principalID, "stage", stage, "error", err)
	}
}

func (s *Service) reportLine(report Report) string {
	return s.reportHeading(report) + ": " + reportExcerpt(report.Text)
}

func (s *Service) reportHeading(report Report) string {
	return fmt.Sprintf("%s, %s, report id %s", reportSource(report), s.formatReportTime(report.DeliveredAt), report.ID)
}

func reportSource(report Report) string {
	if report.JobType == TypeReminder {
		return fmt.Sprintf("reminder %q (job id %s)", report.JobName, report.JobID)
	}
	return fmt.Sprintf("job %q (job id %s)", report.JobName, report.JobID)
}

// reportExcerpt is the start of a report on one line.
func reportExcerpt(text string) string {
	return truncateRunes(strings.Join(strings.Fields(text), " "), reportExcerptRunes)
}

func (s *Service) formatReportTime(at time.Time) string {
	location, err := LoadZone(s.zone)
	if err != nil {
		location = time.UTC
	}
	return FormatOccurrence(at, location)
}

// contributeReports offers job_reports only to a principal who has a report to read.
func (s *Service) contributeReports(ctx context.Context, principalID string, contribution *tools.Contribution) error {
	exists, err := s.repository.HasJobReports(ctx, principalID)
	if err != nil || !exists {
		return err
	}
	contribution.Guidance += "\n" + reportsGuidance
	contribution.Definitions = append(contribution.Definitions, reportsDefinition)
	return nil
}

func (s *Service) executeReports(ctx context.Context, principalID string, call tools.Call) (tools.Result, error) {
	var arguments struct {
		ReportID string `json:"report_id"`
		Job      string `json:"job"`
	}
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	if id := strings.TrimSpace(arguments.ReportID); id != "" {
		report, found, err := s.repository.LoadJobReport(ctx, principalID, id)
		if err != nil {
			return tools.Result{}, err
		}
		if !found {
			return tools.Result{Text: "There is no report with that id; job_reports without an id lists the recent ones.", IsError: true}, nil
		}
		text := report.Text
		if len(text) > reportResultBytes {
			text = strings.ToValidUTF8(text[:reportResultBytes], "") + "\n[The report continues beyond this point.]"
		}
		heading := fmt.Sprintf("Report %s from %s, sent %s:\n\n", report.ID, reportSource(report), s.formatReportTime(report.DeliveredAt))
		return tools.Result{Text: heading + text, Private: report.Private}, nil
	}
	reports, err := s.repository.RecentJobReports(ctx, principalID, s.now().Add(-reportRetention))
	if err != nil {
		return tools.Result{}, err
	}
	if job := strings.TrimSpace(arguments.Job); job != "" {
		reports = slices.DeleteFunc(reports, func(report Report) bool {
			return report.JobID != job && !strings.EqualFold(report.JobName, job)
		})
	}
	if len(reports) == 0 {
		return tools.Result{Text: "There are no such reports from the last two weeks."}, nil
	}
	lines := []string{"Reports, newest first:"}
	private := false
	for _, report := range reports[:min(len(reports), reportsListed)] {
		lines = append(lines, "- "+s.reportLine(report))
		private = private || report.Private
	}
	if more := len(reports) - reportsListed; more > 0 {
		lines = append(lines, fmt.Sprintf("And %d older ones.", more))
	}
	return tools.Result{Text: strings.Join(lines, "\n"), Private: private}, nil
}
