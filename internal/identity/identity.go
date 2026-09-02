package identity

import (
	"context"
	"fmt"
	"strings"
)

const maxConfiguredPrincipalCount = 2

type ConfiguredPrincipal struct {
	ID             string
	DisplayName    string
	TelegramUserID int64
}

type TelegramUser struct {
	ID          int64
	Username    string
	DisplayName string
}

type Principal struct {
	ID          string
	DisplayName string
}

type AuthorizationStatus string

const (
	AuthorizationStatusAuthorized        AuthorizationStatus = "authorized"
	AuthorizationStatusUnconfigured      AuthorizationStatus = "unconfigured"
	AuthorizationStatusUnknownIdentity   AuthorizationStatus = "unknown_identity"
	AuthorizationStatusNotAllowlisted    AuthorizationStatus = "not_allowlisted"
	AuthorizationStatusAllowlistRevoked  AuthorizationStatus = "allowlist_revoked"
	AuthorizationStatusPrincipalDisabled AuthorizationStatus = "principal_disabled"
)

type Authorization struct {
	Status    AuthorizationStatus
	Principal Principal
}

func (a Authorization) Authorized() bool {
	return a.Status == AuthorizationStatusAuthorized
}

type Repository interface {
	BootstrapConfiguredPrincipals(context.Context, []ConfiguredPrincipal) error
	ObserveTelegramIdentity(context.Context, TelegramUser) error
	ResolveActivePrincipal(context.Context, int64) (Authorization, error)
}

type Service struct {
	repository       Repository
	configured       []ConfiguredPrincipal
	configuredUserID map[int64]struct{}
}

func NewService(repository Repository, configured []ConfiguredPrincipal) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("identity repository is required")
	}
	if len(configured) < 1 || len(configured) > maxConfiguredPrincipalCount {
		return nil, fmt.Errorf("identity service requires one or two configured principals")
	}

	configuredCopy := append([]ConfiguredPrincipal(nil), configured...)
	principalIDs := make(map[string]struct{}, len(configured))
	configuredUserIDs := make(map[int64]struct{}, len(configured))
	for _, principal := range configuredCopy {
		if principal.ID == "" || principal.ID != strings.TrimSpace(principal.ID) {
			return nil, fmt.Errorf("configured principal ID must be non-empty and trimmed")
		}
		if strings.TrimSpace(principal.DisplayName) == "" {
			return nil, fmt.Errorf("configured principal %q display name must be non-empty", principal.ID)
		}
		if principal.TelegramUserID <= 0 {
			return nil, fmt.Errorf("configured principal %q Telegram user ID must be positive", principal.ID)
		}
		if _, exists := principalIDs[principal.ID]; exists {
			return nil, fmt.Errorf("configured principal ID %q is duplicated", principal.ID)
		}
		if _, exists := configuredUserIDs[principal.TelegramUserID]; exists {
			return nil, fmt.Errorf("configured Telegram user ID is duplicated")
		}
		principalIDs[principal.ID] = struct{}{}
		configuredUserIDs[principal.TelegramUserID] = struct{}{}
	}

	return &Service{
		repository:       repository,
		configured:       configuredCopy,
		configuredUserID: configuredUserIDs,
	}, nil
}

func (s *Service) Bootstrap(ctx context.Context) error {
	if err := s.repository.BootstrapConfiguredPrincipals(ctx, s.configured); err != nil {
		return fmt.Errorf("bootstrap configured identities: %w", err)
	}
	return nil
}

func (s *Service) Observe(ctx context.Context, user TelegramUser) error {
	if user.ID <= 0 {
		return fmt.Errorf("observe Telegram identity: user ID must be positive")
	}
	if err := s.repository.ObserveTelegramIdentity(ctx, user); err != nil {
		return fmt.Errorf("observe Telegram identity: %w", err)
	}
	return nil
}

func (s *Service) Authorize(ctx context.Context, telegramUserID int64) (Authorization, error) {
	if _, configured := s.configuredUserID[telegramUserID]; !configured {
		return Authorization{Status: AuthorizationStatusUnconfigured}, nil
	}

	authorization, err := s.repository.ResolveActivePrincipal(ctx, telegramUserID)
	if err != nil {
		return Authorization{}, fmt.Errorf("resolve Telegram identity: %w", err)
	}
	switch authorization.Status {
	case AuthorizationStatusAuthorized:
		if authorization.Principal.ID == "" {
			return Authorization{}, fmt.Errorf("resolve Telegram identity: repository returned an empty principal")
		}
		return authorization, nil
	case AuthorizationStatusUnknownIdentity,
		AuthorizationStatusNotAllowlisted,
		AuthorizationStatusAllowlistRevoked,
		AuthorizationStatusPrincipalDisabled:
		authorization.Principal = Principal{}
		return authorization, nil
	default:
		return Authorization{}, fmt.Errorf("resolve Telegram identity: repository returned unknown authorization status %q", authorization.Status)
	}
}
