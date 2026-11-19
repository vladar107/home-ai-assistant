package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/assistantmail"
)

var _ assistantmail.Repository = (*Store)(nil)

const mailRouteColumns = `gmail_id, principal_id, thread_id, direction, internal_date, spam, recorded_at, notified_at`

func scanMailRoute(row rowScanner) (assistantmail.Route, error) {
	var route assistantmail.Route
	var direction, internal, recorded string
	var spam int
	var notified sql.NullString
	if err := row.Scan(&route.GmailID, &route.PrincipalID, &route.ThreadID, &direction, &internal, &spam, &recorded, &notified); err != nil {
		return assistantmail.Route{}, err
	}
	route.Direction, route.Spam = assistantmail.Direction(direction), spam == 1
	var err error
	if route.InternalDate, err = time.Parse(time.RFC3339Nano, internal); err != nil {
		return assistantmail.Route{}, fmt.Errorf("parse mail internal date: %w", err)
	}
	if route.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded); err != nil {
		return assistantmail.Route{}, fmt.Errorf("parse mail record time: %w", err)
	}
	if route.NotifiedAt, err = parseOptionalTime(notified); err != nil {
		return assistantmail.Route{}, err
	}
	return route, nil
}

func (s *Store) RecordSeen(ctx context.Context, gmailID string, internalDate time.Time, routes []assistantmail.Route) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("record mail: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin record mail: %w", err)
	}
	defer tx.Rollback()
	recorded := time.Now()
	for _, route := range routes {
		if !route.RecordedAt.IsZero() {
			recorded = route.RecordedAt
			break
		}
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO seen_mail (gmail_id, internal_date, recorded_at) VALUES (?, ?, ?) ON CONFLICT (gmail_id) DO NOTHING`,
		gmailID, formatTime(internalDate), formatTime(recorded))
	if err != nil {
		return false, fmt.Errorf("insert seen mail: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 0 {
		return false, err
	}
	for _, route := range routes {
		if route.RecordedAt.IsZero() {
			route.RecordedAt = recorded
		}
		spam := 0
		if route.Spam {
			spam = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mail_routes (gmail_id, principal_id, thread_id, direction, internal_date, spam, recorded_at, notified_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (gmail_id, principal_id) DO NOTHING`,
			gmailID, route.PrincipalID, route.ThreadID, string(route.Direction), formatTime(internalDate), spam,
			formatTime(route.RecordedAt), nullableTime(route.NotifiedAt)); err != nil {
			return false, fmt.Errorf("insert mail route: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit record mail: %w", err)
	}
	return true, nil
}

func (s *Store) RecordSent(ctx context.Context, gmailID, threadID, principalID string, internalDate, at time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record sent mail: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record sent mail: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO seen_mail (gmail_id, internal_date, recorded_at) VALUES (?, ?, ?) ON CONFLICT (gmail_id) DO NOTHING`,
		gmailID, formatTime(internalDate), formatTime(at)); err != nil {
		return fmt.Errorf("insert seen sent mail: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mail_routes (gmail_id, principal_id, thread_id, direction, internal_date, spam, recorded_at)
		 VALUES (?, ?, ?, 'out', ?, 0, ?) ON CONFLICT (gmail_id, principal_id) DO NOTHING`,
		gmailID, principalID, threadID, formatTime(internalDate), formatTime(at)); err != nil {
		return fmt.Errorf("insert sent mail route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record sent mail: %w", err)
	}
	return nil
}

func (s *Store) PrincipalsForThread(ctx context.Context, threadID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT principal_id FROM mail_routes WHERE thread_id = ? AND direction = 'out' ORDER BY principal_id`, threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread principals: %w", err)
	}
	defer rows.Close()
	var principals []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan thread principal: %w", err)
		}
		principals = append(principals, id)
	}
	return principals, rows.Err()
}

func (s *Store) NewestSeenInternalDate(ctx context.Context) (time.Time, bool, error) {
	var newest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT max(internal_date) FROM seen_mail`).Scan(&newest); err != nil {
		return time.Time{}, false, fmt.Errorf("read newest seen mail: %w", err)
	}
	if !newest.Valid {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, newest.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse newest seen mail: %w", err)
	}
	return parsed, true, nil
}

