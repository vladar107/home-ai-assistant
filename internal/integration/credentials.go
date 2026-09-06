package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// Credentials is the file store for integration tokens: one
// <root>/<principal>/<integration>.json per connection, mode 0600 inside 0700 directories. A file
// placed there by the operator is adopted the same way as one written by the application.
type Credentials struct {
	root string
}

type credentialFile struct {
	Token string `json:"token"`
}

func NewCredentials(root string) (*Credentials, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("integration credentials directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve integration credentials directory: %w", err)
	}
	if err := preparePrivateDirectory(absolute); err != nil {
		return nil, err
	}
	return &Credentials{root: absolute}, nil
}

// Read returns the token for the principal's integration. A missing file reads as not connected;
// a file readable by others, or one that is not a plain token record, is an error so an unsafe
// operator placement is reported rather than silently used.
func (c *Credentials) Read(principalID, integration string) (string, bool, error) {
	path, err := c.path(principalID, integration)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect %s credentials: %w", integration, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", false, fmt.Errorf("%s credentials file must be a regular file without group or other permissions", integration)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s credentials: %w", integration, err)
	}
	var record credentialFile
	if err := json.Unmarshal(contents, &record); err != nil || strings.TrimSpace(record.Token) == "" {
		return "", false, fmt.Errorf("%s credentials file is not a token record", integration)
	}
	return record.Token, true, nil
}

func (c *Credentials) Write(principalID, integration, token string) error {
	path, err := c.path(principalID, integration)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := preparePrivateDirectory(directory); err != nil {
		return err
	}
	contents, err := json.Marshal(credentialFile{Token: token})
	if err != nil {
		return fmt.Errorf("encode %s credentials: %w", integration, err)
	}
	// The temporary file shares the directory so the rename cannot cross a filesystem.
	file, err := os.CreateTemp(directory, integration+".*")
	if err != nil {
		return fmt.Errorf("create %s credentials file: %w", integration, err)
	}
	temporaryPath := file.Name()
	if err := writeCredentialFile(file, contents); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("write %s credentials: %w", integration, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("install %s credentials file: %w", integration, err)
	}
	return nil
}

func (c *Credentials) Remove(principalID, integration string) (bool, error) {
	path, err := c.path(principalID, integration)
	if err != nil {
		return false, err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove %s credentials: %w", integration, err)
	}
	return true, nil
}

func (c *Credentials) path(principalID, integration string) (string, error) {
	name, err := agentruntime.ProfileDirectoryName(principalID)
	if err != nil {
		return "", err
	}
	if !integrationNamePattern.MatchString(integration) {
		return "", fmt.Errorf("integration name cannot name a credentials file")
	}
	return filepath.Join(c.root, name, integration+".json"), nil
}

func writeCredentialFile(file *os.File, contents []byte) error {
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		return err
	}
	return file.Sync()
}

func preparePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("integration credentials directory %q must be a real directory", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("integration credentials directory %q must not be accessible by group or others", path)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create integration credentials directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure integration credentials directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect integration credentials directory: %w", err)
	}
	return nil
}
