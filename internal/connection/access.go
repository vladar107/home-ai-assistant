package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNotConnected reports that the principal has no usable model subscription of their own.
var ErrNotConnected = errors.New("model subscription is not connected")

const (
	// refreshMargin renews a token that would expire during the request about to use it.
	refreshMargin = 5 * time.Minute
	// lockStale and lockWait match the proper-lockfile settings Pi uses for auth.json, so a lock
	// either side abandons is reclaimed by the other after the same interval.
	lockStale       = 30 * time.Second
	lockWait        = 30 * time.Second
	lockMaximumWait = time.Second
	// refreshTimeout keeps the lock well inside its stale interval, since it is never touched
	// while held.
	refreshTimeout = 15 * time.Second
)

// Credential is a principal's current ChatGPT access token. It is handed to a request and never
// stored or logged.
type Credential struct {
	AccessToken string
	AccountID   string
}

// ErrCredentialStale reports that the stored access token is expired or due for a refresh.
var ErrCredentialStale = errors.New("model credential needs a refresh")

// CurrentCredential returns the principal's access token only while it is not due for a refresh.
// It never refreshes, so a caller on a short deadline cannot cancel a refresh after the token
// endpoint rotated the refresh token.
func (s *Service) CurrentCredential(ctx context.Context, principalID string) (Credential, error) {
	profileDir, err := s.ProfileDir(principalID)
	if err != nil {
		return Credential{}, err
	}
	record, bound, err := s.repository.LoadModelConnection(ctx, principalID)
	if err != nil {
		return Credential{}, err
	}
	if !bound {
		return Credential{}, ErrNotConnected
	}
	credential, found, err := readCredential(profileDir, s.provider)
	if err != nil {
		return Credential{}, err
	}
	if !found || credential.AccountID != record.AccountID {
		return Credential{}, ErrNotConnected
	}
	if !current(credential) {
		return Credential{}, ErrCredentialStale
	}
	return Credential{AccessToken: credential.Access, AccountID: credential.AccountID}, nil
}

// Credential returns the principal's own access token, refreshing it first when it is about to
// expire. Pi refreshes the same file, so the refresh happens under Pi's lock and is skipped when
// Pi got there first.
func (s *Service) Credential(ctx context.Context, principalID string) (Credential, error) {
	profileDir, err := s.ProfileDir(principalID)
	if err != nil {
		return Credential{}, err
	}
	record, bound, err := s.repository.LoadModelConnection(ctx, principalID)
	if err != nil {
		return Credential{}, err
	}
	if !bound {
		return Credential{}, ErrNotConnected
	}
	credential, found, err := readCredential(profileDir, s.provider)
	if err != nil {
		return Credential{}, err
	}
	if !found || credential.AccountID != record.AccountID {
		return Credential{}, ErrNotConnected
	}
	if current(credential) {
		return Credential{AccessToken: credential.Access, AccountID: credential.AccountID}, nil
	}

	authPath := filepath.Join(profileDir, credentialsFileName)
	release, err := lockAuthFile(ctx, authPath)
	if err != nil {
		return Credential{}, err
	}
	defer release()

	credential, found, err = readCredential(profileDir, s.provider)
	if err != nil {
		return Credential{}, err
	}
	if !found || credential.AccountID != record.AccountID {
		return Credential{}, ErrNotConnected
	}
	if current(credential) {
		return Credential{AccessToken: credential.Access, AccountID: credential.AccountID}, nil
	}

	refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	refreshed, err := s.client.refresh(refreshCtx, credential.Refresh)
	if err != nil {
		s.logger.WarnContext(ctx, "connection.refresh.failed", "principal_id", principalID)
		return Credential{}, fmt.Errorf("refresh model credentials: %w", err)
	}
	if refreshed.AccountID != record.AccountID {
		return Credential{}, fmt.Errorf("refreshed model credentials belong to a different account")
	}
	if err := replaceCredential(profileDir, s.provider, refreshed); err != nil {
		return Credential{}, err
	}
	s.logger.InfoContext(ctx, "connection.refresh.completed", "principal_id", principalID)
	return Credential{AccessToken: refreshed.Access, AccountID: refreshed.AccountID}, nil
}

func current(credential oauthCredential) bool {
	return credential.Access != "" && time.Until(credential.expiresAt()) > refreshMargin
}

// lockAuthFile takes the lock proper-lockfile would: a directory beside the file whose
// modification time marks it alive.
func lockAuthFile(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(lockWait)
	delay := 10 * time.Millisecond
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock model credentials: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("model credentials are locked")
		}
		if !wait(ctx, delay) {
			return nil, ctx.Err()
		}
		delay = min(delay*2, lockMaximumWait)
	}
}

// replaceCredential rewrites one provider's record and keeps every other record in the file, which
// Pi may have added.
func replaceCredential(profileDir, provider string, credential oauthCredential) error {
	providers := map[string]json.RawMessage{}
	contents, err := os.ReadFile(filepath.Join(profileDir, credentialsFileName))
	if err != nil {
		return fmt.Errorf("read model credentials: %w", err)
	}
	if err := json.Unmarshal(contents, &providers); err != nil {
		return fmt.Errorf("decode model credentials: %w", err)
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("encode model credentials: %w", err)
	}
	providers[provider] = encoded
	return writeProviders(profileDir, providers)
}
