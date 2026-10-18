package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// AddTab records a tab the registry does not know yet and reports whether it did; a known tab is
// left as it is.
func (s *Store) AddTab(ctx context.Context, principalID string, threadID int64, name, namedBy string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("add tab: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO telegram_tabs (principal_id, thread_id, name, named_by) VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id, thread_id) DO NOTHING`,
		principalID,
		threadID,
		name,
		namedBy,
	)
	if err != nil {
		return false, fmt.Errorf("add tab: %w", err)
	}
	added, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add tab: %w", err)
	}
	return added > 0, nil
}

// RenameTab records a name the user gave a tab and reports whether the registry changed. A fixed
// tab keeps its name.
func (s *Store) RenameTab(ctx context.Context, principalID string, threadID int64, name string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("rename tab: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO telegram_tabs (principal_id, thread_id, name, named_by) VALUES (?, ?, ?, 'user')
		 ON CONFLICT (principal_id, thread_id) DO UPDATE
		 SET name = excluded.name, named_by = 'user', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE telegram_tabs.named_by <> 'fixed'
		   AND (telegram_tabs.name <> excluded.name OR telegram_tabs.named_by <> 'user')`,
		principalID,
		threadID,
		name,
	)
	if err != nil {
		return false, fmt.Errorf("rename tab: %w", err)
	}
	renamed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rename tab: %w", err)
	}
	return renamed > 0, nil
}

// ConfirmTab records that the tab took a send or a message, unless it already did, and reports
// whether the registry holds the tab as confirmed; a tab it does not know is not.
func (s *Store) ConfirmTab(ctx context.Context, principalID string, threadID int64) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("confirm tab: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE telegram_tabs
		 SET updated_at = CASE WHEN confirmed_at IS NULL THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE updated_at END,
		     confirmed_at = COALESCE(confirmed_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 WHERE principal_id = ? AND thread_id = ?`,
		principalID,
		threadID,
	)
	if err != nil {
		return false, fmt.Errorf("confirm tab: %w", err)
	}
	known, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("confirm tab: %w", err)
	}
	return known > 0, nil
}

func (s *Store) Tab(ctx context.Context, principalID string, threadID int64) (thread.Tab, bool, error) {
	if s == nil || s.db == nil {
		return thread.Tab{}, false, fmt.Errorf("load tab: SQLite store is not open")
	}
	tab := thread.Tab{ThreadID: threadID}
	var created string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT name, named_by, COALESCE(icon_custom_emoji_id, ''), COALESCE(named_session_id, ''), confirmed_at IS NOT NULL, created_at
		 FROM telegram_tabs WHERE principal_id = ? AND thread_id = ?`,
		principalID,
		threadID,
	).Scan(&tab.Name, &tab.NamedBy, &tab.IconCustomEmojiID, &tab.NamedSessionID, &tab.Confirmed, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return thread.Tab{}, false, nil
	}
	if err != nil {
		return thread.Tab{}, false, fmt.Errorf("load tab: %w", err)
	}
	if tab.CreatedAt, err = time.Parse(conversationTimeLayout, created); err != nil {
		return thread.Tab{}, false, fmt.Errorf("load tab: invalid creation time")
	}
	return tab, true, nil
}

// ClaimTabName records that the bot names the tab after the session and reports whether it may.
// Only a tab the bot named may be named by it, and only once per conversation.
func (s *Store) ClaimTabName(ctx context.Context, principalID string, threadID int64, sessionID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("claim tab name: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE telegram_tabs SET named_session_id = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND named_by = 'bot' AND named_session_id IS NOT ?`,
		sessionID,
		principalID,
		threadID,
		sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("claim tab name: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim tab name: %w", err)
	}
	return claimed > 0, nil
}

