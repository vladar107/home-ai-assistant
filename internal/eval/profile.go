// Package eval holds the pieces of the agent evaluation harness: the eval profile, its login, the
// quota pre-flight and the privacy scan.
package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/identity"
	"github.com/vladar107/home-ai-assistant/internal/store/sqlite"
)

const (
	// PrincipalID is the synthetic principal every eval run plays as.
	PrincipalID = "eval"
	// TelegramUserID is the synthetic Telegram identity the eval principal is bootstrapped with.
	// It is never contacted.
	TelegramUserID int64 = 1000000001

	storeFilename = "eval.sqlite"
)

// Profiles is an eval profiles root: the eval profile directory and the eval store beside it.
type Profiles struct {
	Root string
	// Dir is the eval principal's Pi profile directory, <root>/eval.
	Dir string
	// StorePath is the persistent eval store, whose model_connections row binds the credential.
	StorePath string
}

// PrepareProfiles validates the root against the application's data directory and creates the
// root and the eval profile as real private directories.
func PrepareProfiles(root, dataDir string) (Profiles, error) {
	if strings.TrimSpace(root) == "" {
		return Profiles{}, errors.New("--profiles is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return Profiles{}, fmt.Errorf("resolve profiles root: %w", err)
	}
	absoluteData, err := filepath.Abs(dataDir)
	if err != nil {
		return Profiles{}, fmt.Errorf("resolve data directory: %w", err)
	}
	if within(resolveExisting(absoluteRoot), resolveExisting(absoluteData)) {
		return Profiles{}, errors.New("the profiles root must not be, or lie inside, the application's data directory")
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return Profiles{}, fmt.Errorf("create profiles root: %w", err)
	}
	profiles := Profiles{Root: absoluteRoot, Dir: filepath.Join(absoluteRoot, PrincipalID), StorePath: filepath.Join(absoluteRoot, storeFilename)}
	if err := ensurePrivateDirectory(profiles.Dir); err != nil {
		return Profiles{}, err
	}
	return profiles, nil
}

// OpenStore opens the eval store through the application's own SQLite store and migrations and
// makes sure the eval principal exists, which a model connection needs.
func (p Profiles) OpenStore(ctx context.Context) (*sqlite.Store, error) {
	store, err := sqlite.Open(ctx, p.StorePath)
	if err != nil {
		return nil, fmt.Errorf("open eval store: %w", err)
	}
	principal := identity.ConfiguredPrincipal{ID: PrincipalID, DisplayName: "Eval", TelegramUserID: TelegramUserID}
	if err := store.BootstrapConfiguredPrincipals(ctx, []identity.ConfiguredPrincipal{principal}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("bootstrap eval principal: %w", err)
	}
	return store, nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create eval profile: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inspect eval profile: %w", err)
	case !info.IsDir():
		return errors.New("the eval profile must be a real directory, not a link or a file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("restrict eval profile: %w", err)
		}
	}
	return nil
}

// resolveExisting resolves symlinks in the longest existing prefix of an absolute path and
// appends the rest, so a path that does not exist yet still compares by its real location.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	var rest []string
	for current := path; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = append(rest, filepath.Base(current))
	}
}

// within reports whether path is directory or lies inside it.
func within(path, directory string) bool {
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
