package eval

import (
	"bytes"
	"cmp"
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
	"github.com/vladar107/home-ai-assistant/internal/parcel"
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
	SectionParcels  = "parcels"
	// SectionAssistantMail is the assistant's own mailbox, whose mail a triage_mail step delivers.
	SectionAssistantMail = "assistant_mail"

	// DefaultAccount is the mailbox and calendar owner of a section that names none.
	DefaultAccount = "sam@example.test"
	// DefaultAssistantMailbox and DefaultAssistantTag are the assistant mailbox's address and the
	// eval principal's tag on it when the section names none.
	DefaultAssistantMailbox = "assistant@example.test"
	DefaultAssistantTag     = "sam1"

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
		SectionParcels:  validateParcels,

		SectionAssistantMail: validateAssistantMail,
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

// assistantMailSection is the assistant's own mailbox with the eval principal's tag, and the mail
// a triage_mail step delivers to it.
type assistantMailSection struct {
	Address  string                 `json:"address,omitempty"`
	Tag      string                 `json:"tag,omitempty"`
	Messages []assistantMailMessage `json:"messages"`
}

// assistantMailMessage is one message to the eval principal's tagged address. Received defaults to
// the case's clock and must not be earlier, since mail from before the mailbox started is never
// triaged; DMARC is pass or fail, pass by default.
type assistantMailMessage struct {
	ThreadID string `json:"thread_id,omitempty"`
	From     string `json:"from"`
	To       string `json:"to,omitempty"`
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Received string `json:"received,omitempty"`
	DMARC    string `json:"dmarc,omitempty"`
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
	Pages       []notionPage       `json:"pages,omitempty"`
	DataSources []notionDataSource `json:"data_sources,omitempty"`
	// Writes switches on the principal's Notion write grant, so the write tools are offered.
	Writes bool `json:"writes,omitempty"`
}

// notionDataSource is a table: its typed columns and its rows. The title column is named by
// TitleProperty and is not listed in Properties.
type notionDataSource struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	TitleProperty string           `json:"title_property,omitempty"`
	Properties    []notionProperty `json:"properties,omitempty"`
	Rows          []notionRow      `json:"rows,omitempty"`
}

type notionProperty struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
}

type notionRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Values holds a value per column name: a YYYY-MM-DD date or date-time string, a number, an
	// option name, text, or a bool; null or absent leaves the cell empty.
	Values map[string]any `json:"values,omitempty"`
}

const defaultNotionTitleProperty = "Title"

// notionPropertyTypes are the column types a case may give a table, and the JSON type of the
// values each takes.
var notionPropertyTypes = map[string]string{
	"date": "string", "number": "number", "select": "string", "status": "string", "rich_text": "string", "checkbox": "bool",
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

func validateAssistantMail(raw json.RawMessage) error {
	var section assistantMailSection
	if err := decodeStrict(raw, &section); err != nil {
		return err
	}
	if section.Tag != "" && !assistantTag.MatchString(section.Tag) {
		return fmt.Errorf("tag %q: want lowercase letters and digits", section.Tag)
	}
	if len(section.Messages) == 0 {
		return errors.New("messages: at least one message is required")
	}
	for i, message := range section.Messages {
		if message.From == "" || message.Subject == "" || message.Body == "" {
			return fmt.Errorf("messages[%d]: from, subject and body are required", i)
		}
		if message.Received != "" {
			if err := validateTime(fmt.Sprintf("messages[%d].received", i), message.Received); err != nil {
				return err
			}
		}
		if message.DMARC != "" && message.DMARC != "pass" && message.DMARC != "fail" {
			return fmt.Errorf("messages[%d].dmarc %q: want pass or fail", i, message.DMARC)
		}
	}
	return nil
}

var assistantTag = regexp.MustCompile(`^[a-z0-9]+$`)

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
	if err := uniqueIDs("pages", ids); err != nil {
		return err
	}
	sources := make([]string, 0, len(section.DataSources))
	for i, source := range section.DataSources {
		sources = append(sources, source.ID)
		if source.Title == "" {
			return fmt.Errorf("data_sources[%d]: title is required", i)
		}
		if err := validateNotionTable(i, source); err != nil {
			return err
		}
		for _, row := range source.Rows {
			ids = append(ids, row.ID)
		}
	}
	if err := uniqueIDs("data_sources", sources); err != nil {
		return err
	}
	return uniqueIDs("pages and rows", ids)
}

