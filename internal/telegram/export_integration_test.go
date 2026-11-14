package telegram

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	"github.com/vladar107/home-ai-assistant/internal/schedule"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/trip"
	"github.com/vladar107/home-ai-assistant/internal/watch"
	"github.com/vladar107/home-ai-assistant/internal/wiring"
)

// The records one principal keeps in every section of the export, seeded straight into the
// database so each test can export them, change the file, and restore it elsewhere.
const (
	exportPlace       = "e0a1b2c3d4e5f601"
	exportRoute       = "e0a1b2c3d4e5f602"
	exportTrip        = "e0a1b2c3"
	exportBooking     = "e0a1b2c4"
	exportCollection  = "e0a1b2c3d4e5f603"
	exportRecord      = "e0a1b2c3d4e5f604"
	exportIntake      = "e0a1b2c3d4e5f605"
	exportProject     = "e0a1b2c3d4e5f606"
	exportItem        = "e0a1b2c5"
	exportFact        = "e0a1b2c6"
	exportInstruction = "e0a1b2c7"
	exportProcedure   = "e0a1b2c8"
	exportSource      = "e0a1b2c3d4e5f607"
	exportCronJob     = "e0a1b2c3d4e5f608"
	exportReminderJob = "e0a1b2c3d4e5f609"
	exportTripJob     = "e0a1b2c3d4e5f60a"
	exportPlaceJob    = "e0a1b2c3d4e5f60b"
	exportAgentWatch  = "e0a1b2c3d4e5f60c"
	exportPlaceWatch  = "e0a1b2c3d4e5f60d"
	exportTokenHash   = "1111111111111111111111111111111111111111111111111111111111111111"
	// exportWatchSummary is the message pinned in the exported agent watch's tab.
	exportWatchSummary = 4242

	exportFactText        = "Prefers tea in the morning."
	exportInstructionText = "Answer briefly."
	exportSteps           = "Find the restaurant\nCall them to book"
	exportPrompt          = "Summarize the morning news for me."
	exportAgentCheck      = "Look up the price of the blue kettle at the hardware store."
	exportAgentCondition  = "it costs less than 30 euros"
)

const storedTime = "2006-01-02T15:04:05.000Z"

func stamp(at time.Time) string { return at.UTC().Format(storedTime) }

