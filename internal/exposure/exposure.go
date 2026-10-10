// Package exposure records which conversations have read third-party content and answers whether
// a tool call may still act at once. Text from the household's own devices exposes a conversation
// for memory; text from outside the household, such as web pages, email, or logs, also makes home
// actions, reminders, and background tasks wait for the user's confirmation until /new.
package exposure

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vladar107/home-ai-assistant/internal/outbox"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

type Repository interface {
	// MarkConversationExposed flags the session of the chat request as exposed, and as externally
	// exposed when external is set.
	MarkConversationExposed(ctx context.Context, principalID, requestID string, external bool) error
	ConversationExternal(ctx context.Context, principalID, requestID string) (bool, error)
	// MarkConversationPrivate flags the session of the chat request as having read private content.
	MarkConversationPrivate(ctx context.Context, principalID, requestID string) error
	ConversationPrivate(ctx context.Context, principalID, requestID string) (bool, error)
}

type Service struct {
	repository Repository
	logger     *slog.Logger
}

var (
	_ tools.TurnExposureRecorder = (*Service)(nil)
	_ tools.TurnPrivacyRecorder  = (*Service)(nil)
)

func NewService(repository Repository, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("exposure repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("exposure logger is required")
	}
	return &Service{repository: repository, logger: logger}, nil
}

// MarkTurnExposed records that a turn read third-party content. Only a chat turn is recorded, on
// its conversation session, because the content stays in that session's history until /new.
func (s *Service) MarkTurnExposed(ctx context.Context, principalID, turn string, external bool) {
	requestID, chat := outbox.ChatRequest(turn)
	if !chat {
		return
	}
	if err := s.repository.MarkConversationExposed(ctx, principalID, requestID, external); err != nil {
		s.logger.WarnContext(ctx, "exposure.mark_failed", "principal_id", principalID, "error", err)
	}
}

// External reports whether the calling run, or the chat conversation it belongs to, has read text
// from outside the household. A scheduled or task run answers from its own record alone. A call
// with neither a chat turn nor a run record, or a failed lookup, counts as external.
func (s *Service) External(ctx context.Context, principalID string) bool {
	run := tools.RunFrom(ctx)
	if run != nil && run.External() {
		return true
	}
	requestID, chat := outbox.ChatRequest(tools.TurnFrom(ctx))
	if !chat {
		return run == nil
	}
	external, err := s.repository.ConversationExternal(ctx, principalID, requestID)
	if err != nil {
		s.logger.WarnContext(ctx, "exposure.lookup_failed", "principal_id", principalID, "error", err)
		return true
	}
	return external
}

// MarkTurnPrivate records that a turn read Notion page content or NAS file contents. Only a chat
// turn is recorded, on its conversation session, because the content stays in that session's
// history until /new.
func (s *Service) MarkTurnPrivate(ctx context.Context, principalID, turn string) {
	requestID, chat := outbox.ChatRequest(turn)
	if !chat {
		return
	}
	if err := s.repository.MarkConversationPrivate(ctx, principalID, requestID); err != nil {
		s.logger.WarnContext(ctx, "exposure.mark_private_failed", "principal_id", principalID, "error", err)
	}
}

// Private reports whether the conversation the chat request belongs to has read Notion page
// content or NAS file contents. A failed lookup counts as private.
func (s *Service) Private(ctx context.Context, principalID, requestID string) bool {
	private, err := s.repository.ConversationPrivate(ctx, principalID, requestID)
	if err != nil {
		s.logger.WarnContext(ctx, "exposure.private_lookup_failed", "principal_id", principalID, "error", err)
		return true
	}
	return private
}
