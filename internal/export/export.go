// Package export gives a user one signed file holding what they configured and keep in the
// assistant, and restores such a file into the same user's account. Each owner of user data
// contributes a section through an Exporter, which wiring registers in the order the sections
// restore. A restore adds only what is missing, through the owner's own validation, and never
// overwrites; secrets, credentials, conversations, and operational records are in no section.
package export

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Row is one stored row as a section carries it: column names to values, which are strings,
// integers, floats, or nil as SQLite holds them, and json.Number for numbers read from a file.
// Every row is the principal's, so it never carries principal_id.
type Row map[string]any

// Text is the column's string value, or "" when it holds none.
func (r Row) Text(column string) string {
	value, _ := r[column].(string)
	return value
}

// Null reports whether the column is missing or null.
func (r Row) Null(column string) bool {
	value, found := r[column]
	return !found || value == nil
}

// Int is the column's integer value.
func (r Row) Int(column string) (int64, bool) {
	switch value := r[column].(type) {
	case int64:
		return value, true
	case json.Number:
		number, err := value.Int64()
		return number, err == nil
	case float64:
		if value == math.Trunc(value) && math.Abs(value) < 1<<53 {
			return int64(value), true
		}
	}
	return 0, false
}

// Float is the column's numeric value.
func (r Row) Float(column string) (float64, bool) {
	switch value := r[column].(type) {
	case float64:
		return value, true
	case int64:
		return float64(value), true
	case json.Number:
		number, err := value.Float64()
		return number, err == nil
	}
	return 0, false
}

// Bool reads a 0 or 1 column.
func (r Row) Bool(column string) bool {
	value, _ := r.Int(column)
	return value == 1
}

// Time reads a column the store wrote as an RFC 3339 instant.
func (r Row) Time(column string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, r.Text(column))
	return parsed, err == nil
}

// Clone copies the row, so a section can change what it writes without changing what it read.
func (r Row) Clone() Row { return maps.Clone(r) }

// Tables are a section's rows by table.
type Tables map[string][]Row

// Table is one table a section exports. Omit are columns that never leave the database, such as
// a secret, or a message ID that means something only in one chat.
type Table struct {
	Name string
	Omit []string
}

// Section describes what an exporter contributes: its name, which is its file's name, its label
// as the user reads it, the newest version of its records this code reads, and its tables.
type Section struct {
	Name    string
	Label   string
	Version int
	Tables  []Table
}

// Exporter is one owner's section of the file.
type Exporter interface {
	Section() Section
	// Keep reports whether one of the principal's rows belongs in the file, such as a place that
	// was not deleted.
	Keep(table string, row Row) bool
	// Plan decides, through the owner's own validation, what restoring the section's rows adds,
	// what is already there, and what is skipped and why, and queues the rows to add. version is
	// the section's version in the file, never newer than the exporter's.
	Plan(ctx context.Context, plan *Planning, version int, tables Tables) error
}

// Restorer is an exporter told once its section was restored, such as a scheduler that then looks
// for due jobs at once.
type Restorer interface {
	Restored(ctx context.Context, principalID string)
}

// Write is one row to insert for the restoring principal.
type Write struct {
	Table string
	Row   Row
}

// Limit is an owner's cap, checked in the transaction that writes a section: once its rows are in,
// the principal may hold at most Max rows of Table that match Where, an SQL condition the owner
// writes in code, with Args as its parameters. A write that would pass a cap fails the section, so
// a record created meanwhile elsewhere can never push the principal past it.
type Limit struct {
	Table string
	Where string
	Args  []any
	Max   int
}

// Store reads and writes the tables the sections export.
type Store interface {
	// ExportRows returns the principal's rows of the table without principal_id and the omitted
	// columns, in the order they were stored.
	ExportRows(ctx context.Context, table, principalID string, omit []string) ([]Row, error)
	// Columns returns the table's columns other than principal_id.
	Columns(ctx context.Context, table string) ([]string, error)
	// IDOwners reports for each id the table holds whether it is the principal's (true) or
	// another principal's (false); a free id is left out.
	IDOwners(ctx context.Context, table, principalID string, ids []string) (map[string]bool, error)
	// RestoreRows inserts the rows for the principal in one transaction, all or none, and fails
	// it when the rows would pass a limit.
	RestoreRows(ctx context.Context, principalID string, writes []Write, limits []Limit) error
}