// seedExport stores one record of every section for the principal.
func (h *conversationHarness) seedExport(principal string) {
	h.t.Helper()
	now := time.Now().UTC().Truncate(time.Minute)
	created := stamp(now.Add(-time.Hour))
	tripStart := now.Add(10 * 24 * time.Hour)
	checkIn := tripStart.Add(-24 * time.Hour)
	remindAt := now.Add(48 * time.Hour)
	tripJobAt := tripStart.Add(-time.Hour)
	tripAnchor := schedule.Anchor{Kind: schedule.AnchorTrip, TripID: exportTrip, Edge: schedule.EdgeStart, OffsetMinutes: -60, Label: "Lisbon"}
	tripJobConfig, err := schedule.JoinConfig(json.RawMessage(`{"text":"Pack the suitcase"}`), &tripAnchor, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	placeAnchor := schedule.Anchor{Kind: schedule.AnchorPlace, PlaceID: exportPlace, Edge: schedule.EdgeArrive, Label: "Gym"}
	placeJobConfig, err := schedule.JoinConfig(json.RawMessage(`{"text":"Stretch first"}`), &placeAnchor, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	agent, err := json.Marshal(watch.Spec{
		Kind: watch.KindAgent, Target: watch.Target{Name: "Kettle price"},
		Check:    &watch.Check{Instruction: exportAgentCheck, Condition: exportAgentCondition, IntervalMinutes: 60},
		Triggers: []string{watch.TriggerEnter}, Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, Start: "00:00", End: "24:00", Zone: "Europe/Amsterdam",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	arrival, err := json.Marshal(watch.Spec{
		Kind: watch.KindPlace, Target: watch.Target{Name: "Gym", Place: exportPlace},
		Triggers: []string{watch.TriggerArrive}, Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, Start: "00:00", End: "24:00", Zone: "Europe/Amsterdam",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO places (id, principal_id, name, name_key, latitude, longitude, radius_m, created_at, updated_at) VALUES (?, ?, 'Gym', 'gym', 52.25, 4.75, 150, ?, ?)`,
			[]any{exportPlace, principal, created, created}},
		{`INSERT INTO routes (id, principal_id, name, days, arrive_by, legs, created_at, updated_at) VALUES (?, ?, 'To work', '["mon","tue"]', '09:00', ?, ?, ?)`,
			[]any{exportRoute, principal, `[{"mode":"walk","minutes":5},{"mode":"train","from":"Central","to":"Harbour"}]`, created, created}},
		{`INSERT INTO trips (id, principal_id, title, destination, starts_at, ends_at, zone, status, source, created_at, updated_at) VALUES (?, ?, 'Lisbon', 'Lisbon', ?, ?, 'Europe/Lisbon', 'planned', 'chat', ?, ?)`,
			[]any{exportTrip, principal, stamp(tripStart), stamp(tripStart.Add(72 * time.Hour)), created, created}},
		{`INSERT INTO trip_bookings (id, principal_id, trip_id, kind, title, reference, starts_at, check_in_at, check_in_source, fingerprint, position) VALUES (?, ?, ?, 'hotel', 'Hotel', 'ABC123', ?, ?, 'stated', '', 0)`,
			[]any{exportBooking, principal, exportTrip, stamp(tripStart), stamp(checkIn)}},
		{`INSERT INTO collections (id, principal_id, name, description, fields, next_key, created_at, updated_at) VALUES (?, ?, 'Lent items', 'Things lent out', ?, 2, ?, ?)`,
			[]any{exportCollection, principal, `[{"key":"f1","name":"Item","type":"text","required":true}]`, created, created}},
		{`INSERT INTO collection_records (id, principal_id, collection_id, field_values, created_at, updated_at) VALUES (?, ?, ?, '{"f1":"Ladder"}', ?, ?)`,
			[]any{exportRecord, principal, exportCollection, created, created}},
		{`INSERT INTO intake_records (id, principal_id, source, source_ref, kind, title, sender, facts, action_required, created_at, updated_at) VALUES (?, ?, 'file', 'att-1', 'invoice', 'Water bill', 'Water company', ?, 'Pay by the due date', ?, ?)`,
			[]any{exportIntake, principal, `{"dates":[{"label":"Due","date":"2026-12-01"}],"amounts":[],"references":[]}`, created, created}},
		{`INSERT INTO projects (id, principal_id, title, goal, status, created_at, updated_at) VALUES (?, ?, 'Find a dentist', 'Book a checkup', 'active', ?, ?)`,
			[]any{exportProject, principal, created, created}},
		{`INSERT INTO project_items (principal_id, project_id, id, position, text, due_on, note) VALUES (?, ?, ?, 0, 'Ask the neighbours', '2026-12-01', '')`,
			[]any{principal, exportProject, exportItem}},
		{`INSERT INTO project_links (principal_id, project_id, position, kind, reference, label) VALUES (?, ?, 0, 'collection', ?, 'Lent'), (?, ?, 1, 'intake', ?, '')`,
			[]any{principal, exportProject, exportCollection, principal, exportProject, exportIntake}},
		{`INSERT INTO memory_entries (id, principal_id, kind, text, source, created_at, updated_at) VALUES (?, ?, 'fact', ?, 'confirmed', ?, ?), (?, ?, 'instruction', ?, 'model', ?, ?)`,
			[]any{exportFact, principal, exportFactText, created, created, exportInstruction, principal, exportInstructionText, created, created}},
		{`INSERT INTO memory_procedures (id, principal_id, title, steps, source, created_at, updated_at) VALUES (?, ?, 'Book a table', ?, 'confirmed', ?, ?)`,
			[]any{exportProcedure, principal, exportSteps, created, created}},
		{`INSERT INTO memory_settings (principal_id, enabled, learn_from_feedback, learn_procedures) VALUES (?, 1, 0, 1)`, []any{principal}},
		{`INSERT INTO principal_languages (principal_id, language, changed_at) VALUES (?, 'ru', ?)`, []any{principal, created}},
		{`INSERT INTO integration_switches (principal_id, integration, enabled, changed_at) VALUES (?, 'web', 0, ?)`, []any{principal, created}},
		{`INSERT INTO event_sources (id, principal_id, name, mode, token_hash, enabled, created_at) VALUES (?, ?, 'Backup server', 'assess', ?, 1, ?)`,
			[]any{exportSource, principal, exportTokenHash, created}},
		{`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, cron, zone, config, enabled, next_run_at, created_at, updated_at, destination) VALUES (?, ?, 'Morning news', 'model.turn', '0 8 * * *', 'Europe/Amsterdam', ?, 1, ?, ?, ?, ?)`,
			[]any{exportCronJob, principal, `{"prompt":"` + exportPrompt + `"}`, stamp(now.Add(24 * time.Hour)), created, created, thread.ProjectDestination(exportProject)}},
		{`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, run_at, zone, config, enabled, next_run_at, created_at, updated_at) VALUES (?, ?, 'Plants', 'reminder.text', ?, 'Europe/Amsterdam', '{"text":"Water the plants"}', 0, ?, ?, ?)`,
			[]any{exportReminderJob, principal, stamp(remindAt), stamp(remindAt), created, created}},
		{`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, run_at, zone, config, enabled, next_run_at, created_at, updated_at, destination) VALUES (?, ?, 'Pack', 'reminder.text', ?, 'Europe/Lisbon', ?, 1, ?, ?, ?, ?)`,
			[]any{exportTripJob, principal, stamp(tripJobAt), string(tripJobConfig), stamp(tripJobAt), created, created, thread.TripDestination(exportTrip)}},
		{`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, run_at, zone, config, enabled, next_run_at, created_at, updated_at, destination) VALUES (?, ?, 'Check in: Hotel', 'reminder.text', ?, 'Europe/Lisbon', '{"text":"Check in for Hotel"}', 1, ?, ?, ?, ?)`,
			[]any{trip.ReminderID(exportTrip, exportBooking), principal, stamp(checkIn), stamp(checkIn), created, created, thread.TripDestination(exportTrip)}},
		{`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, anchor, zone, config, enabled, next_run_at, created_at, updated_at) VALUES (?, ?, 'Stretch', 'reminder.text', 'repeating', 'Europe/Amsterdam', ?, 1, ?, ?, ?)`,
			[]any{exportPlaceJob, principal, string(placeJobConfig), stamp(now.AddDate(100, 0, 0)), created, created}},
		{`INSERT INTO watches (id, principal_id, kind, config, enabled, created_at, updated_at, travel_pause) VALUES (?, ?, 'agent', ?, 1, ?, ?, 1), (?, ?, 'place', ?, 0, ?, ?, 0)`,
			[]any{exportAgentWatch, principal, string(agent), created, created, exportPlaceWatch, principal, string(arrival), created, created}},
		{`UPDATE watches SET summary_message_id = ? WHERE id = ?`, []any{exportWatchSummary, exportAgentWatch}},
	} {
		if _, err := h.db.ExecContext(h.ctx, statement.query, statement.args...); err != nil {
			h.t.Fatalf("%s: %v", statement.query, err)
		}
	}
}

// exportKeyPath is the harness's export key in its data directory.
func (h *conversationHarness) exportKeyPath() string {
	return filepath.Join(filepath.Dir(h.attachmentsDir), export.KeyFileName)
}

// takeExport sends /export and returns the file that follows the reply that describes it.
func (h *conversationHarness) takeExport(updateID, userID int64) []byte {
	h.t.Helper()
	h.push(textUpdate(updateID, userID, "/export"))
	if reply := h.nextMessage(); reply.text != localetest.English(ExportReply) || reply.replyTo != requestMessageID(updateID) {
		h.t.Fatalf("export reply = %+v", reply)
	}
	document := h.nextDocument()
	if document.chatID != userID || !regexp.MustCompile(`^home-assistant-export-\d{4}-\d{2}-\d{2}\.zip$`).MatchString(document.name) {
		h.t.Fatalf("export document = %s to %d", document.name, document.chatID)
	}
	return document.data
}

// restorePreview sends the file with /restore as its caption and returns the preview, its pages
// joined.
func (h *conversationHarness) restorePreview(updateID, userID int64, data []byte) string {
	h.t.Helper()
	fileID := fmt.Sprintf("export-%d", updateID)
	h.addFile(fileID, "documents/"+fileID+".zip", data)
	h.push(documentUpdate(updateID, userID, "/restore", MediaFile{FileID: fileID, FileName: "export.zip", MimeType: "application/zip", FileSize: int64(len(data))}))
	first := h.nextMessage()
	pages := []string{first.text}
	if match := regexp.MustCompile(`^\(1/(\d+)\) `).FindStringSubmatch(first.text); match != nil {
		var count int
		fmt.Sscan(match[1], &count)
		for range count - 1 {
			pages = append(pages, h.nextMessage().text)
		}
	}
	return strings.Join(pages, "\n")
}

// restore previews the file, confirms the restore, and returns the preview and the outcome.
func (h *conversationHarness) restore(updateID, userID int64, data []byte) (string, string) {
	h.t.Helper()
	preview := h.restorePreview(updateID, userID, data)
	card := h.nextMessage()
	if !strings.HasPrefix(card.text, localetest.English(export.RestoreHeading)) {
		h.t.Fatalf("restore card = %+v after preview %q", card, preview)
	}
	confirm, _ := approvalButtons(h.t, card.keyboard)
	h.push(callbackUpdate(updateID+1, userID, card.messageID, confirm))
	h.wantAnswer(updateID + 1)
	return preview, h.nextEdit().text
}

// principalRows is every row of the principal's in the table, in the order they were stored, without
// the columns given.
func (h *conversationHarness) principalRows(table, principal string, without ...string) []map[string]any {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.ctx, `SELECT * FROM `+table+` WHERE principal_id = ? ORDER BY rowid`, principal)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		h.t.Fatal(err)
	}
	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			h.t.Fatal(err)
		}
		row := map[string]any{}
		for index, column := range columns {
			if !slices.Contains(without, column) {
				row[column] = values[index]
			}
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *conversationHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.ExecContext(h.ctx, query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func copyKey(t *testing.T, from, to *conversationHarness) {
	t.Helper()
	key, err := os.ReadFile(from.exportKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to.exportKeyPath(), key, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newExportHarness(t *testing.T) *conversationHarness {
	t.Helper()
	h := newConversationHarnessWithOptions(t, newControlledRuntime(), harnessOptions{watches: &stubEvaluator{}, locations: true})
	h.provision("alice", "acct-alice")
	h.start()
	return h
}

// zipEntry is one part of a file a test builds.
type zipEntry struct {
	name string
	body []byte
}

func readZip(t *testing.T, data []byte) []zipEntry {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var entries []zipEntry
	for _, file := range reader.File {
		part, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, zipEntry{file.Name, body})
	}
	return entries
}

func writeZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		part, err := writer.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// resign changes an export with edit, which receives the manifest and each section's file decoded,
// and signs the result with the key as the installation would.
func resign(t *testing.T, key, data []byte, edit func(manifest map[string]any, sections map[string]map[string]any)) []byte {
	t.Helper()
	entries := readZip(t, data)
	var signed struct {
		Manifest  json.RawMessage `json:"manifest"`
		Signature string          `json:"signature"`
	}
	sections := map[string]map[string]any{}
	for _, entry := range entries {
		if entry.name == "manifest.json" {
			if err := json.Unmarshal(entry.body, &signed); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var section map[string]any
		if err := json.Unmarshal(entry.body, &section); err != nil {
			t.Fatal(err)
		}
		sections[entry.name] = section
	}
	var manifest map[string]any
	if err := json.Unmarshal(signed.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	edit(manifest, sections)
	rebuilt := []zipEntry{{name: "manifest.json"}}
	for _, item := range manifest["sections"].([]any) {
		entry := item.(map[string]any)
		name := entry["file"].(string)
		body, err := json.Marshal(sections[name])
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		entry["sha256"] = hex.EncodeToString(sum[:])
		rebuilt = append(rebuilt, zipEntry{name, body})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("home-assistant-export\x00"))
	mac.Write(raw)
	body, err := json.Marshal(map[string]any{"manifest": json.RawMessage(raw), "signature": hex.EncodeToString(mac.Sum(nil))})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt[0].body = body
	return writeZip(t, rebuilt)
}

// sectionRows are a decoded section's rows of one table.
func sectionRows(section map[string]any, table string) []any {
	rows, _ := section["tables"].(map[string]any)[table].([]any)
	return rows
}

func setSectionRows(section map[string]any, table string, rows []any) {
	section["tables"].(map[string]any)[table] = rows
}

// Everything the user keeps comes back field by field in an empty database holding the same key,
// with every behaviour-changing text shown in full first; restoring again adds nothing, and no
// secret, credential, or other user's row is ever in the file.
func TestExportRestoresEverySectionIntoAnEmptyDatabase(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	source.exec(`INSERT INTO memory_entries (id, principal_id, kind, text, source, created_at, updated_at) VALUES ('b0b0b0b0', 'bob', 'fact', 'Bob keeps bees.', 'confirmed', ?, ?)`, stamp(time.Now()), stamp(time.Now()))
	if _, err := os.Stat(source.exportKeyPath()); !os.IsNotExist(err) {
		t.Fatalf("export key before the first export: %v", err)
	}
	data := source.takeExport(1, 101)
	info, err := os.Stat(source.exportKeyPath())
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("export key = %+v, %v", info, err)
	}
	for _, secret := range []string{exportTokenHash, "token_hash", "acct-alice", "Bob keeps bees", "summary_message_id", "pending_actions", "last_used_at"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("the compressed export holds %q", secret)
		}
		for _, entry := range readZip(t, data) {
			if bytes.Contains(entry.body, []byte(secret)) {
				t.Fatalf("%s holds %q", entry.name, secret)
			}
		}
	}
	for _, entry := range readZip(t, data) {
		for table := range wiring.ExcludedFromExport {
			if bytes.Contains(entry.body, []byte(`"`+table+`"`)) {
				t.Fatalf("%s holds the excluded table %s", entry.name, table)
			}
		}
	}

	target := newExportHarness(t)
	copyKey(t, source, target)
	preview, outcome := target.restore(1, 101, data)
	for _, want := range []string{exportFactText, exportInstructionText, exportSteps, exportPrompt, "Water the plants", "Pack the suitcase", exportAgentCheck, exportAgentCondition, `"Lisbon"`, `"Gym"`, `"Lent items"`, `"Find a dentist"`, `"Water bill"`, `"To work"`, `"Backup server"`} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview lacks %q:\n%s", want, preview)
		}
	}
	if strings.Contains(preview, "Skipped") || !strings.Contains(outcome, "Restored") || strings.Contains(outcome, "Could not") {
		t.Fatalf("preview:\n%s\noutcome: %s", preview, outcome)
	}
	for _, section := range target.assembly.Exports.Sections() {
		for _, table := range section.Tables {
			without := slices.Clone(table.Omit)
			if table.Name == "scheduled_jobs" {
				// A recurring job runs from its next occurrence after the restore.
				without = append(without, "next_run_at")
			}
			want, got := source.principalRows(table.Name, "alice", without...), target.principalRows(table.Name, "alice", without...)
			if len(want) == 0 || !equalRows(want, got) {
				t.Fatalf("%s:\nexported %v\nrestored %v", table.Name, want, got)
			}
		}
	}
	if hash := target.principalRows("event_sources", "alice")[0]["token_hash"]; hash == exportTokenHash || len(hash.(string)) != 64 {
		t.Fatalf("restored token hash = %v", hash)
	}
	if target.count(`SELECT count(*) FROM memory_entries WHERE principal_id = 'bob'`) != 0 {
		t.Fatal("another user's entry was restored")
	}
	if source.count(`SELECT count(*) FROM watches WHERE summary_message_id = ?`, exportWatchSummary) != 1 || target.count(`SELECT count(*) FROM watches WHERE summary_message_id IS NOT NULL`) != 0 {
		t.Fatal("a watch's summary message was exported, or the exported watch had none")
	}

	again := target.restorePreview(3, 101, data)
	if !strings.Contains(again, "Everything in this file is already here") || strings.Contains(again, "+ ") {
		t.Fatalf("second preview:\n%s", again)
	}
	if same := source.restorePreview(2, 101, data); !strings.Contains(same, "Everything in this file is already here") {
		t.Fatalf("preview in the database it came from:\n%s", same)
	}
	if source.documentCount() != 0 || len(source.messages) != 0 || len(target.messages) != 0 {
		t.Fatal("a restore that adds nothing asked for confirmation")
	}
}

// An export made before the Widgets switch carries the proposal widget choice under its old name:
// it is restored as the Widgets switch, and leaves a widgets choice the user already made as it is.
func TestRestoreReadsTheProposalWidgetSwitchAsWidgets(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.exec(`INSERT INTO integration_switches (principal_id, integration, enabled, changed_at) VALUES ('alice', 'proposal-widget', 1, ?)`, stamp(time.Now().Add(-time.Hour)))
	data := source.takeExport(1, 101)

	target := newExportHarness(t)
	copyKey(t, source, target)
	preview, outcome := target.restore(1, 101, data)
	if !strings.Contains(preview, "widgets on") || strings.Contains(preview, "proposal-widget") || !strings.Contains(outcome, "Restored") {
		t.Fatalf("preview:\n%s\noutcome: %s", preview, outcome)
	}
	if enabled, err := target.switches.Enabled(target.ctx, "alice", integration.WidgetsSwitch); err != nil || !enabled {
		t.Fatalf("widgets after the restore = %v %v", enabled, err)
	}
	if count := target.count(`SELECT count(*) FROM integration_switches WHERE integration = 'proposal-widget'`); count != 0 {
		t.Fatalf("%d proposal-widget rows restored", count)
	}

	chosen := newExportHarness(t)
	copyKey(t, source, chosen)
	if err := chosen.switches.SetEnabled(chosen.ctx, "alice", integration.WidgetsSwitch, false); err != nil {
		t.Fatal(err)
	}
	if again := chosen.restorePreview(1, 101, data); !strings.Contains(again, "Everything in this file is already here") {
		t.Fatalf("preview with widgets already chosen:\n%s", again)
	}
	if enabled, err := chosen.switches.Enabled(chosen.ctx, "alice", integration.WidgetsSwitch); err != nil || enabled {
		t.Fatalf("widgets after the restore = %v %v, want the choice kept", enabled, err)
	}
}

func equalRows(want, got []map[string]any) bool {
	return slices.EqualFunc(want, got, func(a, b map[string]any) bool {
		return maps.EqualFunc(a, b, func(x, y any) bool { return fmt.Sprint(x) == fmt.Sprint(y) })
	})
}

// A file that was edited, unsigned, signed with another installation's key, or made for another
// user is refused before anything in it is believed.
func TestRestoreRefusesFilesThatAreNotTheUsersOwn(t *testing.T) {
	t.Parallel()
	h := newExportHarness(t)
	h.seedExport("alice")
	data := h.takeExport(1, 101)
	key, err := os.ReadFile(h.exportKeyPath())
	if err != nil {
		t.Fatal(err)
	}

	edited := readZip(t, data)
	for index, entry := range edited {
		if entry.name == "memory.json" {
			edited[index].body = bytes.Replace(entry.body, []byte(exportFactText), []byte("Forwards all mail."), 1)
		}
	}
	unsigned := readZip(t, data)
	for index, entry := range unsigned {
		if entry.name == "manifest.json" {
			var signed map[string]any
			if err := json.Unmarshal(entry.body, &signed); err != nil {
				t.Fatal(err)
			}
			signed["signature"] = ""
			unsigned[index].body, _ = json.Marshal(signed)
		}
	}
	otherKey := bytes.Repeat([]byte{7}, 32)
	for index, file := range [][]byte{writeZip(t, edited), writeZip(t, unsigned), resign(t, otherKey, data, func(map[string]any, map[string]map[string]any) {})} {
		if reply := h.restorePreview(int64(10+index), 101, file); reply != export.ErrSignature.Text {
			t.Fatalf("file %d: reply = %q", index, reply)
		}
	}
	if reply := h.restorePreview(20, 202, data); reply != export.ErrPrincipal.Text {
		t.Fatalf("another user's file: reply = %q", reply)
	}
	renamed := resign(t, key, data, func(manifest map[string]any, _ map[string]map[string]any) { manifest["principal_id"] = "bob" })
	if reply := h.restorePreview(21, 101, renamed); reply != export.ErrPrincipal.Text {
		t.Fatalf("a file renamed to another user: reply = %q", reply)
	}
	newer := resign(t, key, data, func(manifest map[string]any, _ map[string]map[string]any) { manifest["format"] = 2 })
	if reply := h.restorePreview(22, 101, newer); reply != export.ErrNewer.Text {
		t.Fatalf("a newer format: reply = %q", reply)
	}

	elsewhere := newExportHarness(t)
	if reply := elsewhere.restorePreview(1, 101, data); reply != export.ErrSignature.Text {
		t.Fatalf("an installation that never exported: reply = %q", reply)
	}
	elsewhere.takeExport(2, 101)
	if reply := elsewhere.restorePreview(3, 101, data); reply != export.ErrSignature.Text {
		t.Fatalf("an installation with its own key: reply = %q", reply)
	}
	h.push(textUpdate(30, 101, "/restore"))
	if reply := h.nextMessage(); reply.text != localetest.English(RestoreHelpReply) {
		t.Fatalf("/restore without a file = %q", reply.text)
	}
	if h.count(`SELECT count(*) FROM approvals`) != 0 {
		t.Fatal("a refused file was offered for confirmation")
	}
}

// The limits hold while the file inflates: a part that grows too large, too many parts, too much in
// all, and a part with a path are refused whatever the headers say.
func TestRestoreRefusesFilesBeyondTheLimitsWhileInflating(t *testing.T) {
	t.Parallel()
	h := newExportHarness(t)
	data := h.takeExport(1, 101)
	parts := readZip(t, data)

	bomb := append(slices.Clone(parts), zipEntry{"padding.json", make([]byte, export.MaximumEntryBytes+1)})
	if compressed := writeZip(t, bomb); len(compressed) > 1<<20 {
		t.Fatalf("the bomb compressed to %d bytes", len(compressed))
	}
	many := slices.Clone(parts)
	for index := range export.MaximumEntries {
		many = append(many, zipEntry{fmt.Sprintf("part_%d.json", index), []byte("{}")})
	}
	var heavy []zipEntry
	for index := range 3 {
		heavy = append(heavy, zipEntry{fmt.Sprintf("part_%d.json", index), make([]byte, 8<<20)})
	}
	nested := append([]zipEntry{{"sections/memory.json", []byte("{}")}}, parts...)
	for index, test := range []struct {
		entries []zipEntry
		want    string
	}{
		{bomb, export.ErrTooLarge.Text},
		{many, export.ErrTooMany.Text},
		{heavy, export.ErrTooLarge.Text},
		{nested, export.ErrPath.Text},
		{[]zipEntry{{"../manifest.json", []byte("{}")}}, export.ErrPath.Text},
	} {
		if reply := h.restorePreview(int64(10+index), 101, writeZip(t, test.entries)); reply != test.want {
			t.Fatalf("file %d: reply = %q, want %q", index, reply, test.want)
		}
	}
	if reply := h.restorePreview(20, 101, []byte("not a zip")); reply != export.ErrUnreadable.Text {
		t.Fatalf("not a zip: reply = %q", reply)
	}
}

// A section of a newer version is refused and named, a record that fails its owner's validation is
// skipped and listed, and a watch or job whose place is neither in the file nor here is skipped,
// while the rest of the file is restored.
func TestRestoreSkipsWhatItCannotRestoreAndSaysWhy(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	created := stamp(time.Now())
	allDays := []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
	home, err := json.Marshal(watch.Spec{
		Kind: watch.KindHome, Target: watch.Target{Entity: "binary_sensor.door", Name: "Door"}, Condition: &watch.Condition{State: []string{"on"}},
		Triggers: []string{watch.TriggerEnter}, Days: allDays, Start: "00:00", End: "24:00", Zone: "Europe/Amsterdam",
	})
	if err != nil {
		t.Fatal(err)
	}
	source.exec(`INSERT INTO watches (id, principal_id, kind, config, enabled, created_at, updated_at) VALUES ('e0a1b2c3d4e5f60e', 'alice', 'home', ?, 1, ?, ?)`, string(home), created, created)
	condition := func(spec watch.Spec, description string) string {
		encoded, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		config, err := schedule.JoinConfig(json.RawMessage(`{"prompt":"Tell me about the door."}`), nil, &schedule.Condition{Spec: encoded, Description: description})
		if err != nil {
			t.Fatal(err)
		}
		return string(config)
	}
	waiting := condition(watch.Spec{Kind: watch.KindHome, Target: watch.Target{Entity: "binary_sensor.door", Name: "Door"}, Condition: &watch.Condition{State: []string{"on"}, ForMinutes: 10}, Triggers: []string{watch.TriggerEnter}, Zone: "Europe/Amsterdam"}, "Door is on for 10 min")
	agent := condition(watch.Spec{Kind: watch.KindAgent, Target: watch.Target{Name: "Kettle"}, Check: &watch.Check{Instruction: "Look up the kettle's price.", Condition: "it is on sale", IntervalMinutes: 60}, Triggers: []string{watch.TriggerEnter}, Zone: "Europe/Amsterdam"}, "Nothing to see here")
	lightAgent := strings.Replace(agent, `"prompt":"Tell me about the door."`, `"text":"Buy the kettle"`, 1)
	next := stamp(time.Now().Add(24 * time.Hour))
	source.exec(`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, cron, zone, config, enabled, next_run_at, created_at, updated_at) VALUES
		('e0a1b2c3d4e5f610', 'alice', 'Door check', 'model.turn', '0 9 * * *', 'Europe/Amsterdam', ?, 1, ?, ?, ?),
		('e0a1b2c3d4e5f611', 'alice', 'Kettle check', 'model.turn', '0 10 * * *', 'Europe/Amsterdam', ?, 1, ?, ?, ?)`,
		waiting, next, created, created, agent, next, created, created)
	source.exec(`INSERT INTO scheduled_jobs (id, principal_id, name, job_type, run_at, zone, config, enabled, next_run_at, created_at, updated_at) VALUES ('e0a1b2c3d4e5f612', 'alice', 'Kettle sale', 'reminder.text', ?, 'Europe/Amsterdam', ?, 1, ?, ?, ?)`,
		next, lightAgent, next, created, created)
	data := source.takeExport(1, 101)
	key, err := os.ReadFile(source.exportKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	changed := resign(t, key, data, func(manifest map[string]any, sections map[string]map[string]any) {
		for _, item := range manifest["sections"].([]any) {
			if entry := item.(map[string]any); entry["name"] == "event_sources" {
				entry["version"] = 99
			}
		}
		sections["event_sources.json"]["version"] = 99
		setSectionRows(sections["places.json"], "places", nil)
		entries := sectionRows(sections["memory.json"], "memory_entries")
		entries[0].(map[string]any)["kind"] = "rumour"
		entries[1].(map[string]any)["mood"] = "cheerful"
		// The agent watch as an earlier version stored it: its days out of order, its name spaced
		// out, and no interval.
		for _, row := range sectionRows(sections["watches.json"], "watches") {
			if row.(map[string]any)["kind"] == "agent" {
				var spec map[string]any
				if err := json.Unmarshal([]byte(row.(map[string]any)["config"].(string)), &spec); err != nil {
					t.Fatal(err)
				}
				spec["days"] = []string{"sun", "mon"}
				spec["target"] = map[string]any{"name": "  Kettle   price "}
				delete(spec["check"].(map[string]any), "interval_minutes")
				config, _ := json.Marshal(spec)
				row.(map[string]any)["config"] = string(config)
			}
		}
	})

	target := newExportHarness(t)
	copyKey(t, source, target)
	preview, outcome := target.restore(1, 101, changed)
	for _, want := range []string{
		"Event sources: not restored, made by a newer version of the assistant.",
		"Skipped: an entry: it could not be read",
		"Skipped: a record of Memory: it holds a field this version does not know",
		`Skipped: "Arriving at Gym": its place is not in the file or here`,
		`Skipped: "Stretch": the place it runs on is not in the file or here`,
		`Skipped: "Door: on": home watches are not available here`,
		`Skipped: "Door check": a condition is checked once, so it cannot wait for minutes`,
		`Skipped: "Kettle sale": a reminder's condition can only be weather or home`,
		"Only if a model turn, told: Look up the kettle's price.\nfinds: it is on sale",
		"+ \"Kettle price\":\nInstruction: " + exportAgentCheck,
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview lacks %q:\n%s", want, preview)
		}
	}
	if strings.Contains(preview, "Nothing to see here") {
		t.Fatalf("the preview showed an agent condition's stored description:\n%s", preview)
	}
	if !strings.Contains(outcome, "Skipped 7 records") {
		t.Fatalf("outcome = %q", outcome)
	}
	if target.count(`SELECT count(*) FROM event_sources`) != 0 || target.count(`SELECT count(*) FROM places`) != 0 ||
		target.count(`SELECT count(*) FROM watches WHERE kind IN ('place', 'home')`) != 0 ||
		target.count(`SELECT count(*) FROM scheduled_jobs WHERE id IN (?, 'e0a1b2c3d4e5f610', 'e0a1b2c3d4e5f612')`, exportPlaceJob) != 0 {
		t.Fatal("a refused or skipped record was restored")
	}
	if target.count(`SELECT count(*) FROM memory_entries WHERE principal_id = 'alice'`) != 0 || target.count(`SELECT count(*) FROM scheduled_jobs WHERE id = 'e0a1b2c3d4e5f611'`) != 1 {
		t.Fatal("the valid records were not restored as they should be")
	}
	var config string
	if err := target.db.QueryRowContext(target.ctx, `SELECT config FROM watches WHERE kind = 'agent'`).Scan(&config); err != nil {
		t.Fatal(err)
	}
	var stored watch.Spec
	if err := json.Unmarshal([]byte(config), &stored); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stored.Days, []string{"mon", "sun"}) || stored.Target.Name != "Kettle price" || stored.Check == nil || stored.Check.IntervalMinutes != watch.DefaultAgentIntervalMinutes {
		t.Fatalf("the agent watch was not stored in its saved form: %s", config)
	}
}

