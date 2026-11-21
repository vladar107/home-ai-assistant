package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	placeholderSuffixAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
	placeholderSuffixLength   = 6
	placeholderSuffixAttempts = 8
)

// PlaceholderEntry is one value in a principal's placeholder vault and the suffix of its token.
type PlaceholderEntry struct {
	Kind   string
	Value  string
	Suffix string
}

// PlaceholderToken returns the suffix of the principal's token for the value, allocating a new
// random one the first time the value is seen, and marks the value seen now.
func (s *Store) PlaceholderToken(ctx context.Context, principalID, kind, value string) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("allocate placeholder: SQLite store is not open")
	}
	now := formatTime(s.clock())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin allocate placeholder: %w", err)
	}
	defer tx.Rollback()

	var suffix string
	err = tx.QueryRowContext(ctx,
		`UPDATE placeholder_values SET last_seen = ? WHERE principal_id = ? AND kind = ? AND value = ? RETURNING suffix`,
		now, principalID, kind, value).Scan(&suffix)
	switch {
	case err == nil:
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("read placeholder: %w", err)
	default:
		if suffix, err = s.insertPlaceholder(ctx, tx, principalID, kind, value, now); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit allocate placeholder: %w", err)
	}
	return suffix, nil
}

func (s *Store) insertPlaceholder(ctx context.Context, tx *sql.Tx, principalID, kind, value, now string) (string, error) {
	for range placeholderSuffixAttempts {
		suffix, err := s.newPlaceholderSuffix()
		if err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx,
			`INSERT INTO placeholder_values (principal_id, kind, value, suffix, first_seen, last_seen)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (principal_id, suffix) DO NOTHING`,
			principalID, kind, value, suffix, now, now)
		if err != nil {
			return "", fmt.Errorf("insert placeholder: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("insert placeholder: %w", err)
		}
		if inserted == 1 {
			return suffix, nil
		}
	}
	return "", fmt.Errorf("allocate placeholder: no free suffix after %d attempts", placeholderSuffixAttempts)
}

func (s *Store) newPlaceholderSuffix() (string, error) {
	random := s.placeholderRandom
	if random == nil {
		random = rand.Reader
	}
	var raw [placeholderSuffixLength]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return "", fmt.Errorf("generate placeholder suffix: %w", err)
	}
	suffix := make([]byte, placeholderSuffixLength)
	for i, b := range raw {
		suffix[i] = placeholderSuffixAlphabet[b%byte(len(placeholderSuffixAlphabet))]
	}
	return string(suffix), nil
}

// PlaceholderValue returns the value behind the principal's token, and marks it seen now. found is
// false for a suffix the principal never had or whose row was pruned.
func (s *Store) PlaceholderValue(ctx context.Context, principalID, kind, suffix string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("resolve placeholder: SQLite store is not open")
	}
	var value string
	err := s.db.QueryRowContext(ctx,
		`UPDATE placeholder_values SET last_seen = ? WHERE principal_id = ? AND kind = ? AND suffix = ? RETURNING value`,
		formatTime(s.clock()), principalID, kind, suffix).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve placeholder: %w", err)
	}
	return value, true, nil
}

// PlaceholderEntries lists the principal's known values of the kind, for replacing them by exact
// match.
func (s *Store) PlaceholderEntries(ctx context.Context, principalID, kind string) ([]PlaceholderEntry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list placeholders: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, value, suffix FROM placeholder_values WHERE principal_id = ? AND kind = ? ORDER BY value`,
		principalID, kind)
	if err != nil {
		return nil, fmt.Errorf("list placeholders: %w", err)
	}
	defer rows.Close()
	var entries []PlaceholderEntry
	for rows.Next() {
		var entry PlaceholderEntry
		if err := rows.Scan(&entry.Kind, &entry.Value, &entry.Suffix); err != nil {
			return nil, fmt.Errorf("scan placeholder: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate placeholders: %w", err)
	}
	return entries, nil
}

// PrunePlaceholders removes every value last seen before the instant, so its token no longer
// resolves.
func (s *Store) PrunePlaceholders(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune placeholders: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM placeholder_values WHERE last_seen < ?`, formatTime(before)); err != nil {
		return fmt.Errorf("prune placeholders: %w", err)
	}
	return nil
}

// DeletePlaceholders removes the principal's whole vault.
func (s *Store) DeletePlaceholders(ctx context.Context, principalID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete placeholders: SQLite store is not open")
	}
	return deletePlaceholders(ctx, s.db, principalID)
}

func deletePlaceholders(ctx context.Context, db execer, principalID string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM placeholder_values WHERE principal_id = ?`, principalID); err != nil {
		return fmt.Errorf("delete placeholders: %w", err)
	}
	return nil
}
