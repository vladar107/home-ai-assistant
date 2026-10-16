package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

// Who chose a tab's name. A fixed name never follows the tab's topic.
const (
	TabNamedByBot   = "bot"
	TabNamedByUser  = "user"
	TabNamedByFixed = "fixed"
)

// Tab is one tab of a principal's chat as the registry knows it.
type Tab struct {
	ThreadID  int64
	Name      string
	NamedBy   string
	Confirmed bool
	CreatedAt time.Time
}

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

func (s *Store) Tab(ctx context.Context, principalID string, threadID int64) (Tab, bool, error) {
	if s == nil || s.db == nil {
		return Tab{}, false, fmt.Errorf("load tab: SQLite store is not open")
	}
	tab := Tab{ThreadID: threadID}
	var created string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT name, named_by, confirmed_at IS NOT NULL, created_at FROM telegram_tabs
		 WHERE principal_id = ? AND thread_id = ?`,
		principalID,
		threadID,
	).Scan(&tab.Name, &tab.NamedBy, &tab.Confirmed, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Tab{}, false, nil
	}
	if err != nil {
		return Tab{}, false, fmt.Errorf("load tab: %w", err)
	}
	if tab.CreatedAt, err = time.Parse(conversationTimeLayout, created); err != nil {
		return Tab{}, false, fmt.Errorf("load tab: invalid creation time")
	}
	return tab, true, nil
}

func (s *Store) DeleteTab(ctx context.Context, principalID string, threadID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete tab: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM telegram_tabs WHERE principal_id = ? AND thread_id = ?`,
		principalID,
		threadID,
	); err != nil {
		return fmt.Errorf("delete tab: %w", err)
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