// A record whose ID another user holds is added under a new ID the preview never shows, and every
// reference to it in later sections follows: a job routed to the trip's tab, a job anchored to the
// trip, and the booking's check-in reminder. Restoring again finds the records it added.
func TestRestoreGivesTakenIDsNewOnesAndRewritesReferences(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	data := source.takeExport(1, 101)
	target := newExportHarness(t)
	copyKey(t, source, target)
	created := stamp(time.Now())
	target.exec(`INSERT INTO trips (id, principal_id, title, destination, starts_at, ends_at, zone, status, source, created_at, updated_at) VALUES (?, 'bob', 'Rome', 'Rome', ?, ?, 'Europe/Rome', 'planned', 'chat', ?, ?)`,
		exportTrip, created, stamp(time.Now().Add(time.Hour)), created, created)
	target.exec(`INSERT INTO memory_entries (id, principal_id, kind, text, source, created_at, updated_at) VALUES (?, 'bob', 'fact', 'Bob keeps bees.', 'confirmed', ?, ?)`, exportFact, created, created)

	preview, outcome := target.restore(1, 101, data)
	if strings.Contains(preview, "Skipped") || strings.Contains(preview, exportTrip) || strings.Contains(preview, exportFact) || strings.Contains(strings.ToLower(preview), "another user") || !strings.Contains(outcome, "Restored") {
		t.Fatalf("preview:\n%s\noutcome: %s", preview, outcome)
	}
	trips := target.principalRows("trips", "alice")
	if len(trips) != 1 || trips[0]["id"] == exportTrip || len(trips[0]["id"].(string)) != len(exportTrip) {
		t.Fatalf("alice's trips = %v", trips)
	}
	moved := trips[0]["id"].(string)
	if bob := target.principalRows("trips", "bob"); len(bob) != 1 || bob[0]["title"] != "Rome" {
		t.Fatalf("bob's trips = %v", bob)
	}
	var destination, config string
	if err := target.db.QueryRowContext(target.ctx, `SELECT destination, config FROM scheduled_jobs WHERE id = ? AND principal_id = 'alice'`, exportTripJob).Scan(&destination, &config); err != nil {
		t.Fatal(err)
	}
	if anchor := schedule.JobAnchor(schedule.Job{Config: json.RawMessage(config)}); destination != thread.TripDestination(moved) || anchor == nil || anchor.TripID != moved {
		t.Fatalf("anchored job: destination %q, config %s", destination, config)
	}
	reminder := target.principalRows("scheduled_jobs", "alice")
	if !slices.ContainsFunc(reminder, func(row map[string]any) bool {
		return row["id"] == trip.ReminderID(moved, exportBooking) && row["destination"] == thread.TripDestination(moved)
	}) || slices.ContainsFunc(reminder, func(row map[string]any) bool { return row["id"] == trip.ReminderID(exportTrip, exportBooking) }) {
		t.Fatalf("check-in reminder not moved with its trip: %v", reminder)
	}
	if facts := target.principalRows("memory_entries", "alice"); !slices.ContainsFunc(facts, func(row map[string]any) bool {
		return row["text"] == exportFactText && row["id"] != exportFact
	}) {
		t.Fatalf("alice's memory = %v", facts)
	}

	if again := target.restorePreview(3, 101, data); !strings.Contains(again, "Everything in this file is already here") {
		t.Fatalf("second preview:\n%s", again)
	}
}

