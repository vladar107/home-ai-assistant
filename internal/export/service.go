package export

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/delivery"
)

const (
	// RestoreTool is the confirmed action that restores a previewed file.
	RestoreTool = "export_restore"
	// RestoreHeading is what the confirmation of a restore is shown under.
	RestoreHeading = "Restore from your export"
	// RestoreLifetime is how long a previewed restore waits for the user's confirmation.
	RestoreLifetime = 15 * time.Minute

	fileLayout = "2006-01-02"
)

// Files sends a file to a principal's own chat.
type Files interface {
	SendFile(ctx context.Context, principalID string, file delivery.File) error
}

// Offers shows a proposal to its principal for confirmation, outside any chat turn.
type Offers interface {
	Offer(ctx context.Context, principalID string, chatID, threadID int64, heading string, lifetime time.Duration, proposals []approval.Proposal) ([]approval.Record, error)
}

// Config configures the export service. KeyPath is the export key's file in the data directory;
// Location is the zone the file's date is read in; Now is the clock, time.Now without it; Lifetime
// is how long a previewed restore waits for confirmation, RestoreLifetime without it.
type Config struct {
	KeyPath  string
	Location *time.Location
	Now      func() time.Time
	Lifetime time.Duration
}

// Service exports a principal's sections into one signed file and restores such a file.
type Service struct {
	cfg       Config
	store     Store
	files     Files
	offers    Offers
	logger    *slog.Logger
	exporters []Exporter
	guards    map[string]func(ctx context.Context, principalID string, ids []string, store func() error) error

	mu sync.Mutex
	// key is the export key once read or created.
	key []byte
	// held is each principal's one previewed file while its restore waits for confirmation; a
	// newer preview replaces it, and it is dropped when its confirmation can no longer come.
	held map[string]heldFile
	// restoring serializes restores, so two confirmations never plan against each other's writes.
	restoring sync.Mutex
}

type heldFile struct {
	digest string
	data   []byte
	// additions is what the preview the user confirms said the restore adds.
	additions string
	expires   time.Time
}

var (
	_ approval.Executor = (*Service)(nil)
)

func NewService(cfg Config, store Store, files Files, offers Offers, logger *slog.Logger) (*Service, error) {
	switch {
	case cfg.KeyPath == "":
		return nil, errors.New("export key path is required")
	case store == nil || files == nil || offers == nil:
		return nil, errors.New("export store, files, and offers are required")
	case logger == nil:
		return nil, errors.New("export logger is required")
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Lifetime <= 0 {
		cfg.Lifetime = RestoreLifetime
	}
	return &Service{
		cfg: cfg, store: store, files: files, offers: offers, logger: logger, held: make(map[string]heldFile),
		guards: make(map[string]func(context.Context, string, []string, func() error) error),
	}, nil
}

// Register adds a section. Sections restore in the order they are registered, so a section is
// registered after every section whose records it refers to.
func (s *Service) Register(exporter Exporter) error {
	if exporter == nil {
		return errors.New("export section is nil")
	}
	section := exporter.Section()
	if section.Name == "" || section.Label == "" || section.Version < 1 || len(section.Tables) == 0 {
		return fmt.Errorf("export section %q is incomplete", section.Name)
	}
	if !entryName.MatchString(section.Name+".json") || section.Name+".json" == manifestName {
		return fmt.Errorf("export section %q has no valid name", section.Name)
	}
	for _, registered := range s.exporters {
		if registered.Section().Name == section.Name {
			return fmt.Errorf("export section %q is registered twice", section.Name)
		}
		for _, table := range registered.Section().Tables {
			for _, added := range section.Tables {
				if table.Name == added.Name {
					return fmt.Errorf("table %s is exported by two sections", table.Name)
				}
			}
		}
	}
	s.exporters = append(s.exporters, exporter)
	return nil
}

// Sections describes the registered sections in restore order.
func (s *Service) Sections() []Section {
	sections := make([]Section, 0, len(s.exporters))
	for _, exporter := range s.exporters {
		sections = append(sections, exporter.Section())
	}
	return sections
}

func (s *Service) loadKey(create bool) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		return s.key, nil
	}
	key, err := loadKey(s.cfg.KeyPath, create)
	if err != nil {
		return nil, err
	}
	s.key = key
	return key, nil
}

// File is an export ready to send.
type File struct {
	Name string
	Data []byte
}

