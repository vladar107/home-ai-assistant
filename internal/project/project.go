// Package project keeps each user's projects: a goal worked toward over days or weeks, such as
// finding a dentist, with a checklist of items and links to related things. The model changes them
// through tools, a project with tabs on has its own tab with a pinned summary, and its nudges and
// reminders are ordinary scheduled jobs whose destination is the project. Trips stay apart.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusDone      = "done"
	StatusAbandoned = "abandoned"

	LinkNotion     = "notion"
	LinkNAS        = "nas"
	LinkCollection = "collection"
	LinkIntake     = "intake"
	LinkURL        = "url"

	// MaximumOpenProjects is how many active or paused projects a principal keeps.
	MaximumOpenProjects = 20
	MaximumItems        = 100
	MaximumLinks        = 30

	maximumTitleRunes     = 100
	maximumGoalRunes      = 500
	maximumItemRunes      = 300
	maximumNoteRunes      = 500
	maximumReferenceRunes = 500
	maximumLabelRunes     = 100

	dateLayout = "2006-01-02"
)

var (
	statuses  = []string{StatusActive, StatusPaused, StatusDone, StatusAbandoned}
	linkKinds = []string{LinkNotion, LinkNAS, LinkCollection, LinkIntake, LinkURL}
)

// ErrTooManyProjects reports that the principal already keeps MaximumOpenProjects open projects.
var ErrTooManyProjects = errors.New("the principal already has the maximum number of open projects")

// Project is one principal's project with its items and links in their order.
type Project struct {
	ID          string
	PrincipalID string
	Title       string
	Goal        string
	Status      string
	// SummaryMessageID is the message pinned in the project's tab, or zero.
	SummaryMessageID int64
	Items            []Item
	Links            []Link
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Item is one entry of a project's checklist. DueOn is a local date, YYYY-MM-DD, or "".
type Item struct {
	ID     string    `json:"id,omitempty"`
	Text   string    `json:"text"`
	DoneAt time.Time `json:"done_at,omitzero"`
	DueOn  string    `json:"due_on,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// Link points from a project to a related thing: a Notion page, a NAS path, a collection by its ID,
// an intake record, or a web address.
type Link struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Label     string `json:"label,omitempty"`
}

// Open reports whether the project is still being worked on: active or paused.
func (p Project) Open() bool { return Open(p.Status) }

// Open reports whether a status is one of a project still being worked on.
func Open(status string) bool { return status == StatusActive || status == StatusPaused }

func (i Item) Done() bool { return !i.DoneAt.IsZero() }

// OpenItems are the items not checked off, in their order.
func (p Project) OpenItems() []Item {
	var open []Item
	for _, item := range p.Items {
		if !item.Done() {
			open = append(open, item)
		}
	}
	return open
}

// NextDue is the open item due first, if any has a due date.
func (p Project) NextDue() (Item, bool) {
	var next Item
	found := false
	for _, item := range p.OpenItems() {
		if item.DueOn != "" && (!found || item.DueOn < next.DueOn) {
			next, found = item, true
		}
	}
	return next, found
}

func (p Project) item(id string) (int, bool) {
	index := slices.IndexFunc(p.Items, func(item Item) bool { return item.ID == id })
	return index, index >= 0
}

// clone copies the project with its own items and links, so a change leaves the original alone.
func (p Project) clone() Project {
	p.Items, p.Links = slices.Clone(p.Items), slices.Clone(p.Links)
	return p
}

func cleanLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func plainText(value string, limit int, what string) (string, error) {
	text := cleanLine(value)
	if utf8.RuneCountInString(text) > limit {
		return "", fmt.Errorf("%s has at most %d characters", what, limit)
	}
	if strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must be plain text", what)
	}
	return text, nil
}

func normalizeTitle(value string) (string, error) {
	title, err := plainText(value, maximumTitleRunes, "a project title")
	if err != nil {
		return "", err
	}
	if title == "" {
		return "", fmt.Errorf("a project needs a title, such as find a dentist")
	}
	return title, nil
}

func normalizeGoal(value string) (string, error) {
	return plainText(value, maximumGoalRunes, "a goal")
}

func normalizeStatus(value string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(value))
	if !slices.Contains(statuses, status) {
		return "", fmt.Errorf("status must be one of %s", strings.Join(statuses, ", "))
	}
	return status, nil
}

func normalizeItemText(value string) (string, error) {
	text, err := plainText(value, maximumItemRunes, "an item")
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("an item needs its text")
	}
	return text, nil
}

func normalizeNote(value string) (string, error) {
	return plainText(value, maximumNoteRunes, "a note")
}

// normalizeDue checks a local due date; "" means none.
func normalizeDue(value string) (string, error) {
	due := strings.TrimSpace(value)
	if due == "" {
		return "", nil
	}
	parsed, err := time.Parse(dateLayout, due)
	if err != nil || parsed.Year() < 2000 || parsed.Year() > 2100 {
		return "", fmt.Errorf("due_on must be a date such as 2026-11-03")
	}
	return parsed.Format(dateLayout), nil
}

// normalizeItem checks an item's text, due date, and note; its ID and done time are the caller's.
func normalizeItem(item Item) (Item, error) {
	text, err := normalizeItemText(item.Text)
	if err != nil {
		return Item{}, err
	}
	due, err := normalizeDue(item.DueOn)
	if err != nil {
		return Item{}, err
	}
	note, err := normalizeNote(item.Note)
	if err != nil {
		return Item{}, err
	}
	return Item{ID: item.ID, Text: text, DoneAt: item.DoneAt, DueOn: due, Note: note}, nil
}

// normalizeLink checks a link's kind, reference, and label. A web address must be http or https;
// a collection is checked against the principal's collections by the caller.
func normalizeLink(link Link) (Link, error) {
	kind := strings.ToLower(strings.TrimSpace(link.Kind))
	if !slices.Contains(linkKinds, kind) {
		return Link{}, fmt.Errorf("kind must be one of %s", strings.Join(linkKinds, ", "))
	}
	reference, err := plainText(link.Reference, maximumReferenceRunes, "a reference")
	if err != nil {
		return Link{}, err
	}
	if reference == "" {
		return Link{}, fmt.Errorf("a link needs its reference")
	}
	if kind == LinkURL && !strings.HasPrefix(reference, "https://") && !strings.HasPrefix(reference, "http://") {
		return Link{}, fmt.Errorf("a url link must start with https:// or http://")
	}
	label, err := plainText(link.Label, maximumLabelRunes, "a label")
	if err != nil {
		return Link{}, err
	}
	return Link{Kind: kind, Reference: reference, Label: label}, nil
}

// itemID names the index-th item an approval adds, so executing it twice gives the same IDs.
func itemID(approvalID string, index int) string {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s:%d", approvalID, index))
	return hex.EncodeToString(digest[:4])
}