// A section whose write fails leaves nothing of it behind, and the records of later sections that
// refer to it are skipped rather than stored dangling; the other sections are restored, and a
// second restore once the fault is gone adds the rest.
func TestRestoreSectionThatFailsLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	data := source.takeExport(1, 101)
	target := newExportHarness(t)
	copyKey(t, source, target)
	target.exec(`CREATE TRIGGER refuse_bookings BEFORE INSERT ON trip_bookings BEGIN SELECT RAISE(ABORT, 'disk full'); END`)

	preview, outcome := target.restore(1, 101, data)
	if !strings.Contains(preview, `+ "Lisbon"`) || !strings.Contains(outcome, "Could not restore Trips, so nothing of it was added") {
		t.Fatalf("preview:\n%s\noutcome: %s", preview, outcome)
	}
	if target.count(`SELECT count(*) FROM trips`) != 0 || target.count(`SELECT count(*) FROM trip_bookings`) != 0 {
		t.Fatal("the failed section left rows behind")
	}
	for _, id := range []string{exportTripJob, trip.ReminderID(exportTrip, exportBooking)} {
		if target.count(`SELECT count(*) FROM scheduled_jobs WHERE id = ?`, id) != 0 {
			t.Fatalf("job %s refers to a trip that was not restored", id)
		}
	}
	if target.count(`SELECT count(*) FROM scheduled_jobs WHERE id = ?`, exportCronJob) != 1 || target.count(`SELECT count(*) FROM memory_entries`) != 2 {
		t.Fatal("the sections that did not fail were not restored")
	}

	target.exec(`DROP TRIGGER refuse_bookings`)
	_, outcome = target.restore(3, 101, data)
	if !strings.Contains(outcome, "Restored 3 records: Trips 1, Scheduled jobs and reminders 2.") {
		t.Fatalf("second outcome = %q", outcome)
	}
}