// Export builds the principal's file, creating the installation's export key on the first export.
func (s *Service) Export(ctx context.Context, principalID string) (File, error) {
	key, err := s.loadKey(true)
	if err != nil {
		return File{}, err
	}
	at := s.cfg.Now()
	sections := make([]sectionFile, 0, len(s.exporters))
	for _, exporter := range s.exporters {
		section := exporter.Section()
		tables := make(Tables, len(section.Tables))
		for _, table := range section.Tables {
			rows, err := s.store.ExportRows(ctx, table.Name, principalID, table.Omit)
			if err != nil {
				return File{}, fmt.Errorf("export %s: %w", table.Name, err)
			}
			kept := make([]Row, 0, len(rows))
			for _, row := range rows {
				if exporter.Keep(table.Name, row) {
					kept = append(kept, row)
				}
			}
			tables[table.Name] = kept
		}
		sections = append(sections, sectionFile{Section: section.Name, Version: section.Version, Tables: tables})
	}
	data, err := writeArchive(key, principalID, at, sections)
	if err != nil {
		return File{}, err
	}
	s.logger.InfoContext(ctx, "export.created", "principal_id", principalID, "sections", len(sections), "size_bytes", len(data))
	return File{Name: "home-assistant-export-" + at.In(s.cfg.Location).Format(fileLayout) + ".zip", Data: data}, nil
}

// SendExport builds the principal's file and sends it to their own chat as a document.
func (s *Service) SendExport(ctx context.Context, principalID string) error {
	file, err := s.Export(ctx, principalID)
	if err != nil {
		return err
	}
	return s.files.SendFile(ctx, principalID, delivery.File{Name: file.Name, Size: int64(len(file.Data)), Body: io.NopCloser(bytes.NewReader(file.Data))})
}

// open reads and verifies a file for the principal and returns it with the key that verified it; a
// file that is not the principal's own, as this installation exported it, is a *Refusal.
func (s *Service) open(principalID string, data []byte) (*archive, []byte, error) {
	key, err := s.loadKey(false)
	if errors.Is(err, errNoKey) {
		// Without a key nothing here was ever exported, so the file cannot be from here; the limits
		// are still checked first, as for any other file, and no signature verifies without a key.
		_, err := readArchive(data, nil, principalID)
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, err
	}
	a, err := readArchive(data, key, principalID)
	return a, key, err
}

// derive is the ID a record of the table takes when another principal holds its own: keyed with
// the installation's export key, so restoring the same file again finds it, and nobody without the
// key can tell which ID it will be.
func derive(key []byte, principalID string) func(table, id string, attempt int) string {
	return func(table, id string, attempt int) string {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("export-id\x00" + principalID + "\x00" + table + "\x00" + id + "\x00" + strconv.Itoa(attempt)))
		length := min(max(len(id), 8), 64)
		return hex.EncodeToString(mac.Sum(nil))[:length]
	}
}

// Report is what restoring a file adds, finds already there, and skips, section by section.
type Report struct {
	ExportedAt time.Time
	Sections   []SectionResult
}

// Additions is how many records the restore adds.
func (r Report) Additions() int {
	total := 0
	for _, section := range r.Sections {
		if section.Refused == "" && !section.Failed {
			total += len(section.Added)
		}
	}
	return total
}

// run plans every section of the file in restore order and, with apply, restores each in its own
// transaction right after planning it. A section that fails leaves nothing behind, and the
// sections after it see its records as missing, so nothing refers to them dangling.
func (s *Service) run(ctx context.Context, principalID string, a *archive, key []byte, apply bool) (Report, error) {
	report := Report{ExportedAt: a.manifest.ExportedAt}
	known := map[string]map[string]string{}
	derive := derive(key, principalID)
	now := s.cfg.Now()
	seen := map[string]bool{}
	for _, exporter := range s.exporters {
		section := exporter.Section()
		file, found := a.sections[section.Name]
		if !found {
			continue
		}
		seen[section.Name] = true
		if file.Version > section.Version {
			report.Sections = append(report.Sections, SectionResult{Section: section.Name, Label: section.Label, Refused: "made by a newer version of the assistant"})
			continue
		}
		planning := newPlanning(principalID, now, s.store, known, derive, section, apply)
		tables, err := s.readable(ctx, planning, section, file.Tables)
		if err == nil {
			err = exporter.Plan(ctx, planning, file.Version, tables)
		}
		if err != nil {
			if !apply {
				return Report{}, fmt.Errorf("plan restoring %s: %w", section.Name, err)
			}
			s.logger.WarnContext(ctx, "export.restore.section_failed", "principal_id", principalID, "section", section.Name, "stage", "plan", "error", err)
			report.Sections = append(report.Sections, SectionResult{Section: section.Name, Label: section.Label, Failed: true})
			continue
		}
		result := planning.result
		if apply && len(planning.writes) > 0 {
			if err := s.write(ctx, principalID, planning); err != nil {
				s.logger.WarnContext(ctx, "export.restore.section_failed", "principal_id", principalID, "section", section.Name, "stage", "write", "error", err)
				result.Failed = true
			} else if restorer, ok := exporter.(Restorer); ok {
				restorer.Restored(ctx, principalID)
			}
		}
		merge(known, planning.present)
		merge(known, planning.dropped)
		if !result.Failed {
			merge(known, planning.added)
		}
		report.Sections = append(report.Sections, result)
	}
	for _, name := range a.sectionNames() {
		if !seen[name] {
			report.Sections = append(report.Sections, SectionResult{Section: name, Label: name, Refused: "not known to this version of the assistant"})
		}
	}
	return report, nil
}