func (s *Store) Seen(ctx context.Context, gmailID string) (bool, error) {
	var seen bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM seen_mail WHERE gmail_id = ?)`, gmailID).Scan(&seen); err != nil {
		return false, fmt.Errorf("read seen mail: %w", err)
	}
	return seen, nil
}

func (s *Store) HistoryCursor(ctx context.Context) (string, bool, error) {
	var historyID string
	err := s.db.QueryRowContext(ctx, `SELECT history_id FROM mail_cursor WHERE id = 1`).Scan(&historyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read mail history cursor: %w", err)
	}
	return historyID, true, nil
}

func (s *Store) HistoryCursorSavedAt(ctx context.Context) (time.Time, bool, error) {
	var savedAt string
	err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM mail_cursor WHERE id = 1`).Scan(&savedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read mail history cursor time: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, savedAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse mail history cursor time: %w", err)
	}
	return parsed, true, nil
}

func (s *Store) SaveHistoryCursor(ctx context.Context, historyID string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mail_cursor (id, history_id, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET history_id = excluded.history_id, updated_at = excluded.updated_at`,
		historyID, formatTime(s.clock())); err != nil {
		return fmt.Errorf("save mail history cursor: %w", err)
	}
	return nil
}

func (s *Store) queryMailRoutes(ctx context.Context, query string, args ...any) ([]assistantmail.Route, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list mail routes: %w", err)
	}
	defer rows.Close()
	var routes []assistantmail.Route
	for rows.Next() {
		route, err := scanMailRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (s *Store) PendingNotices(ctx context.Context, principalID string, limit int) ([]assistantmail.Route, error) {
	return s.queryMailRoutes(ctx,
		`SELECT `+mailRouteColumns+` FROM mail_routes
		 WHERE principal_id = ? AND direction = 'in' AND notified_at IS NULL
		 ORDER BY internal_date, gmail_id LIMIT ?`, principalID, limit)
}

func (s *Store) MarkNotified(ctx context.Context, principalID string, gmailIDs []string, at time.Time) error {
	if len(gmailIDs) == 0 {
		return nil
	}
	args := []any{formatTime(at), principalID}
	for _, id := range gmailIDs {
		args = append(args, id)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mail_routes SET notified_at = ? WHERE principal_id = ? AND notified_at IS NULL AND gmail_id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(gmailIDs)), ",")+`)`, args...); err != nil {
		return fmt.Errorf("mark mail notified: %w", err)
	}
	return nil
}

func (s *Store) MarkNotifiedBefore(ctx context.Context, principalID string, before time.Time, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mail_routes SET notified_at = ?
		 WHERE principal_id = ? AND direction = 'in' AND notified_at IS NULL AND internal_date < ?`,
		formatTime(at), principalID, formatTime(before)); err != nil {
		return fmt.Errorf("mark mail notified before: %w", err)
	}
	return nil
}

func (s *Store) ListMailRoutes(ctx context.Context, principalID string, limit int) ([]assistantmail.Route, error) {
	return s.queryMailRoutes(ctx,
		`SELECT `+mailRouteColumns+` FROM mail_routes WHERE principal_id = ?
		 ORDER BY internal_date DESC, gmail_id DESC LIMIT ?`, principalID, limit)
}

func (s *Store) Route(ctx context.Context, principalID, gmailID string) (assistantmail.Route, bool, error) {
	route, err := scanMailRoute(s.db.QueryRowContext(ctx,
		`SELECT `+mailRouteColumns+` FROM mail_routes WHERE principal_id = ? AND gmail_id = ?`, principalID, gmailID))
	if errors.Is(err, sql.ErrNoRows) {
		return assistantmail.Route{}, false, nil
	}
	if err != nil {
		return assistantmail.Route{}, false, fmt.Errorf("load mail route: %w", err)
	}
	return route, true, nil
}

