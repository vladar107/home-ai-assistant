package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/looseend"
)

var _ looseend.Repository = (*Store)(nil)

const looseEndColumns = `id, principal_id, kind, title, counterpart, due_at, due_all_day, nudge_on, nudged_on, source, account, source_ref,
	thread_ref, outside, status, created_at, updated_at`

// CreateLooseEnd checks the key, the thread merge, and the open cap in the transaction that
// stores the item, so two scans cannot both add the same deadline or pass the cap.
func (s *Store) CreateLooseEnd(ctx context.Context, insert looseend.Insert) (looseend.Outcome, string, error) {
	if s == nil || s.db == nil {
		return "", "", fmt.Errorf("create loose end: SQLite store is not open")
	}
	item := insert.Item
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("begin loose end: %w", err)
	}
	defer tx.Rollback()
	if outcome, id, err := matchLooseEnd(ctx, tx, insert); err != nil || outcome != "" {
		return outcome, id, err
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM loose_ends WHERE principal_id = ? AND status = 'open'`, item.PrincipalID).Scan(&open); err != nil {
		return "", "", fmt.Errorf("count open loose ends: %w", err)
	}
	if open >= insert.MaximumOpen {
		return looseend.OutcomeOpenCapped, "", nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO loose_ends (`+looseEndColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.PrincipalID, item.Kind, item.Title, item.Counterpart, formatTime(item.DueAt), boolInt(item.DueAllDay), item.NudgeOn, item.NudgedOn,
		item.Source, item.Account, item.SourceRef, item.ThreadRef, boolInt(item.Outside), item.Status, formatTime(item.CreatedAt), formatTime(item.UpdatedAt)); err != nil {
		return "", "", fmt.Errorf("insert loose end: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit loose end: %w", err)
	}
	return looseend.OutcomeCreated, item.ID, nil
}

func (s *Store) FindLooseEnd(ctx context.Context, insert looseend.Insert) (looseend.Outcome, string, error) {
	if s == nil || s.db == nil {
		return "", "", fmt.Errorf("find loose end: SQLite store is not open")
	}
	return matchLooseEnd(ctx, s.db, insert)
}

// matchLooseEnd finds the item that keeps the insert's item out: one with the same key, or an open
// or tombstoned one of the same kind, account, and thread due on the same local day. It returns an
// empty outcome when there is none.
func matchLooseEnd(ctx context.Context, db queryRower, insert looseend.Insert) (looseend.Outcome, string, error) {
	item := insert.Item
	var id string
	var status looseend.Status
	err := db.QueryRowContext(ctx,
		`SELECT id, status FROM loose_ends WHERE principal_id = ? AND source = ? AND account = ? AND source_ref = ? AND kind = ?`,
		item.PrincipalID, item.Source, item.Account, item.SourceRef, item.Kind).Scan(&id, &status)
	switch {
	case err == nil && status.Tombstone():
		return looseend.OutcomeTombstoned, id, nil
	case err == nil:
		return looseend.OutcomeDuplicate, id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", fmt.Errorf("find loose end: %w", err)
	}
	if item.ThreadRef == "" {
		return "", "", nil
	}
	err = db.QueryRowContext(ctx,
		`SELECT id, status FROM loose_ends
		 WHERE principal_id = ? AND kind = ? AND account = ? AND thread_ref = ? AND due_at >= ? AND due_at < ?
		   AND status IN ('open', 'done', 'dropped', 'closed')
		 ORDER BY status = 'open' DESC, rowid LIMIT 1`,
		item.PrincipalID, item.Kind, item.Account, item.ThreadRef, formatTime(insert.DayStart), formatTime(insert.DayEnd)).Scan(&id, &status)
	switch {
	case err == nil && status == looseend.StatusOpen:
		return looseend.OutcomeMerged, id, nil
	case err == nil:
		return looseend.OutcomeTombstoned, id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", fmt.Errorf("find loose end in thread: %w", err)
	}
	return "", "", nil
}

func (s *Store) LoadLooseEnd(ctx context.Context, principalID, id string) (looseend.Item, bool, error) {
	if s == nil || s.db == nil {
		return looseend.Item{}, false, fmt.Errorf("load loose end: SQLite store is not open")
	}
	items, err := s.queryLooseEnds(ctx, `SELECT `+looseEndColumns+` FROM loose_ends WHERE principal_id = ? AND id = ?`, principalID, id)
	if err != nil || len(items) == 0 {
		return looseend.Item{}, false, err
	}
	return items[0], true, nil
}

func (s *Store) ListLooseEnds(ctx context.Context, principalID string, status looseend.Status, kind looseend.Kind) ([]looseend.Item, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list loose ends: SQLite store is not open")
	}
	return s.queryLooseEnds(ctx, `SELECT `+looseEndColumns+` FROM loose_ends
		 WHERE principal_id = ? AND status = ? AND (? = '' OR kind = ?) ORDER BY due_at, rowid`, principalID, status, kind, kind)
}

