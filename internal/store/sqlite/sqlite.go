package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxOpenConnections = 4
	maxIdleConnections = 2
	connectionIdleTime = 5 * time.Minute
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, databasePath string) (*Store, error) {
	absoluteDatabasePath, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path %q: %w", databasePath, err)
	}

	if err := prepareDataDir(filepath.Dir(absoluteDatabasePath)); err != nil {
		return nil, err
	}
	if err := prepareDatabaseFile(absoluteDatabasePath); err != nil {
		return nil, err
	}

	dsn := dataSourceName(absoluteDatabasePath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConnections)
	db.SetMaxIdleConns(maxIdleConnections)
	db.SetConnMaxIdleTime(connectionIdleTime)

	store := &Store{db: db}
	if err := store.Ready(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to SQLite database: %w", err), closeDatabase(db))
	}
	if err := migrate(ctx, db); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate SQLite database: %w", err), closeDatabase(db))
	}

	return store, nil
}

func (s *Store) Ready(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("SQLite store is not open")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping SQLite database: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return closeDatabase(s.db)
}

func closeDatabase(db *sql.DB) error {
	if err := db.Close(); err != nil {
		return fmt.Errorf("close SQLite database: %w", err)
	}
	return nil
}

func prepareDataDir(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("data directory %q must not be a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("data directory %q is not a directory", path)
		}
		if permissions := info.Mode().Perm(); permissions != 0o700 {
			return fmt.Errorf("data directory %q permissions are %04o, want 0700", path, permissions)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect data directory %q: %w", path, err)
	}

	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data directory %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure data directory %q: %w", path, err)
	}
	return nil
}

func prepareDatabaseFile(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("database file %q must not be a symbolic link", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("database file %q is not a regular file", path)
		}
	case errors.Is(err, os.ErrNotExist):
		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return fmt.Errorf("create database file %q: %w", path, createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close database file %q after creation: %w", path, closeErr)
		}
	case err != nil:
		return fmt.Errorf("inspect database file %q: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure database file %q: %w", path, err)
	}
	return nil
}

func dataSourceName(absolutePath string) string {
	query := url.Values{}
	query.Set("_busy_timeout", "5000")
	query.Set("_defensive", "1")
	query.Set("_dqs", "0")
	query.Set("_foreign_keys", "1")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")

	dsn := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absolutePath),
		RawQuery: query.Encode(),
	}
	return dsn.String()
}
