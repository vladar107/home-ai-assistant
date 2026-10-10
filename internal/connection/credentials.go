package connection

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// credentialsFileName is the file Pi reads from an agent directory.
const credentialsFileName = "auth.json"

// oauthCredential mirrors the record Pi stores for an OAuth provider in auth.json.
type oauthCredential struct {
	Type      string `json:"type"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId"`
}

func (c oauthCredential) expiresAt() time.Time {
	if c.Expires <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.Expires).UTC()
}

// readCredential returns the provider's record from a principal's auth.json.
// A missing or unreadable record is reported as absent so a broken profile reads as disconnected.
func readCredential(profileDir, provider string) (oauthCredential, bool, error) {
	contents, err := os.ReadFile(filepath.Join(profileDir, credentialsFileName))
	if errors.Is(err, os.ErrNotExist) {
		return oauthCredential{}, false, nil
	}
	if err != nil {
		return oauthCredential{}, false, fmt.Errorf("read model credentials: %w", err)
	}

	var providers map[string]json.RawMessage
	if err := json.Unmarshal(contents, &providers); err != nil {
		return oauthCredential{}, false, nil
	}
	raw, exists := providers[provider]
	if !exists {
		return oauthCredential{}, false, nil
	}
	var credential oauthCredential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return oauthCredential{}, false, nil
	}
	if credential.Refresh == "" {
		return oauthCredential{}, false, nil
	}
	return credential, true, nil
}

func writeCredential(profileDir, provider string, credential oauthCredential) error {
	return writeProviders(profileDir, map[string]oauthCredential{provider: credential})
}

func writeProviders[T any](profileDir string, providers map[string]T) error {
	contents, err := json.Marshal(providers)
	if err != nil {
		return fmt.Errorf("encode model credentials: %w", err)
	}

	// The temporary file shares the profile directory so the rename cannot cross a filesystem.
	file, err := os.CreateTemp(profileDir, credentialsFileName+".*")
	if err != nil {
		return fmt.Errorf("create model credentials file: %w", err)
	}
	temporaryPath := file.Name()
	if err := writeCredentialFile(file, contents); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(profileDir, credentialsFileName)); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("install model credentials file: %w", err)
	}
	return nil
}

func writeCredentialFile(file *os.File, contents []byte) error {
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure model credentials file: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write model credentials: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush model credentials: %w", err)
	}
	return nil
}

func removeCredential(profileDir string) (bool, error) {
	err := os.Remove(filepath.Join(profileDir, credentialsFileName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove model credentials: %w", err)
	}
	return true, nil
}

func preparePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("model profile directory %q must be a real directory", path)
		}
		if permissions := info.Mode().Perm(); permissions != 0o700 {
			return fmt.Errorf("model profile directory %q permissions are %04o, want 0700", path, permissions)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create model profile directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure model profile directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect model profile directory: %w", err)
	}
	return nil
}
