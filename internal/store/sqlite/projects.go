package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/project"
)

var _ project.Repository = (*Store)(nil)

const projectColumns = `id, principal_id, title, goal, status, COALESCE(summary_message_id, 0), created_at, updated_at`

func (s *Store) CreateProject(ctx context.Context, p project.Project, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create project: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin project creation: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = ?)`, p.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check project: %w", err)
	}
	if exists {
		return false, nil
	}
	if err := checkOpenProjects(ctx, tx, p.PrincipalID, p.ID, limit); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO projects (id, principal_id, title, goal, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.PrincipalID, p.Title, p.Goal, p.Status, formatTime(p.CreatedAt), formatTime(p.UpdatedAt),
	); err != nil {
		return false, fmt.Errorf("insert project: %w", err)
	}
	if err := insertProjectParts(ctx, tx, p); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit project creation: %w", err)
	}
	return true, nil
}

// checkOpenProjects refuses another open project when the principal already keeps limit open ones
// besides the given one.
func checkOpenProjects(ctx context.Context, tx *sql.Tx, principalID, projectID string, limit int) error {
	var open int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM projects WHERE principal_id = ? AND id <> ? AND status IN ('active', 'paused')`,
		principalID, projectID,
	).Scan(&open); err != nil {
		return fmt.Errorf("count open projects: %w", err)
	}
	if open >= limit {
		return project.ErrTooManyProjects
	}
	return nil
}

func insertProjectParts(ctx context.Context, tx *sql.Tx, p project.Project) error {
	for position, item := range p.Items {
		var doneAt any
		if item.Done() {
			doneAt = formatTime(item.DoneAt)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO project_items (principal_id, project_id, id, position, text, done_at, due_on, note) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
			p.PrincipalID, p.ID, item.ID, position, item.Text, doneAt, item.DueOn, item.Note,
		); err != nil {
			return fmt.Errorf("insert project item: %w", err)
		}
	}
	for position, link := range p.Links {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO project_links (principal_id, project_id, position, kind, reference, label) VALUES (?, ?, ?, ?, ?, ?)`,
			p.PrincipalID, p.ID, position, link.Kind, link.Reference, link.Label,
		); err != nil {
			return fmt.Errorf("insert project link: %w", err)
		}
	}
	return nil
}

func (s *Store) SaveProject(ctx context.Context, p project.Project, expectedUpdatedAt time.Time, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("save project: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin project save: %w", err)
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(
		ctx,
		`SELECT status FROM projects WHERE id = ? AND principal_id = ? AND updated_at = ?`,
		p.ID, p.PrincipalID, formatTime(expectedUpdatedAt),
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load project to save: %w", err)
	}
	if project.Open(p.Status) && !project.Open(status) {
		if err := checkOpenProjects(ctx, tx, p.PrincipalID, p.ID, limit); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE projects SET title = ?, goal = ?, status = ?, updated_at = ? WHERE id = ? AND principal_id = ?`,
		p.Title, p.Goal, p.Status, formatTime(p.UpdatedAt), p.ID, p.PrincipalID,
	); err != nil {
		return false, fmt.Errorf("update project: %w", err)
	}
	for _, table := range []string{"project_items", "project_links"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE principal_id = ? AND project_id = ?`, p.PrincipalID, p.ID); err != nil {
			return false, fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if err := insertProjectParts(ctx, tx, p); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit project save: %w", err)
	}
	return true, nil
}

func (s *Store) ListProjects(ctx context.Context, principalID string) ([]project.Project, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list projects: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE principal_id = ? ORDER BY created_at, id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var projects []project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close projects: %w", err)
	}
	byID := make(map[string]*project.Project, len(projects))
	for index := range projects {
		byID[projects[index].ID] = &projects[index]
	}
	if err := s.loadProjectParts(ctx, principalID, "", byID); err != nil {
		return nil, err
	}
	return projects, nil
}

func (s *Store) CountProjects(ctx context.Context, principalID string) (int, int, error) {
	if s == nil || s.db == nil {
		return 0, 0, fmt.Errorf("count projects: SQLite store is not open")
	}
	var total, open int
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(status IN ('active', 'paused')), 0) FROM projects WHERE principal_id = ?`,
		principalID,
	).Scan(&total, &open); err != nil {
		return 0, 0, fmt.Errorf("count projects: %w", err)
	}
	return total, open, nil
}

