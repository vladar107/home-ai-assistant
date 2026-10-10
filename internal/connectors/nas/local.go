package nas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// PrincipalRoots is one principal's configured view of the NAS, as the application hands it to
// the connector.
type PrincipalRoots struct {
	PrincipalID  string
	Roots        []RootPath
	DefaultWrite string
}

// RootPath is one mounted directory and the name the model addresses it by.
type RootPath struct {
	Name     string
	Path     string
	Writable bool
}

// localStore reads the NAS through directories mounted into this container. There is no account
// and no password: the application already runs on the NAS as the household's own user, and a
// read-only mount is refused by the kernel rather than by this package.
type localStore struct {
	principals map[string][]resolvedRoot
}

type resolvedRoot struct {
	name     string
	path     string
	writable bool
}

var _ Store = (*localStore)(nil)

func newLocalStore(configured []PrincipalRoots) (*localStore, error) {
	store := &localStore{principals: make(map[string][]resolvedRoot, len(configured))}
	for _, principal := range configured {
		roots := make([]resolvedRoot, 0, len(principal.Roots))
		for _, root := range principal.Roots {
			// The root is resolved once at startup so a symlinked mount point is compared
			// against its real path on every call rather than its configured one.
			path, err := filepath.EvalSymlinks(root.Path)
			if err != nil {
				return nil, fmt.Errorf("NAS root %q for %q is not reachable", root.Name, principal.PrincipalID)
			}
			roots = append(roots, resolvedRoot{name: root.Name, path: path, writable: root.Writable})
		}
		store.principals[principal.PrincipalID] = roots
	}
	return store, nil
}

func (s *localStore) Open(_ context.Context, principalID string) (Session, bool, error) {
	roots, found := s.principals[principalID]
	if !found || len(roots) == 0 {
		return nil, false, nil
	}
	return &localSession{roots: roots}, true, nil
}

type localSession struct {
	roots []resolvedRoot
}

func (s *localSession) Roots() []Root {
	roots := make([]Root, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, Root{Name: root.name, Writable: root.writable})
	}
	return roots
}

func (s *localSession) List(_ context.Context, location Location) ([]Entry, error) {
	root, name, err := s.resolve(location, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory, err := root.Open(name)
	if err != nil {
		return nil, classify(err)
	}
	defer directory.Close()
	found, err := directory.ReadDir(-1)
	if err != nil {
		return nil, classify(err)
	}
	slices.SortFunc(found, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	entries := make([]Entry, 0, len(found))
	for _, item := range found {
		// A symlink can point outside the mounted root, so the grant cannot be enforced after
		// resolution for one. They are left out of every listing rather than followed.
		if item.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := item.Info()
		if err != nil {
			continue
		}
		entries = append(entries, Entry{
			Name:      item.Name(),
			Directory: item.IsDir(),
			SizeBytes: info.Size(),
			Modified:  info.ModTime(),
		})
	}
	return entries, nil
}

func (s *localSession) Read(_ context.Context, location Location, offset int64, limit int) ([]byte, int64, error) {
	root, name, err := s.resolve(location, false)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return nil, 0, classify(err)
	}
	if info.IsDir() {
		return nil, 0, &Error{Class: ErrorDirectory}
	}
	if !info.Mode().IsRegular() {
		return nil, 0, &Error{Class: ErrorNotFound}
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, 0, classify(err)
	}
	defer file.Close()
	if offset >= info.Size() {
		return nil, info.Size(), nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, classify(err)
	}
	chunk := make([]byte, limit)
	read, err := io.ReadFull(file, chunk)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, 0, classify(err)
	}
	return chunk[:read], info.Size(), nil
}

// Open hands the caller an open file rather than its bytes, so a file on its way to Telegram is
// never held in memory here. Every refusal Read makes is made here for the same reasons.
func (s *localSession) Open(_ context.Context, location Location) (io.ReadCloser, int64, error) {
	root, name, err := s.resolve(location, false)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return nil, 0, classify(err)
	}
	if info.IsDir() {
		return nil, 0, &Error{Class: ErrorDirectory}
	}
	if !info.Mode().IsRegular() {
		return nil, 0, &Error{Class: ErrorNotFound}
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, 0, classify(err)
	}
	return file, info.Size(), nil
}

func (s *localSession) MakeDirectory(_ context.Context, location Location) error {
	root, name, err := s.resolve(location, true)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir(name, 0o755); err != nil {
		return classify(err)
	}
	return nil
}

func (s *localSession) Create(_ context.Context, location Location, data []byte) error {
	root, name, err := s.resolve(location, true)
	if err != nil {
		return err
	}
	defer root.Close()
	// O_EXCL is this package's own guarantee: an existing name fails here rather than being
	// opened for truncation. A read-only mount refuses the call regardless.
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return classify(err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return classify(err)
	}
	return nil
}

func (s *localSession) Close() error { return nil }

// resolve opens a location's root and names the location inside it. The root grant is checked on
// the joined path and on what the filesystem says that path really is, so a symlink planted inside
// a root cannot reach outside it. The caller then works through the opened root, which keeps the
// lookup inside it even when a directory on the path is swapped for a symlink after these checks.
func (s *localSession) resolve(location Location, writing bool) (*os.Root, string, error) {
	root, known := s.root(location.Root)
	if !known {
		return nil, "", &Error{Class: ErrorNotFound}
	}
	if writing && !root.writable {
		return nil, "", &Error{Class: ErrorDenied}
	}
	full, err := location.Resolve(root.path)
	if err != nil {
		return nil, "", err
	}
	if err := confinedToRoot(full, root.path); err != nil {
		return nil, "", err
	}
	opened, err := os.OpenRoot(root.path)
	if err != nil {
		return nil, "", classify(err)
	}
	name := filepath.FromSlash(location.Path)
	if name == "" {
		name = "."
	}
	return opened, name, nil
}

func (s *localSession) root(name string) (resolvedRoot, bool) {
	for _, root := range s.roots {
		if root.name == name {
			return root, true
		}
	}
	return resolvedRoot{}, false
}

// confinedToRoot re-checks the path after the filesystem has followed every symlink in it. A path
// that does not exist yet is checked through its parent, which is what a save needs.
func confinedToRoot(full, root string) error {
	candidate := full
	real, err := filepath.EvalSymlinks(candidate)
	if errors.Is(err, os.ErrNotExist) {
		candidate = filepath.Dir(full)
		real, err = filepath.EvalSymlinks(candidate)
	}
	if err != nil {
		return classify(err)
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return &Error{Class: ErrorNotFound}
	}
	return nil
}