func (s *Store) LooseEndRefs(ctx context.Context, principalID string, source looseend.Source) ([]looseend.Ref, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list loose end sources: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account, source_ref FROM loose_ends WHERE principal_id = ? AND source = ?`, principalID, source)
	if err != nil {
		return nil, fmt.Errorf("list loose end sources: %w", err)
	}
	defer rows.Close()
	var refs []looseend.Ref
	for rows.Next() {
		var ref looseend.Ref
		if err := rows.Scan(&ref.Account, &ref.SourceRef); err != nil {
			return nil, fmt.Errorf("scan loose end source: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list loose end sources: %w", err)
	}
	return refs, nil
}

func (s *Store) DueLooseEnds(ctx context.Context, principalID, today string, now time.Time) ([]looseend.Item, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list due loose ends: SQLite store is not open")
	}
	return s.queryLooseEnds(ctx, `SELECT `+looseEndColumns+` FROM loose_ends
		 WHERE principal_id = ? AND status = 'open' AND nudge_on <= ? AND nudged_on < nudge_on AND due_at > ?
		 ORDER BY due_at, rowid`, principalID, today, formatTime(now))
}

func (s *Store) ChangeLooseEndStatus(ctx context.Context, change looseend.StatusChange) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("change loose end: SQLite store is not open")
	}
	if len(change.From) == 0 {
		return false, nil
	}
	args := []any{change.To, formatTime(change.At), change.PrincipalID, change.ID}
	for _, status := range change.From {
		args = append(args, status)
	}
	query := `UPDATE loose_ends SET status = ?, updated_at = ? WHERE principal_id = ? AND id = ? AND status IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(change.From)), ",") + `)`
	if !change.Expected.IsZero() {
		query += ` AND updated_at = ?`
		args = append(args, formatTime(change.Expected))
	}
	return s.execOne(ctx, "change loose end", query, args...)
}

func (s *Store) SnoozeLooseEnd(ctx context.Context, principalID, id, nudgeOn string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("snooze loose end: SQLite store is not open")
	}
	return s.execOne(ctx, "snooze loose end", `UPDATE loose_ends SET nudge_on = ?, updated_at = ? WHERE principal_id = ? AND id = ? AND status = 'open'`,
		nudgeOn, formatTime(at), principalID, id)
}

func (s *Store) RecordLooseEndNudge(ctx context.Context, principalID, id, expectedNudgeOn, nudgedOn, nudgeOn string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("record loose end nudge: SQLite store is not open")
	}
	return s.execOne(ctx, "record loose end nudge",
		`UPDATE loose_ends SET nudged_on = ?, nudge_on = ? WHERE principal_id = ? AND id = ? AND status = 'open' AND nudge_on = ?`,
		nudgedOn, nudgeOn, principalID, id, expectedNudgeOn)
}

func (s *Store) DeleteLooseEnd(ctx context.Context, principalID, id string, expected time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete loose end: SQLite store is not open")
	}
	return s.execOne(ctx, "delete loose end", `DELETE FROM loose_ends WHERE principal_id = ? AND id = ? AND updated_at = ?`,
		principalID, id, formatTime(expected))
}

func (s *Store) ExpireLooseEnds(ctx context.Context, principalID string, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("expire loose ends: SQLite store is not open")
	}
	return s.execCount(ctx, "expire loose ends", `UPDATE loose_ends SET status = 'expired', updated_at = ? WHERE principal_id = ? AND status = 'open' AND due_at <= ?`,
		formatTime(now), principalID, formatTime(now))
}

func (s *Store) PruneLooseEnds(ctx context.Context, before time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("prune loose ends: SQLite store is not open")
	}
	return s.execCount(ctx, "prune loose ends", `DELETE FROM loose_ends WHERE status <> 'open' AND due_at < ?`, formatTime(before))
}

func (s *Store) LoadLooseEndScan(ctx context.Context, principalID string) (looseend.ScanState, bool, error) {
	if s == nil || s.db == nil {
		return looseend.ScanState{}, false, fmt.Errorf("load loose end scan: SQLite store is not open")
	}
	var readUntil, declined string
	var backfillUntil, backfillBefore sql.NullString
	var state looseend.ScanState
	err := s.db.QueryRowContext(ctx, `SELECT read_until, backfill_until, backfill_before, stalls, backfill_stalls, calendar_digest, calendar_cursor, calendar_declined
		 FROM loose_end_scans WHERE principal_id = ?`,
		principalID).Scan(&readUntil, &backfillUntil, &backfillBefore, &state.Stalls, &state.BackfillStalls, &state.CalendarDigest, &state.CalendarCursor, &declined)
	if errors.Is(err, sql.ErrNoRows) {
		return looseend.ScanState{}, false, nil
	}
	if err != nil {
		return looseend.ScanState{}, false, fmt.Errorf("load loose end scan: %w", err)
	}
	if state.ReadUntil, err = time.Parse(time.RFC3339Nano, readUntil); err != nil {
		return looseend.ScanState{}, false, fmt.Errorf("parse loose end scan time: %w", err)
	}
	if backfillUntil.Valid {
		if state.BackfillUntil, err = time.Parse(time.RFC3339Nano, backfillUntil.String); err != nil {
			return looseend.ScanState{}, false, fmt.Errorf("parse loose end backfill time: %w", err)
		}
		if state.BackfillBefore, err = time.Parse(time.RFC3339Nano, backfillBefore.String); err != nil {
			return looseend.ScanState{}, false, fmt.Errorf("parse loose end backfill time: %w", err)
		}
	}
	if err := json.Unmarshal([]byte(declined), &state.CalendarDeclined); err != nil {
		return looseend.ScanState{}, false, fmt.Errorf("parse declined calendar events: %w", err)
	}
	if len(state.CalendarDeclined) == 0 {
		state.CalendarDeclined = nil
	}
	return state, true, nil
}