func (s *Store) LoadProject(ctx context.Context, principalID, projectID string) (project.Project, bool, error) {
	if s == nil || s.db == nil {
		return project.Project{}, false, fmt.Errorf("load project: SQLite store is not open")
	}
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ? AND principal_id = ?`, projectID, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return project.Project{}, false, nil
	}
	if err != nil {
		return project.Project{}, false, err
	}
	if err := s.loadProjectParts(ctx, principalID, projectID, map[string]*project.Project{p.ID: &p}); err != nil {
		return project.Project{}, false, err
	}
	return p, true, nil
}

func (s *Store) SetProjectSummary(ctx context.Context, principalID, projectID string, messageID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set project summary: SQLite store is not open")
	}
	var value any
	if messageID > 0 {
		value = messageID
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET summary_message_id = ? WHERE id = ? AND principal_id = ?`,
		value, projectID, principalID,
	); err != nil {
		return fmt.Errorf("set project summary: %w", err)
	}
	return nil
}

// loadProjectParts fills in the items and links of the principal's projects, in two queries: one
// project's when projectID is set, and every project's otherwise.
func (s *Store) loadProjectParts(ctx context.Context, principalID, projectID string, projects map[string]*project.Project) error {
	filter, arguments := `principal_id = ?`, []any{principalID}
	if projectID != "" {
		filter, arguments = filter+` AND project_id = ?`, append(arguments, projectID)
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT project_id, id, text, COALESCE(done_at, ''), COALESCE(due_on, ''), note FROM project_items
		 WHERE `+filter+` ORDER BY project_id, position`,
		arguments...,
	)
	if err != nil {
		return fmt.Errorf("list project items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner, doneAt string
		var item project.Item
		if err := rows.Scan(&owner, &item.ID, &item.Text, &doneAt, &item.DueOn, &item.Note); err != nil {
			return fmt.Errorf("scan project item: %w", err)
		}
		if doneAt != "" {
			if item.DoneAt, err = time.Parse(time.RFC3339Nano, doneAt); err != nil {
				return fmt.Errorf("parse project item done time: %w", err)
			}
		}
		if p := projects[owner]; p != nil {
			p.Items = append(p.Items, item)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate project items: %w", err)
	}
	links, err := s.db.QueryContext(
		ctx,
		`SELECT project_id, kind, reference, label FROM project_links WHERE `+filter+` ORDER BY project_id, position`,
		arguments...,
	)
	if err != nil {
		return fmt.Errorf("list project links: %w", err)
	}
	defer links.Close()
	for links.Next() {
		var owner string
		var link project.Link
		if err := links.Scan(&owner, &link.Kind, &link.Reference, &link.Label); err != nil {
			return fmt.Errorf("scan project link: %w", err)
		}
		if p := projects[owner]; p != nil {
			p.Links = append(p.Links, link)
		}
	}
	if err := links.Err(); err != nil {
		return fmt.Errorf("iterate project links: %w", err)
	}
	return nil
}

func scanProject(row rowScanner) (project.Project, error) {
	var p project.Project
	var createdAt, updatedAt string
	if err := row.Scan(&p.ID, &p.PrincipalID, &p.Title, &p.Goal, &p.Status, &p.SummaryMessageID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return project.Project{}, err
		}
		return project.Project{}, fmt.Errorf("scan project: %w", err)
	}
	var err error
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return project.Project{}, fmt.Errorf("parse project creation time: %w", err)
	}
	if p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return project.Project{}, fmt.Errorf("parse project change time: %w", err)
	}
	return p, nil
}