func validateNotionTable(i int, source notionDataSource) error {
	columns := map[string]string{}
	title := cmp.Or(source.TitleProperty, defaultNotionTitleProperty)
	for j, property := range source.Properties {
		_, known := notionPropertyTypes[property.Type]
		_, duplicate := columns[property.Name]
		switch {
		case property.Name == "" || property.Name == title || duplicate:
			return fmt.Errorf("data_sources[%d].properties[%d]: want a name that is unique and is not the title column %q", i, j, title)
		case !known:
			return fmt.Errorf("data_sources[%d].properties[%d]: type %q: want date, number, select, status, rich_text or checkbox", i, j, property.Type)
		case len(property.Options) > 0 && property.Type != "select" && property.Type != "status":
			return fmt.Errorf("data_sources[%d].properties[%d]: only a select or status takes options", i, j)
		}
		columns[property.Name] = property.Type
	}
	for j, row := range source.Rows {
		if row.Title == "" {
			return fmt.Errorf("data_sources[%d].rows[%d]: title is required", i, j)
		}
		for name, value := range row.Values {
			propertyType, known := columns[name]
			if !known {
				return fmt.Errorf("data_sources[%d].rows[%d]: value for unknown column %q", i, j, name)
			}
			if err := validateNotionValue(propertyType, value); err != nil {
				return fmt.Errorf("data_sources[%d].rows[%d].values.%s: %w", i, j, name, err)
			}
		}
	}
	return nil
}

func validateNotionValue(propertyType string, value any) error {
	if value == nil {
		return nil
	}
	switch notionPropertyTypes[propertyType] {
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("want a string for a %s column", propertyType)
		}
		if propertyType == "date" {
			if _, err := time.Parse("2006-01-02", text[:min(len(text), 10)]); err != nil {
				return fmt.Errorf("date %q: want YYYY-MM-DD, optionally with a time", text)
			}
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return errors.New("want a number")
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return errors.New("want true or false")
		}
	}
	return nil
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

// parcelSeed is one parcel the user follows; the section turns the Parcels switch on for the
// principal.
type parcelSeed struct {
	Carrier     string `json:"carrier"`
	Number      string `json:"number"`
	Title       string `json:"title,omitempty"`
	Sender      string `json:"sender,omitempty"`
	Status      string `json:"status"`
	WindowFrom  string `json:"window_from,omitempty"`
	WindowTo    string `json:"window_to,omitempty"`
	PickupPoint string `json:"pickup_point,omitempty"`
}

func (p parcelSeed) parcel() parcel.Parcel {
	return parcel.Parcel{
		Carrier: p.Carrier, Number: p.Number, Title: p.Title, Sender: p.Sender, Source: parcel.SourceMail,
		Status: parcel.ParseStatus(p.Status), PickupPoint: p.PickupPoint,
	}
}

func validateParcels(raw json.RawMessage) error {
	var parcels []parcelSeed
	if err := decodeStrict(raw, &parcels); err != nil {
		return err
	}
	for i, seed := range parcels {
		if err := seed.parcel().Validate(); err != nil {
			return fmt.Errorf("parcels[%d]: %w", i, err)
		}
		if parcel.ParseStatus(seed.Status) == parcel.StatusUnknown && seed.Status != string(parcel.StatusUnknown) {
			return fmt.Errorf("parcels[%d]: status %q is not one of the parcel statuses", i, seed.Status)
		}
		for _, value := range []string{seed.WindowFrom, seed.WindowTo} {
			if value == "" {
				continue
			}
			if _, err := time.Parse(ClockLayout, value); err != nil {
				return fmt.Errorf("parcels[%d]: %q is not a local time such as 2026-03-10T14:00", i, value)
			}
		}
	}
	return nil
}
