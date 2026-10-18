package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func (s *Store) LaneModel(ctx context.Context, lane conversation.Lane) (agentruntime.ModelEffort, bool, error) {
	if s == nil || s.db == nil {
		return agentruntime.ModelEffort{}, false, fmt.Errorf("load lane model: SQLite store is not open")
	}
	var choice agentruntime.ModelEffort
	err := s.db.QueryRowContext(
		ctx,
		`SELECT model, effort FROM conversation_lane_models WHERE principal_id = ? AND thread_id = ?`,
		lane.PrincipalID,
		lane.ThreadID,
	).Scan(&choice.Model, &choice.Effort)
	if errors.Is(err, sql.ErrNoRows) {
		return agentruntime.ModelEffort{}, false, nil
	}
	if err != nil {
		return agentruntime.ModelEffort{}, false, fmt.Errorf("query lane model: %w", err)
	}
	return choice, true, nil
}

func (s *Store) SaveLaneModel(ctx context.Context, lane conversation.Lane, choice agentruntime.ModelEffort) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save lane model: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO conversation_lane_models (principal_id, thread_id, model, effort)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id, thread_id) DO UPDATE SET
		     model = excluded.model, effort = excluded.effort,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		lane.PrincipalID,
		lane.ThreadID,
		choice.Model,
		choice.Effort,
	); err != nil {
		return fmt.Errorf("save lane model: %w", err)
	}
	return nil
}

func (s *Store) ClearLaneModel(ctx context.Context, lane conversation.Lane) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("clear lane model: SQLite store is not open")
	}
	return clearLaneModel(ctx, s.db, lane)
}

func clearLaneModel(ctx context.Context, db execer, lane conversation.Lane) error {
	if _, err := db.ExecContext(
		ctx,
		`DELETE FROM conversation_lane_models WHERE principal_id = ? AND thread_id = ?`,
		lane.PrincipalID,
		lane.ThreadID,
	); err != nil {
		return fmt.Errorf("clear lane model: %w", err)
	}
	return nil
}
