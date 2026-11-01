package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/collection"
)

var _ collection.Repository = (*Store)(nil)

const (
	collectionColumns = `id, principal_id, name, description, fields, next_key, created_at, updated_at`
	recordColumns     = `id, principal_id, collection_id, field_values, created_at, updated_at`
)

func (s *Store) CreateCollection(ctx context.Context, c collection.Collection, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("create collection: SQLite store is not open")
	}
	fields, err := json.Marshal(c.Fields)
	if err != nil {
		return false, fmt.Errorf("encode collection fields: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin collection creation: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM collections WHERE id = ?)`, c.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check collection: %w", err)
	}
	if exists {
		return false, nil
	}
	names, err := collectionNames(ctx, tx, c.PrincipalID)
	if err != nil {
		return false, err
	}
	if len(names) >= limit {
		return false, collection.ErrTooManyCollections
	}
	for _, name := range names {
		if strings.EqualFold(name, c.Name) {
			return false, collection.ErrNameTaken
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO collections (`+collectionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.PrincipalID, c.Name, c.Description, string(fields), c.NextKey, formatTime(c.CreatedAt), formatTime(c.UpdatedAt),
	); err != nil {
		return false, fmt.Errorf("insert collection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit collection creation: %w", err)
	}
	return true, nil
}

// collectionNames are the principal's collection names, so a name is compared with full case
// folding rather than the index's ASCII-only NOCASE.
func collectionNames(ctx context.Context, tx *sql.Tx, principalID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM collections WHERE principal_id = ?`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list collection names: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan collection name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collection names: %w", err)
	}
	return names, nil
}

func (s *Store) ListCollections(ctx context.Context, principalID string) ([]collection.Collection, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list collections: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+collectionColumns+`, (SELECT COUNT(*) FROM collection_records r WHERE r.principal_id = c.principal_id AND r.collection_id = c.id)
		FROM collections c WHERE principal_id = ? ORDER BY created_at, id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()
	var list []collection.Collection
	for rows.Next() {
		var count int
		c, err := scanCollection(rows, &count)
		if err != nil {
			return nil, err
		}
		c.Records = count
		list = append(list, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collections: %w", err)
	}
	return list, nil
}

func (s *Store) LoadCollection(ctx context.Context, principalID, collectionID string) (collection.Collection, bool, error) {
	if s == nil || s.db == nil {
		return collection.Collection{}, false, fmt.Errorf("load collection: SQLite store is not open")
	}
	var count int
	c, err := scanCollection(s.db.QueryRowContext(ctx,
		`SELECT `+collectionColumns+`, (SELECT COUNT(*) FROM collection_records r WHERE r.principal_id = c.principal_id AND r.collection_id = c.id)
		FROM collections c WHERE id = ? AND principal_id = ?`, collectionID, principalID), &count)
	if errors.Is(err, sql.ErrNoRows) {
		return collection.Collection{}, false, nil
	}
	if err != nil {
		return collection.Collection{}, false, err
	}
	c.Records = count
	return c, true, nil
}

func (s *Store) UpdateCollectionFields(ctx context.Context, c collection.Collection, expectedUpdatedAt time.Time, removedKeys []string, refuseUsed bool) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update collection: SQLite store is not open")
	}
	fields, err := json.Marshal(c.Fields)
	if err != nil {
		return false, fmt.Errorf("encode collection fields: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin collection update: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE collections SET fields = ?, next_key = max(next_key, ?), updated_at = ? WHERE id = ? AND principal_id = ? AND updated_at = ?`,
		string(fields), c.NextKey, formatTime(c.UpdatedAt), c.ID, c.PrincipalID, formatTime(expectedUpdatedAt))
	if err != nil {
		return false, fmt.Errorf("update collection: %w", err)
	}
	updated, err := affectedOne(result, "updated collection")
	if err != nil || !updated {
		return false, err
	}
	for _, key := range removedKeys {
		if refuseUsed {
			var used bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM collection_records WHERE principal_id = ? AND collection_id = ? AND json_type(field_values, ?) IS NOT NULL)`,
				c.PrincipalID, c.ID, `$."`+key+`"`).Scan(&used); err != nil {
				return false, fmt.Errorf("check field values: %w", err)
			}
			if used {
				return false, collection.ErrFieldInUse
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE collection_records SET field_values = json_remove(field_values, ?) WHERE principal_id = ? AND collection_id = ?`,
			`$."`+key+`"`, c.PrincipalID, c.ID); err != nil {
			return false, fmt.Errorf("drop removed field values: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit collection update: %w", err)
	}
	return true, nil
}

func (s *Store) DeleteCollection(ctx context.Context, principalID, collectionID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete collection: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM collections WHERE id = ? AND principal_id = ?`, collectionID, principalID)
	if err != nil {
		return false, fmt.Errorf("delete collection: %w", err)
	}
	return affectedOne(result, "deleted collection")
}

func (s *Store) ListRecords(ctx context.Context, principalID, collectionID string) ([]collection.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list collection records: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordColumns+` FROM collection_records WHERE principal_id = ? AND collection_id = ? ORDER BY created_at, id`,
		principalID, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list collection records: %w", err)
	}
	defer rows.Close()
	var list []collection.Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collection records: %w", err)
	}
	return list, nil
}

func (s *Store) LoadRecord(ctx context.Context, principalID, recordID string) (collection.Record, bool, error) {
	if s == nil || s.db == nil {
		return collection.Record{}, false, fmt.Errorf("load collection record: SQLite store is not open")
	}
	record, err := scanRecord(s.db.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM collection_records WHERE id = ? AND principal_id = ?`, recordID, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return collection.Record{}, false, nil
	}
	if err != nil {
		return collection.Record{}, false, err
	}
	return record, true, nil
}

