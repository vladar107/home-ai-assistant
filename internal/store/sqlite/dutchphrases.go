package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/dutchphrase"
)

var _ dutchphrase.Repository = (*Store)(nil)

func (s *Store) LoadDutchPhrase(ctx context.Context, principalID, localDate string) (dutchphrase.Phrase, bool, error) {
	if s == nil || s.db == nil {
		return dutchphrase.Phrase{}, false, fmt.Errorf("load Dutch phrase: SQLite store is not open")
	}
	phrase := dutchphrase.Phrase{PrincipalID: principalID, LocalDate: localDate}
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT phrase, normalized, usage, translation, usage_translation, note, created_at
		 FROM dutch_phrases WHERE principal_id = ? AND local_date = ?`,
		principalID, localDate,
	).Scan(&phrase.Phrase, &phrase.Normalized, &phrase.Usage, &phrase.Translation, &phrase.UsageTranslation, &phrase.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return dutchphrase.Phrase{}, false, nil
	}
	if err != nil {
		return dutchphrase.Phrase{}, false, fmt.Errorf("load Dutch phrase: %w", err)
	}
	if phrase.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return dutchphrase.Phrase{}, false, fmt.Errorf("parse Dutch phrase creation time: %w", err)
	}
	return phrase, true, nil
}

func (s *Store) RecentDutchPhrases(ctx context.Context, principalID string, limit int) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list Dutch phrases: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT phrase FROM dutch_phrases WHERE principal_id = ? ORDER BY local_date DESC LIMIT ?`,
		principalID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Dutch phrases: %w", err)
	}
	defer rows.Close()
	var phrases []string
	for rows.Next() {
		var phrase string
		if err := rows.Scan(&phrase); err != nil {
			return nil, fmt.Errorf("read Dutch phrase: %w", err)
		}
		phrases = append(phrases, phrase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Dutch phrases: %w", err)
	}
	return phrases, nil
}

func (s *Store) SentDutchPhrases(ctx context.Context, principalID string, normalized []string) (map[string]bool, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("check Dutch phrases: SQLite store is not open")
	}
	sent := make(map[string]bool, len(normalized))
	if len(normalized) == 0 {
		return sent, nil
	}
	args := make([]any, 0, len(normalized)+1)
	args = append(args, principalID)
	for _, value := range normalized {
		args = append(args, value)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT normalized FROM dutch_phrases WHERE principal_id = ? AND normalized IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(normalized)), ", ")+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("check Dutch phrases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("read Dutch phrase: %w", err)
		}
		sent[value] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check Dutch phrases: %w", err)
	}
	return sent, nil
}

func (s *Store) InsertDutchPhrase(ctx context.Context, phrase dutchphrase.Phrase) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("save Dutch phrase: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO dutch_phrases (principal_id, local_date, phrase, normalized, usage, translation, usage_translation, note, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT DO NOTHING`,
		phrase.PrincipalID, phrase.LocalDate, phrase.Phrase, phrase.Normalized, phrase.Usage, phrase.Translation, phrase.UsageTranslation, phrase.Note, formatTime(phrase.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("save Dutch phrase: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save Dutch phrase: %w", err)
	}
	return inserted == 1, nil
}

// HasDutchPhrases reports whether the principal was ever sent a Dutch phrase.
func (s *Store) HasDutchPhrases(ctx context.Context, principalID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("check Dutch phrases: SQLite store is not open")
	}
	var held bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM dutch_phrases WHERE principal_id = ?)`, principalID).Scan(&held); err != nil {
		return false, fmt.Errorf("check Dutch phrases: %w", err)
	}
	return held, nil
}
