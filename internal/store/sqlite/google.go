package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

var _ integration.GoogleAccounts = (*Store)(nil)

func (s *Store) SaveGoogleAccount(ctx context.Context, account integration.GoogleAccount) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save Google account: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO google_accounts (
		    principal_id, account_id, address, mail_granted, calendar_granted, mail_enabled, calendar_enabled
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id, account_id) DO UPDATE
		 SET address = excluded.address,
		     mail_granted = excluded.mail_granted,
		     calendar_granted = excluded.calendar_granted,
		     mail_enabled = excluded.mail_enabled,
		     calendar_enabled = excluded.calendar_enabled`,
		account.PrincipalID,
		account.AccountID,
		account.Address,
		boolInt(account.MailGranted),
		boolInt(account.CalendarGranted),
		boolInt(account.MailEnabled),
		boolInt(account.CalendarEnabled),
	); err != nil {
		return fmt.Errorf("save Google account: %w", err)
	}
	return nil
}

func (s *Store) ListGoogleAccounts(ctx context.Context, principalID string) ([]integration.GoogleAccount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list Google accounts: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT account_id, address, mail_granted, calendar_granted, mail_enabled, calendar_enabled, connected_at
		 FROM google_accounts WHERE principal_id = ? ORDER BY connected_at, account_id`,
		principalID,
	)
	if err != nil {
		return nil, fmt.Errorf("list Google accounts: %w", err)
	}
	defer rows.Close()
	var accounts []integration.GoogleAccount
	for rows.Next() {
		account := integration.GoogleAccount{PrincipalID: principalID}
		var mailGranted, calendarGranted, mailEnabled, calendarEnabled int
		var connectedAt string
		if err := rows.Scan(&account.AccountID, &account.Address, &mailGranted, &calendarGranted, &mailEnabled, &calendarEnabled, &connectedAt); err != nil {
			return nil, fmt.Errorf("scan Google account: %w", err)
		}
		account.MailGranted, account.CalendarGranted = mailGranted == 1, calendarGranted == 1
		account.MailEnabled, account.CalendarEnabled = mailEnabled == 1, calendarEnabled == 1
		if account.ConnectedAt, err = time.Parse(time.RFC3339, connectedAt); err != nil {
			return nil, fmt.Errorf("parse Google account timestamp: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Google accounts: %w", err)
	}
	return accounts, nil
}

func (s *Store) DeleteGoogleAccount(ctx context.Context, principalID, accountID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("delete Google account: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM google_accounts WHERE principal_id = ? AND account_id = ?`, principalID, accountID)
	if err != nil {
		return false, fmt.Errorf("delete Google account: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deleted Google account count: %w", err)
	}
	return deleted > 0, nil
}

func (s *Store) GoogleAccountOwner(ctx context.Context, accountID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("find Google account owner: SQLite store is not open")
	}
	var principalID string
	err := s.db.QueryRowContext(ctx, `SELECT principal_id FROM google_accounts WHERE account_id = ?`, accountID).Scan(&principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find Google account owner: %w", err)
	}
	return principalID, true, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
