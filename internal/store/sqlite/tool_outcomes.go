package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/memory"
)

// RecordToolOutcome counts one call of the principal's tool that ended with the outcome, on the
// call's UTC day.
func (s *Store) RecordToolOutcome(ctx context.Context, principalID, tool, outcome string, at time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record tool outcome: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO tool_outcome_counts (principal_id, day, tool, outcome, count) VALUES (?, ?, ?, ?, 1)
		 ON CONFLICT (principal_id, day, tool, outcome) DO UPDATE SET count = count + 1`,
		principalID,
		at.UTC().Format(time.DateOnly),
		tool,
		outcome,
	); err != nil {
		return fmt.Errorf("record tool outcome: %w", err)
	}
	return nil
}

// ToolOutcomes sums the principal's tool outcomes over the UTC days from since's on, by tool and
// outcome.
func (s *Store) ToolOutcomes(ctx context.Context, principalID string, since time.Time) ([]memory.ToolOutcome, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list tool outcomes: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT tool, outcome, SUM(count) FROM tool_outcome_counts WHERE principal_id = ? AND day >= ?
		 GROUP BY tool, outcome ORDER BY tool, outcome`,
		principalID,
		since.UTC().Format(time.DateOnly),
	)
	if err != nil {
		return nil, fmt.Errorf("list tool outcomes: %w", err)
	}
	defer rows.Close()
	var outcomes []memory.ToolOutcome
	for rows.Next() {
		var outcome memory.ToolOutcome
		if err := rows.Scan(&outcome.Tool, &outcome.Outcome, &outcome.Count); err != nil {
			return nil, fmt.Errorf("scan tool outcome: %w", err)
		}
		outcomes = append(outcomes, outcome)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tool outcomes: %w", err)
	}
	return outcomes, nil
}

// PruneToolOutcomes removes every principal's counts of UTC days before before's.
func (s *Store) PruneToolOutcomes(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("prune tool outcomes: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tool_outcome_counts WHERE day < ?`, before.UTC().Format(time.DateOnly)); err != nil {
		return fmt.Errorf("prune tool outcomes: %w", err)
	}
	return nil
}
