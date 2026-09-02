package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/identity"
)

type TelegramCursor struct {
	LastCommittedUpdateID int64
}

var _ identity.Repository = (*Store)(nil)

func (s *Store) BootstrapConfiguredPrincipals(ctx context.Context, configured []identity.ConfiguredPrincipal) error {
	if len(configured) < 1 || len(configured) > 2 {
		return fmt.Errorf("bootstrap configured principals: got %d principals, want one or two", len(configured))
	}
	if s == nil || s.db == nil {
		return fmt.Errorf("bootstrap configured principals: SQLite store is not open")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin configured principal bootstrap: %w", err)
	}
	defer tx.Rollback()

	for _, principal := range configured {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO principals (id, display_name) VALUES (?, ?) ON CONFLICT (id) DO NOTHING`,
			principal.ID,
			principal.DisplayName,
		); err != nil {
			return fmt.Errorf("insert configured principal %q: %w", principal.ID, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO telegram_identities (telegram_user_id) VALUES (?) ON CONFLICT (telegram_user_id) DO NOTHING`,
			principal.TelegramUserID,
		); err != nil {
			return fmt.Errorf("insert configured Telegram identity: %w", err)
		}

		var existingPrincipalID string
		err := tx.QueryRowContext(
			ctx,
			`SELECT principal_id FROM telegram_allowlist WHERE telegram_user_id = ?`,
			principal.TelegramUserID,
		).Scan(&existingPrincipalID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO telegram_allowlist (telegram_user_id, principal_id) VALUES (?, ?)`,
				principal.TelegramUserID,
				principal.ID,
			); err != nil {
				return fmt.Errorf("allow configured Telegram identity: %w", err)
			}
		case err != nil:
			return fmt.Errorf("read allowlist mapping for Telegram identity: %w", err)
		case existingPrincipalID != principal.ID:
			return fmt.Errorf(
				"configured Telegram identity is already mapped to principal %q, refusing mapping to %q",
				existingPrincipalID,
				principal.ID,
			)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit configured principal bootstrap: %w", err)
	}
	return nil
}

func (s *Store) ObserveTelegramIdentity(ctx context.Context, user identity.TelegramUser) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("observe Telegram identity: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO telegram_identities (telegram_user_id, username, display_name)
		 VALUES (?, NULLIF(?, ''), NULLIF(?, ''))
		 ON CONFLICT (telegram_user_id) DO UPDATE SET
		     username = excluded.username,
		     display_name = excluded.display_name,
		     last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		user.ID,
		user.Username,
		user.DisplayName,
	); err != nil {
		return fmt.Errorf("upsert Telegram identity: %w", err)
	}
	return nil
}

func (s *Store) ResolveActivePrincipal(ctx context.Context, telegramUserID int64) (identity.Authorization, error) {
	if s == nil || s.db == nil {
		return identity.Authorization{}, fmt.Errorf("resolve active principal: SQLite store is not open")
	}

	var principalID sql.NullString
	var principalDisplayName sql.NullString
	var principalStatus sql.NullString
	var allowlistRevokedAt sql.NullString
	err := s.db.QueryRowContext(
		ctx,
		`SELECT allowlist.principal_id, allowlist.revoked_at, principal.display_name, principal.status
		 FROM telegram_identities AS telegram_identity
		 LEFT JOIN telegram_allowlist AS allowlist
		     ON allowlist.telegram_user_id = telegram_identity.telegram_user_id
		 LEFT JOIN principals AS principal
		     ON principal.id = allowlist.principal_id
		 WHERE telegram_identity.telegram_user_id = ?`,
		telegramUserID,
	).Scan(&principalID, &allowlistRevokedAt, &principalDisplayName, &principalStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Authorization{Status: identity.AuthorizationStatusUnknownIdentity}, nil
	}
	if err != nil {
		return identity.Authorization{}, fmt.Errorf("query principal for Telegram identity: %w", err)
	}
	if !principalID.Valid {
		return identity.Authorization{Status: identity.AuthorizationStatusNotAllowlisted}, nil
	}
	if allowlistRevokedAt.Valid {
		return identity.Authorization{Status: identity.AuthorizationStatusAllowlistRevoked}, nil
	}
	if !principalDisplayName.Valid || !principalStatus.Valid {
		return identity.Authorization{}, fmt.Errorf("principal %q referenced by Telegram identity is missing", principalID.String)
	}
	if principalStatus.String == "disabled" {
		return identity.Authorization{Status: identity.AuthorizationStatusPrincipalDisabled}, nil
	}
	if principalStatus.String != "active" {
		return identity.Authorization{}, fmt.Errorf("principal %q has unsupported status %q", principalID.String, principalStatus.String)
	}

	return identity.Authorization{
		Status: identity.AuthorizationStatusAuthorized,
		Principal: identity.Principal{
			ID:          principalID.String,
			DisplayName: principalDisplayName.String,
		},
	}, nil
}

func (s *Store) LoadTelegramCursor(ctx context.Context, botUserID int64) (TelegramCursor, bool, error) {
	if s == nil || s.db == nil {
		return TelegramCursor{}, false, fmt.Errorf("load Telegram cursor: SQLite store is not open")
	}

	var cursor TelegramCursor
	err := s.db.QueryRowContext(
		ctx,
		`SELECT last_committed_update_id FROM telegram_polling_cursors WHERE bot_user_id = ?`,
		botUserID,
	).Scan(&cursor.LastCommittedUpdateID)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramCursor{}, false, nil
	}
	if err != nil {
		return TelegramCursor{}, false, fmt.Errorf("query Telegram cursor: %w", err)
	}
	return cursor, true, nil
}

func (s *Store) AdvanceTelegramCursor(ctx context.Context, botUserID, updateID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("advance Telegram cursor: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO telegram_polling_cursors (bot_user_id, last_committed_update_id)
		 VALUES (?, ?)
		 ON CONFLICT (bot_user_id) DO UPDATE SET
		     last_committed_update_id = excluded.last_committed_update_id,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE excluded.last_committed_update_id > telegram_polling_cursors.last_committed_update_id`,
		botUserID,
		updateID,
	); err != nil {
		return fmt.Errorf("advance Telegram cursor: %w", err)
	}
	return nil
}