func (s *Store) CreateRecords(ctx context.Context, records []collection.Record, limit int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("create collection records: SQLite store is not open")
	}
	if len(records) == 0 {
		return 0, nil
	}
	principalID, collectionID := records[0].PrincipalID, records[0].CollectionID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin record creation: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM collections WHERE id = ? AND principal_id = ?)`, collectionID, principalID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("check collection: %w", err)
	}
	if !exists {
		return 0, collection.ErrCollectionGone
	}
	var fresh []collection.Record
	for _, record := range records {
		if record.PrincipalID != principalID || record.CollectionID != collectionID {
			return 0, fmt.Errorf("create collection records: the records belong to more than one collection")
		}
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM collection_records WHERE id = ?)`, record.ID).Scan(&taken); err != nil {
			return 0, fmt.Errorf("check collection record: %w", err)
		}
		if !taken {
			fresh = append(fresh, record)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_records WHERE principal_id = ? AND collection_id = ?`, principalID, collectionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count collection records: %w", err)
	}
	if count+len(fresh) > limit {
		return 0, collection.ErrTooManyRecords
	}
	for _, record := range fresh {
		values, err := json.Marshal(record.Values)
		if err != nil {
			return 0, fmt.Errorf("encode collection record: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collection_records (`+recordColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
			record.ID, principalID, collectionID, string(values), formatTime(record.CreatedAt), formatTime(record.UpdatedAt),
		); err != nil {
			return 0, fmt.Errorf("insert collection record: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit record creation: %w", err)
	}
	return len(fresh), nil
}

func (s *Store) UpdateRecord(ctx context.Context, record collection.Record, expectedUpdatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("update collection record: SQLite store is not open")
	}
	values, err := json.Marshal(record.Values)
	if err != nil {
		return false, fmt.Errorf("encode collection record: %w", err)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE collection_records SET field_values = ?, updated_at = ? WHERE id = ? AND principal_id = ? AND updated_at = ?`,
		string(values), formatTime(record.UpdatedAt), record.ID, record.PrincipalID, formatTime(expectedUpdatedAt))
	if err != nil {
		return false, fmt.Errorf("update collection record: %w", err)
	}
	return affectedOne(result, "updated collection record")
}

func (s *Store) DeleteRecord(ctx context.Context, principalID, recordID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete collection record: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM collection_records WHERE id = ? AND principal_id = ?`, recordID, principalID)
	if err != nil {
		return false, fmt.Errorf("delete collection record: %w", err)
	}
	return affectedOne(result, "deleted collection record")
}

func (s *Store) DeleteUnchangedRecords(ctx context.Context, principalID string, records map[string]time.Time) (int, bool, error) {
	if s == nil || s.db == nil {
		return 0, false, fmt.Errorf("delete collection records: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin record deletion: %w", err)
	}
	defer tx.Rollback()
	deleted := 0
	for id, updatedAt := range records {
		var stored string
		err := tx.QueryRowContext(ctx, `SELECT updated_at FROM collection_records WHERE id = ? AND principal_id = ?`, id, principalID).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return 0, false, fmt.Errorf("check collection record: %w", err)
		case stored != formatTime(updatedAt):
			return 0, false, nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM collection_records WHERE id = ? AND principal_id = ?`, id, principalID); err != nil {
			return 0, false, fmt.Errorf("delete collection record: %w", err)
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit record deletion: %w", err)
	}
	return deleted, true, nil
}

func scanCollection(row rowScanner, extra ...any) (collection.Collection, error) {
	var c collection.Collection
	var fields, createdAt, updatedAt string
	if err := row.Scan(append([]any{&c.ID, &c.PrincipalID, &c.Name, &c.Description, &fields, &c.NextKey, &createdAt, &updatedAt}, extra...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return collection.Collection{}, err
		}
		return collection.Collection{}, fmt.Errorf("scan collection: %w", err)
	}
	if err := json.Unmarshal([]byte(fields), &c.Fields); err != nil {
		return collection.Collection{}, fmt.Errorf("decode collection fields: %w", err)
	}
	var err error
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return collection.Collection{}, fmt.Errorf("parse collection creation time: %w", err)
	}
	if c.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return collection.Collection{}, fmt.Errorf("parse collection update time: %w", err)
	}
	return c, nil
}

func scanRecord(row rowScanner) (collection.Record, error) {
	var record collection.Record
	var values, createdAt, updatedAt string
	if err := row.Scan(&record.ID, &record.PrincipalID, &record.CollectionID, &values, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return collection.Record{}, err
		}
		return collection.Record{}, fmt.Errorf("scan collection record: %w", err)
	}
	if err := json.Unmarshal([]byte(values), &record.Values); err != nil {
		return collection.Record{}, fmt.Errorf("decode collection record: %w", err)
	}
	var err error
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return collection.Record{}, fmt.Errorf("parse collection record creation time: %w", err)
	}
	if record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return collection.Record{}, fmt.Errorf("parse collection record update time: %w", err)
	}
	return record, nil
}