// CreateLooseEndScan keeps a backfill window only while both of its bounds are set.
func (s *Store) CreateLooseEndScan(ctx context.Context, principalID string, state looseend.ScanState) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create loose end scan: SQLite store is not open")
	}
	backfillUntil, backfillBefore := looseEndBackfill(state)
	declined, err := looseEndDeclined(state)
	if err != nil {
		return fmt.Errorf("create loose end scan: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO loose_end_scans (principal_id, read_until, backfill_until, backfill_before, stalls, backfill_stalls, calendar_digest, calendar_cursor, calendar_declined)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (principal_id) DO NOTHING`,
		principalID, formatTime(state.ReadUntil), backfillUntil, backfillBefore, state.Stalls, state.BackfillStalls, state.CalendarDigest, state.CalendarCursor, declined); err != nil {
		return fmt.Errorf("create loose end scan: %w", err)
	}
	return nil
}

// SaveLooseEndScan keeps a backfill window only while both of its bounds are set.
func (s *Store) SaveLooseEndScan(ctx context.Context, principalID string, state looseend.ScanState) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save loose end scan: SQLite store is not open")
	}
	backfillUntil, backfillBefore := looseEndBackfill(state)
	declined, err := looseEndDeclined(state)
	if err != nil {
		return fmt.Errorf("save loose end scan: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loose_end_scans SET read_until = ?, backfill_until = ?, backfill_before = ?, stalls = ?, backfill_stalls = ?, calendar_digest = ?, calendar_cursor = ?,
		 calendar_declined = ? WHERE principal_id = ?`,
		formatTime(state.ReadUntil), backfillUntil, backfillBefore, state.Stalls, state.BackfillStalls, state.CalendarDigest, state.CalendarCursor, declined, principalID); err != nil {
		return fmt.Errorf("save loose end scan: %w", err)
	}
	return nil
}

func looseEndDeclined(state looseend.ScanState) (string, error) {
	declined := state.CalendarDeclined
	if declined == nil {
		declined = []string{}
	}
	encoded, err := json.Marshal(declined)
	return string(encoded), err
}

func looseEndBackfill(state looseend.ScanState) (until, before any) {
	if !state.BackfillUntil.IsZero() && !state.BackfillBefore.IsZero() {
		return formatTime(state.BackfillUntil), formatTime(state.BackfillBefore)
	}
	return nil, nil
}

func (s *Store) ResetLooseEndScan(ctx context.Context, principalID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("reset loose end scan: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM loose_end_scans WHERE principal_id = ?`, principalID); err != nil {
		return fmt.Errorf("reset loose end scan: %w", err)
	}
	return nil
}

func (s *Store) execOne(ctx context.Context, action, query string, args ...any) (bool, error) {
	count, err := s.execCount(ctx, action, query, args...)
	return count == 1, err
}

func (s *Store) execCount(ctx context.Context, action, query string, args ...any) (int, error) {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	return int(affected), nil
}

func (s *Store) queryLooseEnds(ctx context.Context, query string, args ...any) ([]looseend.Item, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query loose ends: %w", err)
	}
	defer rows.Close()
	var items []looseend.Item
	for rows.Next() {
		var item looseend.Item
		var dueAt, createdAt, updatedAt string
		var allDay, outside int
		if err := rows.Scan(&item.ID, &item.PrincipalID, &item.Kind, &item.Title, &item.Counterpart, &dueAt, &allDay, &item.NudgeOn, &item.NudgedOn,
			&item.Source, &item.Account, &item.SourceRef, &item.ThreadRef, &outside, &item.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan loose end: %w", err)
		}
		item.DueAllDay, item.Outside = allDay == 1, outside == 1
		for _, field := range []struct {
			value  string
			target *time.Time
		}{{dueAt, &item.DueAt}, {createdAt, &item.CreatedAt}, {updatedAt, &item.UpdatedAt}} {
			if *field.target, err = time.Parse(time.RFC3339Nano, field.value); err != nil {
				return nil, fmt.Errorf("parse loose end time: %w", err)
			}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query loose ends: %w", err)
	}
	return items, nil
}