// readable leaves out, and lists as skipped, each record of the section's tables that holds a
// column the table does not have or a value no column can hold, so one such record never fails
// the whole section.
func (s *Service) readable(ctx context.Context, plan *Planning, section Section, tables Tables) (Tables, error) {
	result := make(Tables, len(tables))
	for table, rows := range tables {
		if !slices.ContainsFunc(section.Tables, func(known Table) bool { return known.Name == table }) {
			plan.Skip(fmt.Sprintf("%d records of %s", len(rows), table), "this version keeps no such records")
			continue
		}
		columns, err := s.store.Columns(ctx, table)
		if err != nil {
			return nil, err
		}
		kept := make([]Row, 0, len(rows))
		for _, row := range rows {
			fits := true
			for column, value := range row {
				switch value.(type) {
				case nil, string, json.Number, int64, float64:
				default:
					fits = false
				}
				if !slices.Contains(columns, column) {
					fits = false
				}
			}
			if !fits {
				plan.Skip("a record of "+section.Label, "it holds a field this version does not know")
				continue
			}
			kept = append(kept, row)
		}
		result[table] = kept
	}
	return result, nil
}

// write stores a section's rows in one transaction within its limits, under the guard of each
// record its rows refer to that must not be deleted meanwhile.
func (s *Service) write(ctx context.Context, principalID string, plan *Planning) error {
	store := func() error { return s.store.RestoreRows(ctx, principalID, plan.writes, plan.limits) }
	kinds := make([]string, 0, len(plan.guards))
	for kind := range plan.guards {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		guard, found := s.guards[kind]
		if !found {
			continue
		}
		ids, inner := plan.guards[kind], store
		store = func() error { return guard(ctx, principalID, ids, inner) }
	}
	return store()
}

// SetGuard keeps records of a kind, such as saved places, from being deleted while a section that
// refers to them is written; guard runs store while the principal's records with the IDs cannot
// be deleted, and fails without running it when one is gone. It is set at wiring time.
func (s *Service) SetGuard(kind string, guard func(ctx context.Context, principalID string, ids []string, store func() error) error) {
	s.guards[kind] = guard
}

func merge(into, from map[string]map[string]string) {
	for kind, refs := range from {
		if into[kind] == nil {
			into[kind] = map[string]string{}
		}
		for old, id := range refs {
			into[kind][old] = id
		}
	}
}

// Preview is what restoring a file would do, as the user reads it before confirming.
type Preview struct {
	// Pages are the preview's messages, in order, to be sent as plain text.
	Pages []string
	// Summary is the confirmation's text.
	Summary   string
	Additions int
	digest    string
	additions string
	data      []byte
}

// PreviewRestore verifies the principal's file and plans restoring it without changing anything.
// A file that is not restored at all is a *Refusal, whose text tells the user why.
func (s *Service) PreviewRestore(ctx context.Context, principalID string, data []byte) (Preview, error) {
	a, key, err := s.open(principalID, data)
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			s.logger.InfoContext(ctx, "export.restore.refused", "principal_id", principalID, "reason", refusal.Reason)
		}
		return Preview{}, err
	}
	s.restoring.Lock()
	report, err := s.run(ctx, principalID, a, key, false)
	s.restoring.Unlock()
	if err != nil {
		return Preview{}, err
	}
	s.logger.InfoContext(ctx, "export.restore.previewed", "principal_id", principalID, "additions", report.Additions())
	return Preview{
		Pages: pages(s.previewText(report)), Summary: s.summaryText(report), Additions: report.Additions(),
		digest: digest(data), additions: report.fingerprint(), data: data,
	}, nil
}

