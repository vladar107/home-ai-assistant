package judgment

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/integration"
)

const (
	// OptInIntegration names the connection record whose presence means the principal opted in.
	OptInIntegration = "typesafe"

	optInWorkspaceID   = "typesafe-judgments-on"
	optInWorkspaceName = "TypeSafe safety checks on"
)

// OptInService keeps each principal's TypeSafe switch. It is off until the principal turns it on.
type OptInService struct {
	repository integration.Repository
	logger     *slog.Logger
	now        func() time.Time
}

var _ OptIn = (*OptInService)(nil)

func NewOptInService(repository integration.Repository, logger *slog.Logger, now func() time.Time) (*OptInService, error) {
	if repository == nil {
		return nil, fmt.Errorf("judgment opt-in repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("judgment opt-in logger is required")
	}
	if now == nil {
		now = time.Now
	}
	return &OptInService{repository: repository, logger: logger, now: now}, nil
}

func (s *OptInService) Status(ctx context.Context, principalID string) (bool, error) {
	_, optedIn, err := s.repository.LoadIntegrationConnection(ctx, principalID, OptInIntegration)
	return optedIn, err
}

func (s *OptInService) JudgmentsEnabled(ctx context.Context, principalID string) (bool, error) {
	return s.Status(ctx, principalID)
}

func (s *OptInService) Enable(ctx context.Context, principalID string) error {
	optedIn, err := s.Status(ctx, principalID)
	if err != nil || optedIn {
		return err
	}
	if err := s.repository.SaveIntegrationConnection(ctx, integration.Connection{
		PrincipalID:   principalID,
		Integration:   OptInIntegration,
		TokenKind:     integration.TokenKindInternal,
		WorkspaceID:   optInWorkspaceID,
		WorkspaceName: optInWorkspaceName,
	}); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "judgment.optin.enabled", "principal_id", principalID)
	return nil
}

func (s *OptInService) Disable(ctx context.Context, principalID string) error {
	deleted, err := s.repository.DeleteIntegrationConnection(ctx, principalID, OptInIntegration)
	if err != nil {
		return err
	}
	if deleted {
		s.logger.InfoContext(ctx, "judgment.optin.disabled", "principal_id", principalID)
	}
	return nil
}