// The kinds of record a later section refers to, which Planning.Refer and Planning.Resolve name.
const (
	RefPlace      = "place"
	RefTrip       = "trip"
	RefCollection = "collection"
	RefIntake     = "intake"
	RefProject    = "project"
	// RefRoute is a saved route, which a route watch names by its name, folded to lower case.
	RefRoute = "route"
	// RefTripReminder is a booking's check-in reminder, a job whose ID is derived from its trip's
	// and booking's.
	RefTripReminder = "trip_reminder"
)

// maximumAttempts bounds how many derived IDs are tried for one record whose ID another principal
// holds.
const maximumAttempts = 8

// Assignment is where one record of the file goes: its ID, and whether the principal already
// holds it, so nothing is added.
type Assignment struct {
	ID      string
	Present bool
}

// Planning is one section's restore plan as it is made: it reads the principal's current rows,
// settles record IDs, follows references to earlier sections, and collects what is added, what is
// already there, and what is skipped.
type Planning struct {
	principalID string
	now         time.Time
	store       Store
	known       map[string]map[string]string
	derive      func(table, id string, attempt int) string
	applying    bool

	result  SectionResult
	writes  []Write
	limits  []Limit
	guards  map[string][]string
	present map[string]map[string]string
	added   map[string]map[string]string
	dropped map[string]map[string]string
}

func newPlanning(principalID string, now time.Time, store Store, known map[string]map[string]string, derive func(string, string, int) string, section Section, applying bool) *Planning {
	return &Planning{
		principalID: principalID, now: now, store: store, known: known, derive: derive, applying: applying,
		result:  SectionResult{Section: section.Name, Label: section.Label},
		guards:  map[string][]string{},
		present: map[string]map[string]string{}, added: map[string]map[string]string{}, dropped: map[string]map[string]string{},
	}
}

// Applying reports whether the plan is about to be written rather than only shown, so work that
// matters only for a write, such as minting a secret, is left out of a preview.
func (p *Planning) Applying() bool { return p.applying }

// Limit adds an owner's cap the section's write must respect.
func (p *Planning) Limit(limit Limit) { p.limits = append(p.limits, limit) }

// Guard asks that the record of the kind, such as a saved place, be kept from being deleted while
// the section is written; the write fails when it is already gone.
func (p *Planning) Guard(kind, id string) {
	if !slices.Contains(p.guards[kind], id) {
		p.guards[kind] = append(p.guards[kind], id)
	}
}

// Drop records that the record of the kind known in the file as id is not restored, so what is
// derived from it, such as a booking's check-in reminder, is skipped too.
func (p *Planning) Drop(kind, id string) {
	kind = droppedKind(kind)
	if p.dropped[kind] == nil {
		p.dropped[kind] = map[string]string{}
	}
	p.dropped[kind][id] = ""
}

// Dropped reports whether this section or an earlier one dropped the record.
func (p *Planning) Dropped(kind, id string) bool {
	kind = droppedKind(kind)
	_, here := p.dropped[kind][id]
	_, earlier := p.known[kind][id]
	return here || earlier
}

// droppedKind is where the references keep the records of a kind that were dropped.
func droppedKind(kind string) string { return "dropped:" + kind }

// PrincipalID is the restoring principal.
func (p *Planning) PrincipalID() string { return p.principalID }

// Now is the instant the plan is made at, which past one-time records are judged against.
func (p *Planning) Now() time.Time { return p.now }

// Current returns the principal's rows of the table as they are now.
func (p *Planning) Current(ctx context.Context, table string) ([]Row, error) {
	return p.store.ExportRows(ctx, table, p.principalID, nil)
}