// fingerprint names exactly what the report adds, so a confirmation can tell that it still adds
// what the user was shown.
func (r Report) fingerprint() string {
	hash := sha256.New()
	for _, section := range r.Sections {
		if section.Refused != "" || section.Failed {
			continue
		}
		for _, added := range section.Added {
			fmt.Fprintf(hash, "%q %q %q\n", section.Section, added.Title, added.Text)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type restoreArguments struct {
	Digest string `json:"digest"`
}

// OfferRestore holds the previewed file and asks the principal to confirm restoring it, in the
// chat and tab given. The file is held in memory only, one per principal, until its confirmation
// can no longer come.
func (s *Service) OfferRestore(ctx context.Context, principalID string, chatID, threadID int64, preview Preview) error {
	if preview.Additions == 0 || preview.digest == "" {
		return errors.New("the preview adds nothing to confirm")
	}
	arguments, err := json.Marshal(restoreArguments{Digest: preview.digest})
	if err != nil {
		return err
	}
	held := heldFile{digest: preview.digest, data: preview.data, additions: preview.additions, expires: s.cfg.Now().Add(s.cfg.Lifetime + time.Minute)}
	s.mu.Lock()
	s.held[principalID] = held
	s.mu.Unlock()
	time.AfterFunc(s.cfg.Lifetime+time.Minute, func() { s.release(principalID, held.digest) })
	records, err := s.offers.Offer(ctx, principalID, chatID, threadID, RestoreHeading, s.cfg.Lifetime, []approval.Proposal{{
		Tool: RestoreTool, Arguments: arguments, Summary: preview.Summary,
		Intent: approval.Intent{Verb: "restore", TargetKind: "export"},
	}})
	if records == nil {
		s.release(principalID, held.digest)
		return err
	}
	if err != nil {
		// The confirmation is stored, and the approval sweep sends it until it expires.
		s.logger.WarnContext(ctx, "export.restore.offer_undelivered", "principal_id", principalID, "error", err)
	}
	return nil
}

// release forgets the principal's held file when it is still the one with the digest.
func (s *Service) release(principalID, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, found := s.held[principalID]; found && held.digest == digest {
		delete(s.held, principalID)
	}
}

func (s *Service) ActionTools() []string { return []string{RestoreTool} }

// ExecuteApproved restores the file the principal confirmed, as long as it is still held: it is
// kept in memory only, so a restart forgets it and the user sends it again. A file that would no
// longer add exactly what the preview showed is not restored.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	if record.Tool != RestoreTool {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That action is no longer available."}
	}
	var arguments restoreArguments
	if err := json.Unmarshal(record.Arguments, &arguments); err != nil || arguments.Digest == "" {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed restore could not be read; nothing was restored."}
	}
	s.mu.Lock()
	held, found := s.held[record.PrincipalID]
	s.mu.Unlock()
	if !found || held.digest != arguments.Digest || !s.cfg.Now().Before(held.expires) {
		return approval.Outcome{}, &approval.ActionError{Reason: "gone", Text: RestoreGoneText}
	}
	s.release(record.PrincipalID, held.digest)
	report, err := s.restore(ctx, record.PrincipalID, held.data, held.additions)
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return approval.Outcome{}, &approval.ActionError{Reason: refusal.Reason, Text: refusal.Text}
		}
		return approval.Outcome{}, err
	}
	return approval.Outcome{Text: restoredText(report)}, nil
}

// RestoreGoneText is the outcome of a confirmation whose file is no longer held.
const RestoreGoneText = "I no longer hold that file, so nothing was restored; send it again with /restore."

// ErrChanged refuses a confirmed restore that would no longer add what its preview showed.
var ErrChanged = &Refusal{"changed", "What this file would add changed since you saw the preview, so nothing was restored; send it again with /restore to see what it adds now."}

// Restore verifies the principal's file and restores it, section by section.
func (s *Service) Restore(ctx context.Context, principalID string, data []byte) (Report, error) {
	return s.restore(ctx, principalID, data, "")
}

// restore is Restore that, given the additions a preview showed, first checks that the file still
// adds exactly those.
func (s *Service) restore(ctx context.Context, principalID string, data []byte, additions string) (Report, error) {
	a, key, err := s.open(principalID, data)
	if err != nil {
		return Report{}, err
	}
	s.restoring.Lock()
	defer s.restoring.Unlock()
	if additions != "" {
		planned, err := s.run(ctx, principalID, a, key, false)
		if err != nil {
			return Report{}, err
		}
		if planned.fingerprint() != additions {
			s.logger.InfoContext(ctx, "export.restore.refused", "principal_id", principalID, "reason", ErrChanged.Reason)
			return Report{}, ErrChanged
		}
	}
	report, err := s.run(ctx, principalID, a, key, true)
	if err != nil {
		return Report{}, err
	}
	failed := 0
	for _, section := range report.Sections {
		if section.Failed {
			failed++
		}
	}
	s.logger.InfoContext(ctx, "export.restore.done", "principal_id", principalID, "additions", report.Additions(), "failed_sections", failed)
	return report, nil
}
