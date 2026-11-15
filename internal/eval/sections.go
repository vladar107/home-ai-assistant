package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/memory"
	"github.com/vladar107/home-ai-assistant/internal/routes"
	"github.com/vladar107/home-ai-assistant/internal/weekday"
)

// The world sections a case may carry. Each section's shape is documented in evals/cases/README.md.
const (
	SectionGmail    = "gmail"
	SectionCalendar = "calendar"
	SectionNotion   = "notion"
	SectionJobs     = "jobs"
	SectionMemory   = "memory"
	SectionBacklog  = "backlog"
	SectionRoutes   = "routes"
	SectionPlaces   = "places"

	// DefaultAccount is the mailbox and calendar owner of a section that names none.
	DefaultAccount = "sam@example.test"

	jobAtLayout = "2006-01-02 15:04"
)

func init() {
	for name, validate := range map[string]func(json.RawMessage) error{
		SectionGmail:    validateGmail,
		SectionCalendar: validateCalendar,
		SectionNotion:   validateNotion,
		SectionJobs:     validateJobs,
		SectionMemory:   validateMemory,
		SectionBacklog:  validateBacklog,
		SectionRoutes:   validateRoutes,
		SectionPlaces:   validatePlaces,
	} {
		DefaultRegistry.RegisterWorldSection(name, validate)
	}
}

type gmailSection struct {
	Account  string         `json:"account,omitempty"`
	Messages []gmailMessage `json:"messages"`
}

type gmailMessage struct {
	ID          string         `json:"id"`
	ThreadID    string         `json:"thread_id,omitempty"`
	From        string         `json:"from"`
	To          string         `json:"to,omitempty"`
	Subject     string         `json:"subject"`
	Snippet     string         `json:"snippet,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	Received    string         `json:"received"`
	Body        string         `json:"body"`
	HTML        string         `json:"html,omitempty"`
	Attachments map[string]int `json:"attachments,omitempty"`
}

type calendarSection struct {
	Account string          `json:"account,omitempty"`
	Events  []calendarEvent `json:"events"`
}

type calendarEvent struct {
	ID          string             `json:"id"`
	Summary     string             `json:"summary"`
	Description string             `json:"description,omitempty"`
	Location    string             `json:"location,omitempty"`
	Start       string             `json:"start"`
	End         string             `json:"end"`
	AllDay      bool               `json:"all_day,omitempty"`
	Organizer   string             `json:"organizer,omitempty"`
	Attendees   []calendarAttendee `json:"attendees,omitempty"`
}

type calendarAttendee struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Response string `json:"response,omitempty"`
}

type notionSection struct {
	Pages []notionPage `json:"pages"`
}

type notionPage struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	URL   string   `json:"url,omitempty"`
	Lines []string `json:"lines,omitempty"`
}

type jobSeed struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Reminder string `json:"reminder,omitempty"`
	HeadsUp  bool   `json:"heads_up,omitempty"`
	Cron     string `json:"cron,omitempty"`
	At       string `json:"at,omitempty"`
	Zone     string `json:"zone,omitempty"`
	Model    string `json:"model,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type memorySeed struct {
	Kind string `json:"kind,omitempty"`
	Text string `json:"text"`
}

type backlogSeed struct {
	Title   string `json:"title"`
	Details string `json:"details,omitempty"`
}

type placeSeed struct {
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	RadiusM int     `json:"radius_m,omitempty"`
}

var kebabID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func decodeStrict(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the section")
	}
	return nil
}

func validateTime(field, value string) error {
	if _, err := time.Parse(ClockLayout, value); err != nil {
		return fmt.Errorf("%s %q: want %s", field, value, ClockLayout)
	}
	return nil
}

func uniqueIDs(kind string, ids []string) error {
	seen := map[string]bool{}
	for i, id := range ids {
		if id == "" {
			return fmt.Errorf("%s[%d]: id is required", kind, i)
		}
		if seen[id] {
			return fmt.Errorf("%s[%d]: duplicate id %q", kind, i, id)
		}
		seen[id] = true
	}
	return nil
}

func validateGmail(raw json.RawMessage) error {
	var section gmailSection
	if err := decodeStrict(raw, &section); err != nil {
		return err
	}
	ids := make([]string, 0, len(section.Messages))
	for i, message := range section.Messages {
		ids = append(ids, message.ID)
		if message.From == "" || message.Subject == "" || (message.Body == "" && message.HTML == "") {
			return fmt.Errorf("messages[%d]: from, subject and a body are required", i)
		}
		if err := validateTime(fmt.Sprintf("messages[%d].received", i), message.Received); err != nil {
			return err
		}
	}
	return uniqueIDs("messages", ids)
}

func validateCalendar(raw json.RawMessage) error {
	var section calendarSection
	if err := decodeStrict(raw, &section); err != nil {
		return err
	}
	ids := make([]string, 0, len(section.Events))
	for i, event := range section.Events {
		ids = append(ids, event.ID)
		if event.Summary == "" {
			return fmt.Errorf("events[%d]: summary is required", i)
		}
		if err := validateTime(fmt.Sprintf("events[%d].start", i), event.Start); err != nil {
			return err
		}
		if err := validateTime(fmt.Sprintf("events[%d].end", i), event.End); err != nil {
			return err
		}
		if event.End <= event.Start {
			return fmt.Errorf("events[%d]: end must be after start", i)
		}
	}
	return uniqueIDs("events", ids)
}

