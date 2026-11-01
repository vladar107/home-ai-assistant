package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

var _ export.Store = (*Store)(nil)

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// exportColumn is one column of a table a section exports, with its declared type.
type exportColumn struct {
	name     string
	typeName string
}

// exportColumns returns the table's columns other than principal_id. Only a table that has a
// principal_id column is exported, so every query below is scoped to one principal.
func exportColumns(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]exportColumn, error) {
	if !identifierPattern.MatchString(table) {
		return nil, fmt.Errorf("table name %q is not valid", table)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT name, type FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer rows.Close()
	var columns []exportColumn
	owned := false
	for rows.Next() {
		var column exportColumn
		if err := rows.Scan(&column.name, &column.typeName); err != nil {
			return nil, fmt.Errorf("read columns of %s: %w", table, err)
		}
		if column.name == "principal_id" {
			owned = true
			continue
		}
		if !identifierPattern.MatchString(column.name) {
			return nil, fmt.Errorf("column %s.%s is not exportable", table, column.name)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	if !owned {
		return nil, fmt.Errorf("table %s has no principal_id column", table)
	}
	return columns, nil
}

func (s *Store) ExportRows(ctx context.Context, table, principalID string, omit []string) ([]export.Row, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("export rows: SQLite store is not open")
	}
	all, err := exportColumns(ctx, s.db, table)
	if err != nil {
		return nil, err
	}
	columns := make([]string, 0, len(all))
	for _, column := range all {
		if !slices.Contains(omit, column.name) {
			columns = append(columns, column.name)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+strings.Join(columns, ", ")+` FROM `+table+` WHERE principal_id = ? ORDER BY rowid`, principalID)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", table, err)
	}
	defer rows.Close()
	var result []export.Row
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("read %s: %w", table, err)
		}
		row := make(export.Row, len(columns))
		for index, column := range columns {
			value := values[index]
			if bytes, ok := value.([]byte); ok {
				value = string(bytes)
			}
			row[column] = value
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	return result, nil
}

func (s *Store) IDOwners(ctx context.Context, table, principalID string, ids []string) (map[string]bool, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("check export IDs: SQLite store is not open")
	}
	columns, err := exportColumns(ctx, s.db, table)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(columns, func(column exportColumn) bool { return column.name == "id" }) {
		return nil, fmt.Errorf("table %s has no id column", table)
	}
	owners := make(map[string]bool, len(ids))
	for _, id := range ids {
		var owner string
		err := s.db.QueryRowContext(ctx, `SELECT principal_id FROM `+table+` WHERE id = ?`, id).Scan(&owner)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check %s ID: %w", table, err)
		}
		owners[id] = owner == principalID
	}
	return owners, nil
}

func (s *Store) Columns(ctx context.Context, table string) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("read export columns: SQLite store is not open")
	}
	columns, err := exportColumns(ctx, s.db, table)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.name)
	}
	return names, nil
}

func (s *Store) RestoreRows(ctx context.Context, principalID string, writes []export.Write, limits []export.Limit) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("restore rows: SQLite store is not open")
	}
	if principalID == "" {
		return fmt.Errorf("restore rows: principal is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restore: %w", err)
	}
	defer tx.Rollback()
	known := map[string]map[string]string{}
	for _, write := range writes {
		types, found := known[write.Table]
		if !found {
			columns, err := exportColumns(ctx, tx, write.Table)
			if err != nil {
				return err
			}
			types = make(map[string]string, len(columns))
			for _, column := range columns {
				types[column.name] = strings.ToUpper(column.typeName)
			}
			known[write.Table] = types
		}
		names := make([]string, 0, len(write.Row))
		for name := range write.Row {
			if _, exists := types[name]; !exists {
				return fmt.Errorf("restore %s: the table has no column %q", write.Table, name)
			}
			names = append(names, name)
		}
		sort.Strings(names)
		values := make([]any, 0, len(names)+1)
		values = append(values, principalID)
		for _, name := range names {
			value, err := storedValue(write.Row[name], types[name])
			if err != nil {
				return fmt.Errorf("restore %s.%s: %w", write.Table, name, err)
			}
			values = append(values, value)
		}
		statement := `INSERT INTO ` + write.Table + ` (principal_id`
		if len(names) > 0 {
			statement += `, ` + strings.Join(names, ", ")
		}
		statement += `) VALUES (?` + strings.Repeat(", ?", len(names)) + `)`
		if _, err := tx.ExecContext(ctx, statement, values...); err != nil {
			return fmt.Errorf("restore %s: %w", write.Table, err)
		}
	}
	// The caps are counted inside the transaction, which holds the write lock, so nothing created
	// meanwhile elsewhere can push the principal past one.
	for _, limit := range limits {
		if _, err := exportColumns(ctx, tx, limit.Table); err != nil {
			return err
		}
		where := ""
		if limit.Where != "" {
			where = ` AND (` + limit.Where + `)`
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+limit.Table+` WHERE principal_id = ?`+where, append([]any{principalID}, limit.Args...)...).Scan(&count); err != nil {
			return fmt.Errorf("count %s for its cap: %w", limit.Table, err)
		}
		if count > limit.Max {
			return fmt.Errorf("restore %s: %d rows would pass the cap of %d", limit.Table, count, limit.Max)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restore: %w", err)
	}
	return nil
}

// storedValue turns a value read from a file back into what its column holds.
func storedValue(value any, typeName string) (any, error) {
	switch v := value.(type) {
	case nil, string, int64, float64:
		return v, nil
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	case json.Number:
		if typeName == "INTEGER" {
			return v.Int64()
		}
		if number, err := v.Int64(); err == nil && typeName != "REAL" {
			return number, nil
		}
		return v.Float64()
	default:
		return nil, fmt.Errorf("a value of type %T cannot be stored", value)
	}
}