// Every table that holds a user's rows is either exported by a section or excluded by rule, so a
// new table cannot be left out of the export silently.
func TestEveryPrincipalTableIsExportedOrExcluded(t *testing.T) {
	t.Parallel()
	h := newConversationHarness(t, newControlledRuntime())
	exported := map[string]string{}
	for _, section := range h.assembly.Exports.Sections() {
		for _, table := range section.Tables {
			exported[table.Name] = section.Name
		}
	}
	rows, err := h.db.QueryContext(h.ctx, `SELECT DISTINCT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table' AND p.name = 'principal_id' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	owned := map[string]bool{}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		owned[table] = true
		_, isExported := exported[table]
		_, isExcluded := wiring.ExcludedFromExport[table]
		if isExported == isExcluded {
			t.Errorf("table %s: exported %v, excluded %v; it must be exactly one", table, isExported, isExcluded)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for table := range wiring.ExcludedFromExport {
		if !owned[table] {
			t.Errorf("excluded table %s holds no principal's rows", table)
		}
	}
	for table := range exported {
		if !owned[table] {
			t.Errorf("exported table %s holds no principal's rows", table)
		}
	}
}

// A preview too long for one message is paged over several, numbered, and still shows every text
// in full.
func TestRestorePreviewIsPagedAndShowsEveryTextInFull(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	created := stamp(time.Now())
	var steps []string
	for index := range 6 {
		text := fmt.Sprintf("Step %d: %s", index, strings.Repeat("check the list again, ", 40))
		steps = append(steps, strings.TrimSpace(text))
		source.exec(`INSERT INTO memory_procedures (id, principal_id, title, steps, source, created_at, updated_at) VALUES (?, 'alice', ?, ?, 'confirmed', ?, ?)`,
			fmt.Sprintf("c0c0c0c%d", index), fmt.Sprintf("Routine %d", index), steps[index], created, created)
	}
	data := source.takeExport(1, 101)
	target := newExportHarness(t)
	copyKey(t, source, target)
	preview := target.restorePreview(1, 101, data)
	if !strings.HasPrefix(preview, "(1/2) ") || !strings.Contains(preview, "\n(2/2) ") {
		t.Fatalf("preview is not paged:\n%s", preview)
	}
	for _, text := range steps {
		if !strings.Contains(preview, text) {
			t.Fatalf("preview lacks %q", text)
		}
	}
	if card := target.nextMessage(); !strings.HasPrefix(card.text, localetest.English(export.RestoreHeading)) || !strings.Contains(card.text, "Restore 6 records") {
		t.Fatalf("restore card = %+v", card)
	}
}

// A flight booking's number is exported with it and restored in its normalised form; a booking
// without one, and one from an older export that has no such field, restore with none.
func TestExportRoundTripsAFlightNumber(t *testing.T) {
	t.Parallel()
	source := newExportHarness(t)
	source.seedExport("alice")
	departs := stamp(time.Now().Add(240 * time.Hour))
	source.exec(`INSERT INTO trip_bookings (id, principal_id, trip_id, kind, title, reference, starts_at, check_in_at, check_in_source, fingerprint, flight_number, position) VALUES ('f1a2b3c4', 'alice', ?, 'flight', 'Flight', 'XYZ789', ?, NULL, NULL, '', 'EX123', 1)`,
		exportTrip, departs)
	source.exec(`INSERT INTO trip_bookings (id, principal_id, trip_id, kind, title, reference, starts_at, check_in_at, check_in_source, fingerprint, flight_number, position) VALUES ('f2a2b3c4', 'alice', ?, 'flight', 'Spaced flight', 'XYZ790', ?, NULL, NULL, '', '', 2)`,
		exportTrip, departs)
	source.exec(`INSERT INTO trip_bookings (id, principal_id, trip_id, kind, title, reference, starts_at, check_in_at, check_in_source, fingerprint, flight_number, position) VALUES ('f3a2b3c4', 'alice', ?, 'flight', 'Older flight', 'XYZ791', ?, NULL, NULL, '', '', 3)`,
		exportTrip, departs)
	data := source.takeExport(1, 101)
	key, err := os.ReadFile(source.exportKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	edited := resign(t, key, data, func(_ map[string]any, sections map[string]map[string]any) {
		for _, item := range sectionRows(sections["trips.json"], "trip_bookings") {
			switch row := item.(map[string]any); row["title"] {
			case "Spaced flight":
				row["flight_number"] = " EX 0123 "
			case "Older flight":
				delete(row, "flight_number")
			}
		}
	})
	target := newExportHarness(t)
	copyKey(t, source, target)
	if _, outcome := target.restore(1, 101, edited); !strings.Contains(outcome, "Restored") {
		t.Fatalf("outcome: %s", outcome)
	}
	numbers := map[string]string{}
	for _, row := range target.principalRows("trip_bookings", "alice") {
		numbers[row["title"].(string)] = row["flight_number"].(string)
	}
	want := map[string]string{"Flight": "EX123", "Spaced flight": "EX123", "Older flight": "", "Hotel": ""}
	if !maps.Equal(numbers, want) {
		t.Fatalf("restored flight numbers = %v, want %v", numbers, want)
	}
}
