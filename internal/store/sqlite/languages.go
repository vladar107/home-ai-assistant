package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

var _ locale.Preferences = (*Store)(nil)

func (s *Store) LoadLanguage(ctx context.Context, principalID string) (locale.Language, bool, error) {
	if s == nil || s.db == nil {
		return locale.English, false, fmt.Errorf("load language: SQLite store is not open")
	}
	var code string
	err := s.db.QueryRowContext(ctx, `SELECT language FROM principal_languages WHERE principal_id = ?`, principalID).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return locale.English, false, nil
	}
	if err != nil {
		return locale.English, false, fmt.Errorf("query language: %w", err)
	}
	language, ok := locale.Parse(code)
	if !ok {
		return locale.English, false, fmt.Errorf("query language: stored language %q is unknown", code)
	}
	return language, true, nil
}

func (s *Store) SaveLanguage(ctx context.Context, principalID string, language locale.Language) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save language: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO principal_languages (principal_id, language) VALUES (?, ?)
		 ON CONFLICT (principal_id) DO UPDATE
		 SET language = excluded.language,
		     changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE language != excluded.language`,
		principalID,
		language.String(),
	); err != nil {
		return fmt.Errorf("save language: %w", err)
	}
	return nil
}

func (s *Store) ChosenLanguages(ctx context.Context) (map[string]locale.Language, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list languages: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT principal_id, language FROM principal_languages`)
	if err != nil {
		return nil, fmt.Errorf("query languages: %w", err)
	}
	defer rows.Close()
	chosen := map[string]locale.Language{}
	for rows.Next() {
		var principalID, code string
		if err := rows.Scan(&principalID, &code); err != nil {
			return nil, fmt.Errorf("scan language: %w", err)
		}
		if language, ok := locale.Parse(code); ok {
			chosen[principalID] = language
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate languages: %w", err)
	}
	return chosen, nil
}