func (s *Store) MailSwitchState(ctx context.Context, principalID string) (assistantmail.SwitchState, bool, error) {
	var enabled int
	var onSince, changedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, on_since, switch_changed_at FROM mail_switch_states WHERE principal_id = ?`, principalID,
	).Scan(&enabled, &onSince, &changedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assistantmail.SwitchState{}, false, nil
	}
	if err != nil {
		return assistantmail.SwitchState{}, false, fmt.Errorf("load mail switch state: %w", err)
	}
	state := assistantmail.SwitchState{Enabled: enabled == 1}
	if state.OnSince, err = parseOptionalTime(onSince); err != nil {
		return assistantmail.SwitchState{}, false, err
	}
	if state.ChangedAt, err = parseOptionalTime(changedAt); err != nil {
		return assistantmail.SwitchState{}, false, err
	}
	return state, true, nil
}

func (s *Store) SaveMailSwitchState(ctx context.Context, principalID string, state assistantmail.SwitchState, at time.Time) error {
	enabled := 0
	if state.Enabled {
		enabled = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mail_switch_states (principal_id, enabled, on_since, switch_changed_at, observed_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET enabled = excluded.enabled, on_since = excluded.on_since,
		     switch_changed_at = excluded.switch_changed_at, observed_at = excluded.observed_at`,
		principalID, enabled, nullableTime(state.OnSince), nullableTime(state.ChangedAt), formatTime(at)); err != nil {
		return fmt.Errorf("save mail switch state: %w", err)
	}
	return nil
}

// StartSend relies on the store's immediate transactions: the write lock is taken at BEGIN, so the
// count and the insert of concurrent starts are serialized.
func (s *Store) StartSend(ctx context.Context, principalID, sendID string, dayStart, dayEnd time.Time, limit int, at time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin start send: %w", err)
	}
	defer tx.Rollback()
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT principal_id FROM mail_sends WHERE send_id = ?`, sendID).Scan(&owner)
	if err == nil {
		if owner != principalID {
			return false, fmt.Errorf("start send: send %q belongs to another principal", sendID)
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check mail send: %w", err)
	}
	var sent int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM mail_sends WHERE principal_id = ? AND started_at >= ? AND started_at < ?`,
		principalID, formatTime(dayStart), formatTime(dayEnd)).Scan(&sent); err != nil {
		return false, fmt.Errorf("count mail sends: %w", err)
	}
	if sent >= limit {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mail_sends (send_id, principal_id, started_at) VALUES (?, ?, ?)`,
		sendID, principalID, formatTime(at)); err != nil {
		return false, fmt.Errorf("insert mail send: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit start send: %w", err)
	}
	return true, nil
}

func (s *Store) RecordContacts(ctx context.Context, principalID string, addresses []string, at time.Time) error {
	if len(addresses) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record mail contacts: %w", err)
	}
	defer tx.Rollback()
	for _, address := range addresses {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mail_contacts (principal_id, address, first_sent_at, last_sent_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (principal_id, address) DO UPDATE SET last_sent_at = excluded.last_sent_at`,
			principalID, strings.ToLower(address), formatTime(at), formatTime(at)); err != nil {
			return fmt.Errorf("insert mail contact: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record mail contacts: %w", err)
	}
	return nil
}

func (s *Store) KnownContacts(ctx context.Context, principalID string, addresses []string) ([]string, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	args := []any{principalID}
	for _, address := range addresses {
		args = append(args, strings.ToLower(address))
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT address FROM mail_contacts WHERE principal_id = ? AND address IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(addresses)), ",")+`) ORDER BY address`, args...)
	if err != nil {
		return nil, fmt.Errorf("list mail contacts: %w", err)
	}
	defer rows.Close()
	var known []string
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, fmt.Errorf("scan mail contact: %w", err)
		}
		known = append(known, address)
	}
	return known, rows.Err()
}