func validateNotion(raw json.RawMessage) error {
	var section notionSection
	if err := decodeStrict(raw, &section); err != nil {
		return err
	}
	ids := make([]string, 0, len(section.Pages))
	for i, page := range section.Pages {
		ids = append(ids, page.ID)
		if page.Title == "" {
			return fmt.Errorf("pages[%d]: title is required", i)
		}
	}
	return uniqueIDs("pages", ids)
}

func validateJobs(raw json.RawMessage) error {
	var jobs []jobSeed
	if err := decodeStrict(raw, &jobs); err != nil {
		return err
	}
	ids := make([]string, 0, len(jobs))
	for i, job := range jobs {
		ids = append(ids, job.ID)
		if !kebabID.MatchString(job.ID) {
			return fmt.Errorf("jobs[%d]: id %q: want lowercase kebab case", i, job.ID)
		}
		kinds := 0
		for _, set := range []bool{job.Prompt != "" && !job.HeadsUp, job.Reminder != "", job.HeadsUp} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			return fmt.Errorf("jobs[%d]: want exactly one of prompt, reminder or heads_up", i)
		}
		switch {
		case job.HeadsUp:
			if job.Cron != "" && job.At != "" {
				return fmt.Errorf("jobs[%d]: cron and at exclude each other", i)
			}
		case (job.Cron == "") == (job.At == ""):
			return fmt.Errorf("jobs[%d]: want exactly one of cron or at", i)
		}
		if job.At != "" {
			if _, err := time.Parse(jobAtLayout, job.At); err != nil {
				return fmt.Errorf("jobs[%d].at %q: want %s", i, job.At, jobAtLayout)
			}
		}
		if job.Reminder != "" && job.Cron != "" {
			return fmt.Errorf("jobs[%d]: a reminder runs once, so it takes at", i)
		}
		if job.Zone != "" {
			if _, err := time.LoadLocation(job.Zone); err != nil {
				return fmt.Errorf("jobs[%d].zone %q: %w", i, job.Zone, err)
			}
		}
	}
	return uniqueIDs("jobs", ids)
}

func validateMemory(raw json.RawMessage) error {
	var entries []memorySeed
	if err := decodeStrict(raw, &entries); err != nil {
		return err
	}
	for i, entry := range entries {
		switch entry.Kind {
		case "", memory.KindFact, memory.KindInstruction:
		default:
			return fmt.Errorf("memory[%d].kind %q: want %s or %s", i, entry.Kind, memory.KindFact, memory.KindInstruction)
		}
		if err := validateMemoryText(entry.Kind, entry.Text); err != nil {
			return fmt.Errorf("memory[%d]: %w", i, err)
		}
	}
	return nil
}

func validateMemoryText(kind, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("text is required")
	}
	limit := memory.MaximumFactRunes
	if kind == memory.KindInstruction {
		limit = memory.MaximumInstructionRunes
	}
	if utf8.RuneCountInString(text) > limit {
		return fmt.Errorf("text is longer than %d characters", limit)
	}
	return nil
}

func validateBacklog(raw json.RawMessage) error {
	var items []backlogSeed
	if err := decodeStrict(raw, &items); err != nil {
		return err
	}
	for i, item := range items {
		if strings.TrimSpace(item.Title) == "" {
			return fmt.Errorf("backlog[%d]: title is required", i)
		}
	}
	return nil
}

func validateRoutes(raw json.RawMessage) error {
	var specs []routes.Spec
	if err := decodeStrict(raw, &specs); err != nil {
		return err
	}
	if len(specs) > routes.MaximumRoutesPerPrincipal {
		return fmt.Errorf("at most %d routes", routes.MaximumRoutesPerPrincipal)
	}
	names := map[string]bool{}
	for i, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" {
			return fmt.Errorf("routes[%d]: name is required", i)
		}
		if names[spec.Name] {
			return fmt.Errorf("routes[%d]: duplicate name %q", i, spec.Name)
		}
		names[spec.Name] = true
		if _, err := weekday.Normalize(spec.Days); err != nil || len(spec.Days) == 0 {
			return fmt.Errorf("routes[%d].days: want weekday names", i)
		}
		if len(spec.Legs) == 0 {
			return fmt.Errorf("routes[%d]: at least one leg is required", i)
		}
		for j, leg := range spec.Legs {
			if leg.Mode == "" {
				return fmt.Errorf("routes[%d].legs[%d]: mode is required", i, j)
			}
		}
	}
	return nil
}

func validatePlaces(raw json.RawMessage) error {
	var places []placeSeed
	if err := decodeStrict(raw, &places); err != nil {
		return err
	}
	for i, place := range places {
		if strings.TrimSpace(place.Name) == "" {
			return fmt.Errorf("places[%d]: name is required", i)
		}
		if place.Lat < -90 || place.Lat > 90 || place.Lon < -180 || place.Lon > 180 {
			return fmt.Errorf("places[%d]: lat and lon are out of range", i)
		}
		if place.RadiusM < 0 {
			return fmt.Errorf("places[%d]: radius_m must not be negative", i)
		}
	}
	return nil
}

// longID is seedID for the rows whose IDs are 16 characters long.
func longID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// seedID is a stable ID for a seeded row, so a case's rows have the same IDs in every trial.
func seedID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:4])
}