// NameBotTab records the name and icon the bot gave a tab after the session and reports whether the
// registry changed. An empty icon keeps the tab's icon, as Telegram does. A tab the user named
// meanwhile keeps the user's name, and one a later conversation claimed keeps that one's.
func (s *Store) NameBotTab(ctx context.Context, principalID string, threadID int64, sessionID, name, iconCustomEmojiID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("name tab: SQLite store is not open")
	}
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE telegram_tabs
		 SET name = ?, icon_custom_emoji_id = COALESCE(NULLIF(?, ''), icon_custom_emoji_id),
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND named_by = 'bot' AND named_session_id = ?`,
		name,
		iconCustomEmojiID,
		principalID,
		threadID,
		sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("name tab: %w", err)
	}
	named, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("name tab: %w", err)
	}
	return named > 0, nil
}

// DeleteTab forgets a tab. A trip's tab takes the trip's summary with it, since that message was in
// the tab.
func (s *Store) DeleteTab(ctx context.Context, principalID string, threadID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete tab: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tab delete: %w", err)
	}
	defer tx.Rollback()
	var subject sql.NullString
	err = tx.QueryRowContext(
		ctx,
		`DELETE FROM telegram_tabs WHERE principal_id = ? AND thread_id = ? RETURNING subject`,
		principalID,
		threadID,
	).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete tab: %w", err)
	}
	if tripID, isTrip := thread.TripOf(subject.String); isTrip {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE trips SET summary_message_id = NULL WHERE principal_id = ? AND id = ?`,
			principalID,
			tripID,
		); err != nil {
			return fmt.Errorf("forget trip summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tab delete: %w", err)
	}
	return nil
}

// LaneLastRequest returns when the lane last took a message from its user, if it ever did.
func (s *Store) LaneLastRequest(ctx context.Context, lane conversation.Lane) (time.Time, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, false, fmt.Errorf("load last lane request: SQLite store is not open")
	}
	var last sql.NullString
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT MAX(created_at) FROM conversation_requests WHERE principal_id = ? AND thread_id = ?`,
		lane.PrincipalID,
		lane.ThreadID,
	).Scan(&last); err != nil {
		return time.Time{}, false, fmt.Errorf("load last lane request: %w", err)
	}
	if !last.Valid {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(conversationTimeLayout, last.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("load last lane request: invalid creation time")
	}
	return at, true, nil
}

// SubjectTab returns the tab the registry records as serving the subject, such as a feed.
func (s *Store) SubjectTab(ctx context.Context, principalID, subject string) (int64, bool, error) {
	if s == nil || s.db == nil {
		return 0, false, fmt.Errorf("load subject tab: SQLite store is not open")
	}
	var threadID int64
	err := s.db.QueryRowContext(
		ctx,
		`SELECT thread_id FROM telegram_tabs WHERE principal_id = ? AND subject = ?`,
		principalID,
		subject,
	).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("load subject tab: %w", err)
	}
	return threadID, true, nil
}

// AddSubjectTab records a tab the bot made to serve the subject, under a fixed name.
func (s *Store) AddSubjectTab(ctx context.Context, principalID string, threadID int64, name, subject string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("add subject tab: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO telegram_tabs (principal_id, thread_id, name, named_by, subject) VALUES (?, ?, ?, 'fixed', ?)`,
		principalID,
		threadID,
		name,
		subject,
	); err != nil {
		return fmt.Errorf("add subject tab: %w", err)
	}
	return nil
}

// SetFixedTabName records the name the bot gave a tab of a fixed kind, such as a trip's after its
// title changed.
func (s *Store) SetFixedTabName(ctx context.Context, principalID string, threadID int64, name string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set tab name: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE telegram_tabs
		 SET name = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND named_by = 'fixed'`,
		name,
		principalID,
		threadID,
	); err != nil {
		return fmt.Errorf("set tab name: %w", err)
	}
	return nil
}

// SetFixedTabIcon records the icon the bot gave a tab of a fixed kind, such as a feed.
func (s *Store) SetFixedTabIcon(ctx context.Context, principalID string, threadID int64, iconCustomEmojiID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set tab icon: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE telegram_tabs
		 SET icon_custom_emoji_id = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE principal_id = ? AND thread_id = ? AND named_by = 'fixed'`,
		iconCustomEmojiID,
		principalID,
		threadID,
	); err != nil {
		return fmt.Errorf("set tab icon: %w", err)
	}
	return nil
}
