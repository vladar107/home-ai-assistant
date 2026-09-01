package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL UNIQUE CHECK (length(name) > 0),
    checksum TEXT NOT NULL CHECK (length(checksum) = 64),
    applied_at TEXT NOT NULL
) STRICT;
`

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationFilenamePattern = regexp.MustCompile(`^([0-9]{3,})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

type appliedMigration struct {
	name     string
	checksum string
}

func migrate(ctx context.Context, db *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadAppliedMigrations(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrations(migrations, applied); err != nil {
		return err
	}

	for _, migration := range migrations {
		if _, exists := applied[migration.version]; exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("apply migration %q: %w", migration.name, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			migration.version,
			migration.name,
			migration.checksum,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("record migration %q: %w", migration.name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("embedded migration entry %q is a directory", entry.Name())
		}
		matches := migrationFilenamePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("parse migration version %q: %w", matches[1], err)
		}
		if version != len(migrations)+1 {
			return nil, fmt.Errorf("migration %q has version %d, want %d", entry.Name(), version, len(migrations)+1)
		}

		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		checksum := sha256.Sum256(contents)
		migrations = append(migrations, migration{
			version:  version,
			name:     entry.Name(),
			checksum: hex.EncodeToString(checksum[:]),
			sql:      string(contents),
		})
	}

	if len(migrations) == 0 {
		return nil, fmt.Errorf("no embedded migrations found")
	}
	return migrations, nil
}

func loadAppliedMigrations(ctx context.Context, tx *sql.Tx) (map[int]appliedMigration, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]appliedMigration)
	for rows.Next() {
		var version int
		var record appliedMigration
		if err := rows.Scan(&version, &record.name, &record.checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func validateAppliedMigrations(migrations []migration, applied map[int]appliedMigration) error {
	if len(applied) > len(migrations) {
		return fmt.Errorf("database has %d migrations but binary contains %d", len(applied), len(migrations))
	}

	pendingSeen := false
	for _, migration := range migrations {
		record, exists := applied[migration.version]
		if !exists {
			pendingSeen = true
			continue
		}
		if pendingSeen {
			return fmt.Errorf("migration history is not sequential at version %d", migration.version)
		}
		if record.name != migration.name {
			return fmt.Errorf("migration version %d name mismatch: database has %q, binary has %q", migration.version, record.name, migration.name)
		}
		if record.checksum != migration.checksum {
			return fmt.Errorf("migration %q checksum mismatch", migration.name)
		}
	}

	for version := range applied {
		if version < 1 || version > len(migrations) {
			return fmt.Errorf("database contains unknown migration version %d", version)
		}
	}
	return nil
}
