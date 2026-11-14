package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// Migration 027 rebuilds approvals for direct actions; records stored before it must survive
// unchanged as proposals, and a direct record then lives beside them without joining a bundle.
func TestMigrationDirectApprovalsKeepsExistingRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path, err := filepath.Abs(filepath.Join(t.TempDir(), "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.name >= "027" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, chat_id, message_id, created_at, expires_at)
		 VALUES ('aaaaaaaaaaaaaaaa', 'aaaaaaaaaaaaaaaa', 0, 'alice', 'note_write', '{"name":"a"}', '` + hash + `', 'First', 'pending', 101, 7, '2026-09-01T10:00:00.000Z', '2099-09-01T10:10:00.000Z')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, chat_id, message_id, created_at, expires_at)
		 VALUES ('bbbbbbbbbbbbbbbb', 'aaaaaaaaaaaaaaaa', 1, 'alice', 'note_write', '{"name":"b"}', '` + hash + `', 'Second', 'pending', 101, 7, '2026-09-01T10:00:00.000Z', '2099-09-01T10:10:00.000Z')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, outcome, created_at, expires_at, started_at, finished_at)
		 VALUES ('cccccccccccccccc', 'cccccccccccccccc', 0, 'alice', 'note_write', '{}', '` + hash + `', 'Third', 'executed', 'Saved.', '2026-08-01T10:00:00.000Z', '2026-08-01T10:10:00.000Z', '2026-08-01T10:01:00.000Z', '2026-08-01T10:01:01.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	pending, err := store.PendingApprovals(ctx, "alice", 0)
	if err != nil || len(pending) != 2 || pending[0].Summary != "First" || pending[1].Position != 1 || pending[1].MessageID != 7 ||
		pending[0].Mode != approval.ModeProposed || pending[0].RequestID != "" || pending[0].Undo != nil {
		t.Fatalf("pending after migration = %+v %v", pending, err)
	}
	executed, found, err := store.LoadApproval(ctx, "cccccccccccccccc")
	if err != nil || !found || executed.Status != approval.StatusExecuted || executed.Outcome != "Saved." || executed.StartedAt.IsZero() || executed.FinishedAt.IsZero() {
		t.Fatalf("executed after migration = %+v %v %v", executed, found, err)
	}

	now := time.Now()
	direct := approval.Record{
		ID: "dddddddddddddddd", PrincipalID: "alice", Tool: "note_write", Arguments: json.RawMessage(`{"name":"d"}`), Hash: hash,
		Summary: "Direct", CreatedAt: now, ExpiresAt: now.Add(approval.UndoWindow), RequestID: "33333333-3333-4333-8333-333333333333",
	}
	if err := store.CreateDirectApproval(ctx, direct); err != nil {
		t.Fatal(err)
	}
	interrupted := direct
	interrupted.ID, interrupted.Summary = "eeeeeeeeeeeeeeee", "Interrupted"
	if err := store.CreateDirectApproval(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingApprovals(ctx, "alice", 0); err != nil || len(pending) != 2 {
		t.Fatalf("pending beside direct records = %d %v", len(pending), err)
	}
	if found, ok, err := store.FindDirectApproval(ctx, "alice", "note_write", hash, direct.RequestID); err != nil || !ok || found.ID != direct.ID || found.Status != approval.StatusConfirmed || found.StartedAt.IsZero() {
		t.Fatalf("duplicate lookup = %+v %v %v", found, ok, err)
	}
	if finished, err := store.FinishDirectApproval(ctx, direct.ID, approval.StatusDone, "", "Saved.", "", json.RawMessage(`{"name":"d"}`), now.Add(approval.UndoWindow)); err != nil || !finished {
		t.Fatalf("finish direct = %v %v", finished, err)
	}
	if err := store.SetApprovalWarning(ctx, direct.ID, "Warned"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.DirectApprovals(ctx, "alice", direct.RequestID)
	if err != nil || len(listed) != 2 || listed[0].Summary != "Warned" || listed[0].Status != approval.StatusDone || string(listed[0].Undo) != `{"name":"d"}` || listed[0].UndoUntil.IsZero() {
		t.Fatalf("direct records = %+v %v", listed, err)
	}

	expired, err := store.ExpireApprovals(ctx, now, true)
	if err != nil {
		t.Fatal(err)
	}
	var interruptedFound bool
	for _, record := range expired {
		if record.ID == interrupted.ID {
			interruptedFound = record.Status == approval.StatusFailed && record.Reason == approval.ReasonInterrupted
		}
		if record.ID == direct.ID {
			t.Fatal("the restart sweep touched a finished direct record")
		}
	}
	if !interruptedFound {
		t.Fatalf("restart sweep = %+v, want the started direct record failed as interrupted", expired)
	}

	finished, _, err := store.LoadApproval(ctx, direct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimUndo(ctx, direct.ID, "alice", now); err != nil || !claimed {
		t.Fatalf("first undo claim = %v %v", claimed, err)
	}
	if claimed, err := store.ClaimUndo(ctx, direct.ID, "alice", now); err != nil || claimed {
		t.Fatalf("second undo claim = %v %v", claimed, err)
	}
	if undoing, _, err := store.LoadApproval(ctx, direct.ID); err != nil || undoing.Status != approval.StatusDone || undoing.Reason != approval.ReasonUndoing || !undoing.FinishedAt.Equal(finished.FinishedAt) {
		t.Fatalf("claimed record = %+v %v, want done and undoing with its finish time", undoing, err)
	}

	// A restart while the Undo runs leaves the action done and lets a later press undo it.
	expired, err = store.ExpireApprovals(ctx, now, true)
	if err != nil || len(expired) != 1 || expired[0].ID != direct.ID || expired[0].Status != approval.StatusDone || expired[0].Reason != approval.ReasonUndoInterrupted {
		t.Fatalf("restart sweep during an undo = %+v %v", expired, err)
	}
	if claimed, err := store.ClaimUndo(ctx, direct.ID, "alice", now); err != nil || !claimed {
		t.Fatalf("undo claim after the restart = %v %v", claimed, err)
	}
	if err := store.CompleteUndo(ctx, direct.ID, "Removed.", ""); err != nil {
		t.Fatal(err)
	}
	if undone, _, err := store.LoadApproval(ctx, direct.ID); err != nil || undone.Status != approval.StatusUndone || undone.Reason != "" || undone.Outcome != "Removed." || !undone.FinishedAt.Equal(finished.FinishedAt) {
		t.Fatalf("undone record = %+v %v", undone, err)
	}
	var foreignKeyProblems int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyProblems); err != nil || foreignKeyProblems != 0 {
		t.Fatalf("foreign key check = %d %v", foreignKeyProblems, err)
	}
}

// Migration 054 flags a proposal whose message is a widget. A proposal stored before it, such as one
// pending while the bot is upgraded, reads as a text message; the flag is then written with the
// message and cleared with it.
func TestMigrationApprovalWidgetsKeepsExistingRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := migrateUpTo(t, ctx, "054")
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	exec(t, ctx, db,
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, chat_id, message_id, created_at, expires_at, thread_id, heading)
		 VALUES ('aaaaaaaaaaaaaaaa', 'aaaaaaaaaaaaaaaa', 0, 'alice', 'note_write', '{"name":"a"}', '`+hash+`', 'First', 'pending', 101, 7, '2026-10-01T10:00:00.000Z', '2099-10-01T10:10:00.000Z', 3, 'Weekly review')`,
	)

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	record, found, err := store.LoadApproval(ctx, "aaaaaaaaaaaaaaaa")
	if err != nil || !found || record.Widget != approval.NotWidget || record.MessageID != 7 || record.ThreadID != 3 || record.Heading != "Weekly review" || record.Summary != "First" {
		t.Fatalf("record after migration = %+v %v %v", record, found, err)
	}
	if err := store.SetApprovalMessage(ctx, []string{record.ID}, 101, 8, approval.ProposalWidget); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingApprovals(ctx, "alice", 3); err != nil || len(pending) != 1 || pending[0].Widget != approval.ProposalWidget || pending[0].MessageID != 8 {
		t.Fatalf("record shown in a widget = %+v %v", pending, err)
	}
	if err := store.SetApprovalMessage(ctx, []string{record.ID}, 101, 0, approval.NotWidget); err != nil {
		t.Fatal(err)
	}
	if undelivered, err := store.UndeliveredApprovals(ctx); err != nil || len(undelivered) != 1 || undelivered[0].Widget != approval.NotWidget {
		t.Fatalf("record after a failed delivery = %+v %v", undelivered, err)
	}
	foreignKeysHold(t, ctx, db)
}

// Migration 057 replaces the widget flag with the widget's kind: a proposal flagged as a widget
// becomes a proposal widget, and one not flagged has no kind.
func TestMigrationApprovalWidgetKindCarriesTheFlagOver(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := migrateUpTo(t, ctx, "057")
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	insert := func(id string, messageID, widget int) string {
		return `INSERT INTO approvals (id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, chat_id, message_id, created_at, expires_at, thread_id, widget)
		 VALUES ('` + id + `', '` + id + `', 0, 'alice', 'note_write', '{"name":"a"}', '` + hash + `', 'First', 'pending', 101, ` + strconv.Itoa(messageID) + `, '2026-10-01T10:00:00.000Z', '2099-10-01T10:10:00.000Z', 3, ` + strconv.Itoa(widget) + `)`
	}
	exec(t, ctx, db,
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		insert("aaaaaaaaaaaaaaaa", 7, 1),
		insert("bbbbbbbbbbbbbbbb", 8, 0),
	)

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	for id, want := range map[string]approval.WidgetKind{"aaaaaaaaaaaaaaaa": approval.ProposalWidget, "bbbbbbbbbbbbbbbb": approval.NotWidget} {
		record, found, err := store.LoadApproval(ctx, id)
		if err != nil || !found || record.Widget != want || record.Summary != "First" {
			t.Fatalf("record %s after migration = %+v %v %v, want kind %q", id, record, found, err, want)
		}
	}
	if err := store.SetApprovalMessage(ctx, []string{"bbbbbbbbbbbbbbbb"}, 101, 9, approval.ChangesWidget); err != nil {
		t.Fatal(err)
	}
	if record, _, err := store.LoadApproval(ctx, "bbbbbbbbbbbbbbbb"); err != nil || record.Widget != approval.ChangesWidget {
		t.Fatalf("record shown in a changes widget = %+v %v", record, err)
	}
	if count := countRows(t, store, `SELECT count(*) FROM pragma_table_info('approvals') WHERE name = 'widget'`); count != 0 {
		t.Fatal("the widget flag remains")
	}
	if count := countRows(t, store, `SELECT count(*) FROM pragma_table_info('approvals') WHERE name = 'title'`); count != 1 {
		t.Fatal("the title column is missing")
	}
	if count := countRows(t, store, `SELECT count(*) FROM approvals WHERE title = ''`); count != 2 {
		t.Fatalf("%d rows kept an empty title, want 2", count)
	}
	created := approval.Record{
		ID: "cccccccccccccccc", BundleID: "cccccccccccccccc", PrincipalID: "alice", Tool: "note_write", Arguments: json.RawMessage(`{}`), Hash: hash,
		Summary: "From a review:\nSave the note", Title: "Save the note", Status: approval.StatusPending,
		CreatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2099, 10, 1, 10, 0, 0, 0, time.UTC),
	}
	if _, err := store.CreateApprovals(ctx, []approval.Record{created}); err != nil {
		t.Fatal(err)
	}
	if record, found, err := store.LoadApproval(ctx, created.ID); err != nil || !found || record.Title != "Save the note" {
		t.Fatalf("stored title = %+v %v %v", record, found, err)
	}
	foreignKeysHold(t, ctx, db)
}

// ConfirmChosen confirms the kept pending records and ends the other pending ones as left out, in
// one transaction. It changes nothing while no kept record is pending and unexpired.
func TestConfirmChosenEndsTheOthersOnlyWhileAKeptRecordWaits(t *testing.T) {
	t.Parallel()
	store, ctx := openTestStore(t)
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	var records []approval.Record
	for position, id := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc", "dddddddddddddddd"} {
		records = append(records, approval.Record{
			ID: id, BundleID: "aaaaaaaaaaaaaaaa", Position: position, PrincipalID: "alice", Tool: "note_write",
			Arguments: json.RawMessage(`{}`), Hash: hash, Summary: "Change " + id, Status: approval.StatusPending,
			CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		})
	}
	if _, err := store.CreateApprovals(ctx, records); err != nil {
		t.Fatal(err)
	}
	statuses := func() string {
		t.Helper()
		loaded, err := store.LoadBundle(ctx, "aaaaaaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		var line []string
		for _, record := range loaded {
			line = append(line, record.Status+"/"+record.Reason)
		}
		return strings.Join(line, " ")
	}
	finished, err := store.FinishApproval(ctx, "bbbbbbbbbbbbbbbb", approval.StatusPending, approval.StatusRejected, approval.ReasonWithdrawn, "", "")
	if err != nil || !finished {
		t.Fatalf("withdraw = %v %v", finished, err)
	}
	const untouched = "pending/ rejected/withdrawn pending/ pending/"

	confirmed, leftOut, err := store.ConfirmChosen(ctx, "aaaaaaaaaaaaaaaa", "alice", []string{"bbbbbbbbbbbbbbbb"}, now)
	if err != nil || len(confirmed) != 0 || len(leftOut) != 0 || statuses() != untouched {
		t.Fatalf("kept record that no longer waits = %v %v %v, %s", confirmed, leftOut, err, statuses())
	}
	confirmed, leftOut, err = store.ConfirmChosen(ctx, "aaaaaaaaaaaaaaaa", "alice", []string{"aaaaaaaaaaaaaaaa"}, now.Add(10*time.Minute))
	if err != nil || len(confirmed) != 0 || len(leftOut) != 0 || statuses() != untouched {
		t.Fatalf("kept record past its deadline = %v %v %v, %s", confirmed, leftOut, err, statuses())
	}
	confirmed, leftOut, err = store.ConfirmChosen(ctx, "aaaaaaaaaaaaaaaa", "bob", []string{"aaaaaaaaaaaaaaaa"}, now)
	if err != nil || len(confirmed) != 0 || len(leftOut) != 0 || statuses() != untouched {
		t.Fatalf("another principal's choice = %v %v %v, %s", confirmed, leftOut, err, statuses())
	}

	confirmed, leftOut, err = store.ConfirmChosen(ctx, "aaaaaaaaaaaaaaaa", "alice", []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"}, now)
	if err != nil || !slices.Equal(confirmed, []string{"aaaaaaaaaaaaaaaa", "cccccccccccccccc"}) || !slices.Equal(leftOut, []string{"dddddddddddddddd"}) {
		t.Fatalf("choice = %v %v %v", confirmed, leftOut, err)
	}
	if got := statuses(); got != "confirmed/ rejected/withdrawn confirmed/ rejected/left_out" {
		t.Fatalf("statuses after the choice = %s", got)
	}
	if again, left, err := store.ConfirmChosen(ctx, "aaaaaaaaaaaaaaaa", "alice", []string{"aaaaaaaaaaaaaaaa"}, now); err != nil || len(again) != 0 || len(left) != 0 {
		t.Fatalf("repeated choice = %v %v %v", again, left, err)
	}
}
