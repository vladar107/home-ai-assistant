// Package delivery sends a file to a principal's own Telegram chat. The chat is derived from the
// allowlist rather than from the request, so a file can only ever reach the principal it belongs
// to, and a revoked or disabled principal has no chat and is sent nothing. The bytes stream from
// the caller to Telegram without being read here.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// ErrNoChat reports that the principal has no chat to deliver to, so nothing was uploaded.
var ErrNoChat = errors.New("principal has no active Telegram chat")

// Chats is the identity mapping as delivery sees it.
type Chats interface {
	ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
}

// Documents is the Telegram client as delivery sees it.
type Documents interface {
	SendDocument(ctx context.Context, chatID int64, name string, body io.Reader, size int64) error
}

// File is one outbound file. Body is always closed by SendFile.
type File struct {
	Name string
	Size int64
	Body io.ReadCloser
}

type Service struct {
	chats     Chats
	documents Documents
	logger    *slog.Logger
}

func NewService(chats Chats, documents Documents, logger *slog.Logger) (*Service, error) {
	if chats == nil {
		return nil, fmt.Errorf("delivery chats are required")
	}
	if documents == nil {
		return nil, fmt.Errorf("delivery documents are required")
	}
	if logger == nil {
		return nil, fmt.Errorf("delivery logger is required")
	}
	return &Service{chats: chats, documents: documents, logger: logger}, nil
}

// SendFile uploads one file to the principal's own chat. It owns file.Body from the moment it is
// called and closes it on every path, so a caller that has opened a file hands it over and is
// done with it, whether the file is delivered, refused, or rejected as incomplete.
func (s *Service) SendFile(ctx context.Context, principalID string, file File) error {
	if file.Body == nil {
		return fmt.Errorf("delivered file has no content")
	}
	defer file.Body.Close()
	if principalID == "" || file.Name == "" || file.Size <= 0 {
		return fmt.Errorf("delivered file is incomplete")
	}

	chatID, found, err := s.chats.ResolveActiveChat(ctx, principalID)
	if err != nil {
		return fmt.Errorf("resolve chat for delivery: %w", err)
	}
	if !found {
		return ErrNoChat
	}
	if err := s.documents.SendDocument(ctx, chatID, file.Name, file.Body, file.Size); err != nil {
		return fmt.Errorf("upload document to Telegram: %w", err)
	}

	s.logger.InfoContext(ctx, "delivery.file.sent", "principal_id", principalID, "size_bytes", file.Size)
	return nil
}
