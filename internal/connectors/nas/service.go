// Package nas is the NAS connector. The application already runs on the NAS as the household's
// own user, so the files reach it through directories mounted into the container rather than
// through an account: there is no NAS password anywhere in this system. Each principal is granted
// a named set of those directories, every path is resolved inside one of them, and only a root
// mounted read-write can receive a confirmed save. Nothing here can modify or remove a file that
// is already on the NAS.
package nas

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/module"
)

const (
	Integration  = "nas"
	workspaceID  = "nas"
	workspaceTag = "NAS files"
)

type Config struct {
	// Roots is each principal's configured view; a principal absent from it has no NAS access.
	Roots []PrincipalRoots
}

// Status is the non-secret view of a principal's NAS access.
type Status struct {
	Connected bool
	Roots     []Root
	// CanSave reports whether this principal has anywhere to save at all.
	CanSave       bool
	DefaultFolder string
	WritesEnabled bool
}

type Service struct {
	store        Store
	repository   integration.Repository
	proposer     Proposer
	attachments  Attachments
	sender       Sender
	defaultWrite map[string]string
	logger       *slog.Logger

	running atomic.Bool
}

var _ module.Module = (*Service)(nil)

// NewService builds the connector. A nil proposer or nil attachments leaves the save tool out
// entirely, as does a principal with no writable root; a nil sender leaves out the send tool.
func NewService(cfg Config, repository integration.Repository, proposer Proposer, attachments Attachments, sender Sender, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("NAS repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("NAS logger is required")
	}
	store, err := newLocalStore(cfg.Roots)
	if err != nil {
		return nil, err
	}
	defaults := make(map[string]string, len(cfg.Roots))
	for _, principal := range cfg.Roots {
		if principal.DefaultWrite != "" {
			defaults[principal.PrincipalID] = principal.DefaultWrite
		}
	}
	return &Service{
		store:        store,
		repository:   repository,
		proposer:     proposer,
		attachments:  attachments,
		sender:       sender,
		defaultWrite: defaults,
		logger:       logger,
	}, nil
}

func (s *Service) Descriptor() module.Descriptor {
	return module.Descriptor{ID: Integration, Version: "0.1.0", Kind: module.KindConnector}
}

func (s *Service) Start(context.Context) error {
	s.running.Store(true)
	return nil
}

func (s *Service) Ready(context.Context) error {
	if !s.running.Load() {
		return fmt.Errorf("NAS connector is not running")
	}
	return nil
}

func (s *Service) Shutdown(context.Context) error {
	s.running.Store(false)
	return nil
}

// Status reports what this principal may reach. The metadata row exists only to hold the saving
// switch, so it is created the first time a configured principal is seen and never carries a
// credential of any kind.
func (s *Service) Status(ctx context.Context, principalID string) (Status, error) {
	session, allowed, err := s.store.Open(ctx, principalID)
	if err != nil {
		return Status{}, err
	}
	if !allowed {
		return Status{}, nil
	}
	defer session.Close()

	record, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil {
		return Status{}, err
	}
	if !bound {
		record = integration.Connection{
			PrincipalID:   principalID,
			Integration:   Integration,
			TokenKind:     integration.TokenKindInternal,
			WorkspaceID:   workspaceID,
			WorkspaceName: workspaceTag,
		}
		if err := s.repository.SaveIntegrationConnection(ctx, record); err != nil {
			return Status{}, err
		}
		s.logger.InfoContext(ctx, "nas.access.granted", "principal_id", principalID)
	}

	roots := session.Roots()
	status := Status{
		Connected:     true,
		Roots:         roots,
		CanSave:       s.savingPossible(roots),
		WritesEnabled: record.WritesEnabled,
	}
	if folder, found := s.defaultFolder(principalID, roots); found {
		status.DefaultFolder = folder.String()
	}
	return status, nil
}

// SetWrites switches the principal's saving grant; it reports no access when the principal has
// none to switch.
func (s *Service) SetWrites(ctx context.Context, principalID string, enabled bool) (Status, error) {
	changed, err := s.repository.SetIntegrationWrites(ctx, principalID, Integration, enabled)
	if err != nil {
		return Status{}, err
	}
	if !changed {
		return Status{}, nil
	}
	return s.Status(ctx, principalID)
}

// savingPossible reports whether a confirmed save could go anywhere at all for these roots.
func (s *Service) savingPossible(roots []Root) bool {
	if s.proposer == nil || s.attachments == nil {
		return false
	}
	for _, root := range roots {
		if root.Writable {
			return true
		}
	}
	return false
}

// defaultFolder is where a principal's saves land when the user names no folder. A configured
// default that is not among the principal's writable roots is ignored rather than trusted.
func (s *Service) defaultFolder(principalID string, roots []Root) (Location, bool) {
	name, found := s.defaultWrite[principalID]
	if !found {
		return Location{}, false
	}
	for _, root := range roots {
		if root.Name == name && root.Writable {
			return Location{Root: name}, true
		}
	}
	return Location{}, false
}

// session opens the principal's view, reporting false when they have no NAS access, so a grant
// removed from configuration and reloaded fails closed.
func (s *Service) session(ctx context.Context, principalID string) (Session, bool, error) {
	return s.store.Open(ctx, principalID)
}

// writesEnabled reports the principal's saving switch as it stands right now.
func (s *Service) writesEnabled(ctx context.Context, principalID string) (bool, error) {
	record, bound, err := s.repository.LoadIntegrationConnection(ctx, principalID, Integration)
	if err != nil || !bound {
		return false, err
	}
	return record.WritesEnabled, nil
}
