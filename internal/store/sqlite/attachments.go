package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/attachment"
)

var _ attachment.Repository = (*Store)(nil)

const attachmentColumns = `id, principal_id, session_id, request_id, position, kind, file_name, mime_type, size_bytes`

// qualifiedAttachmentColumns names the same columns for a join, where several are ambiguous.
const qualifiedAttachmentColumns = `file.id, file.principal_id, file.session_id, file.request_id,
	file.position, file.kind, file.file_name, file.mime_type, file.size_bytes`

// insertAttachments records one request's attachments inside the transaction that created it.
func insertAttachments(ctx context.Context, tx *sql.Tx, sessionID string, records []attachment.Record) error {
	for _, record := range records {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO conversation_attachments (`+attachmentColumns+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ID,
			record.PrincipalID,
			sessionID,
			record.RequestID,
			record.Position,
			string(record.Kind),
			record.FileName,
			record.MimeType,
			record.SizeBytes,
		); err != nil {
			return fmt.Errorf("save attachment: %w", err)
		}
	}
	return nil
}

func (s *Store) ListRequestAttachments(ctx context.Context, principalID, requestID string) ([]attachment.Record, error) {
	return s.listAttachments(
		ctx,
		`SELECT `+attachmentColumns+` FROM conversation_attachments
		 WHERE principal_id = ? AND request_id = ? ORDER BY position`,
		principalID,
		requestID,
	)
}

func (s *Store) ListSessionAttachments(ctx context.Context, principalID, sessionID string) ([]attachment.Record, error) {
	return s.listAttachments(
		ctx,
		`SELECT `+attachmentColumns+` FROM conversation_attachments
		 WHERE principal_id = ? AND session_id = ? ORDER BY created_at, id`,
		principalID,
		sessionID,
	)
}

func (s *Store) LoadAttachment(ctx context.Context, principalID, id string) (attachment.Record, bool, error) {
	if s == nil || s.db == nil {
		return attachment.Record{}, false, fmt.Errorf("load attachment: SQLite store is not open")
	}
	record, err := scanAttachment(s.db.QueryRowContext(
		ctx,
		`SELECT `+qualifiedAttachmentColumns+` FROM conversation_attachments AS file
		 JOIN conversation_sessions AS session
		   ON session.id = file.session_id AND session.principal_id = file.principal_id
		 WHERE file.principal_id = ? AND file.id = ? AND session.status = 'active'`,
		principalID,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return attachment.Record{}, false, nil
	}
	if err != nil {
		return attachment.Record{}, false, fmt.Errorf("query attachment: %w", err)
	}
	return record, true, nil
}

func (s *Store) ActiveAttachmentSession(ctx context.Context, principalID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("load active conversation session: SQLite store is not open")
	}
	session, found, err := loadActiveConversationSession(ctx, s.db, principalID)
	if err != nil {
		return "", false, err
	}
	return session.ID, found, nil
}

func (s *Store) DeleteSessionAttachments(ctx context.Context, principalID, sessionID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete session attachments: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM conversation_attachments WHERE principal_id = ? AND session_id = ?`,
		principalID,
		sessionID,
	); err != nil {
		return fmt.Errorf("delete session attachments: %w", err)
	}
	return nil
}

// PurgeClosedAttachments drops the rows of every session that is no longer active and reports the
// sessions that remain, so the caller can reconcile what is left on disk.
func (s *Store) PurgeClosedAttachments(ctx context.Context) ([]attachment.SessionRef, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("purge attachments: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM conversation_attachments
		 WHERE NOT EXISTS (
		     SELECT 1 FROM conversation_sessions AS session
		     WHERE session.id = conversation_attachments.session_id
		       AND session.principal_id = conversation_attachments.principal_id
		       AND session.status = 'active'
		 )`,
	); err != nil {
		return nil, fmt.Errorf("purge closed attachments: %w", err)
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT DISTINCT principal_id, session_id FROM conversation_attachments
		 ORDER BY principal_id, session_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list attachment sessions: %w", err)
	}
	defer rows.Close()

	var refs []attachment.SessionRef
	for rows.Next() {
		var ref attachment.SessionRef
		if err := rows.Scan(&ref.PrincipalID, &ref.SessionID); err != nil {
			return nil, fmt.Errorf("scan attachment session: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachment sessions: %w", err)
	}
	return refs, nil
}

func (s *Store) listAttachments(ctx context.Context, query string, arguments ...any) ([]attachment.Record, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list attachments: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()

	var records []attachment.Record
	for rows.Next() {
		var record attachment.Record
		var kind string
		if err := rows.Scan(
			&record.ID,
			&record.PrincipalID,
			&record.SessionID,
			&record.RequestID,
			&record.Position,
			&kind,
			&record.FileName,
			&record.MimeType,
			&record.SizeBytes,
		); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		record.Kind = attachment.Kind(kind)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}
	return records, nil
}

func scanAttachment(row *sql.Row) (attachment.Record, error) {
	var record attachment.Record
	var kind string
	err := row.Scan(
		&record.ID,
		&record.PrincipalID,
		&record.SessionID,
		&record.RequestID,
		&record.Position,
		&kind,
		&record.FileName,
		&record.MimeType,
		&record.SizeBytes,
	)
	record.Kind = attachment.Kind(kind)
	return record, err
}