// Assign settles the IDs of the file's records of a table: an ID that is free is kept, one the
// principal holds is already there, and one another principal holds is replaced by an ID derived
// from it with the installation's key, so restoring again finds the same record and the plan never
// shows that another principal held the ID.
func (p *Planning) Assign(ctx context.Context, table string, ids []string) (map[string]Assignment, error) {
	assigned := make(map[string]Assignment, len(ids))
	owners, err := p.store.IDOwners(ctx, table, p.principalID, ids)
	if err != nil {
		return nil, err
	}
	var taken []string
	for _, id := range ids {
		mine, held := owners[id]
		switch {
		case !held:
			assigned[id] = Assignment{ID: id}
		case mine:
			assigned[id] = Assignment{ID: id, Present: true}
		default:
			taken = append(taken, id)
		}
	}
	for attempt := 0; len(taken) > 0; attempt++ {
		if attempt == maximumAttempts {
			return nil, fmt.Errorf("no free ID was found for a record of %s", table)
		}
		candidates := make(map[string]string, len(taken))
		list := make([]string, 0, len(taken))
		for _, id := range taken {
			candidate := p.derive(table, id, attempt)
			candidates[candidate] = id
			list = append(list, candidate)
		}
		owners, err := p.store.IDOwners(ctx, table, p.principalID, list)
		if err != nil {
			return nil, err
		}
		taken = taken[:0]
		for _, candidate := range list {
			mine, held := owners[candidate]
			switch {
			case !held:
				assigned[candidates[candidate]] = Assignment{ID: candidate}
			case mine:
				assigned[candidates[candidate]] = Assignment{ID: candidate, Present: true}
			default:
				taken = append(taken, candidates[candidate])
			}
		}
	}
	return assigned, nil
}

// Resolve follows a reference to a record of an earlier section, or of this one, by its kind,
// such as "place", to the ID it has once restored. False means the record is neither restored nor
// already there, so whatever refers to it is skipped rather than stored dangling.
func (p *Planning) Resolve(kind, id string) (string, bool) {
	for _, refs := range []map[string]map[string]string{p.added, p.present, p.known} {
		if target, found := refs[kind][id]; found {
			return target, true
		}
	}
	return "", false
}

// Refer records that a record of the kind known in the file as from is to once restored, for the
// references of later records; present marks one already there, which stays referable even when
// the section fails to restore.
func (p *Planning) Refer(kind, from, to string, present bool) {
	refs := p.added
	if present {
		refs = p.present
	}
	if refs[kind] == nil {
		refs[kind] = map[string]string{}
	}
	refs[kind][from] = to
}

// Add queues a row to insert.
func (p *Planning) Add(table string, row Row) {
	p.writes = append(p.writes, Write{Table: table, Row: row})
}

// Adding counts one record the restore adds, shown in the preview by its title.
func (p *Planning) Adding(title string) {
	p.result.Added = append(p.result.Added, Addition{Title: title})
}

// AddingText counts one record the restore adds whose text changes what the assistant does, such
// as a memory entry or a job's instruction, which the preview shows in full.
func (p *Planning) AddingText(title, text string) {
	p.result.Added = append(p.result.Added, Addition{Title: title, Text: text})
}

// Present counts one record already there.
func (p *Planning) Present() { p.result.Present++ }

// Skip lists one record that is not restored, and why.
func (p *Planning) Skip(title, reason string) {
	p.result.Skipped = append(p.result.Skipped, Skipped{Title: title, Reason: reason})
}

// SectionResult is what restoring one section adds, finds already there, and skips, or why the
// whole section is left out.
type SectionResult struct {
	Section string
	Label   string
	// Refused says why the whole section is left out, such as a version newer than this code reads.
	Refused string
	Added   []Addition
	Present int
	Skipped []Skipped
	// Failed is set when the section could not be written, which left nothing of it behind.
	Failed bool
}

// Addition is one record to add, under its title, with the full text the preview shows for one
// that changes what the assistant does.
type Addition struct {
	Title string
	Text  string
}

// Skipped is one record that is not restored, and why.
type Skipped struct {
	Title  string
	Reason string
}

// Quote shows a title in quotes, cut to one line.
func Quote(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	return `"` + title + `"`
}
