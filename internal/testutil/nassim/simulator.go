// Package nassim builds the directory tree the NAS connector reads. The connector speaks to no
// server: it sees directories mounted into the process, so the fake is a temporary directory made
// from the seed, with the roots configuration that points the connector at it. Close removes the
// tree.
//
// A file's content is plain text, and folders on the way to a file are created for it; "folders"
// adds empty ones. Paths are relative and stay inside their root.
//
// Example seed (fictional household files):
//
//	{
//	  "roots": [
//	    {"name": "home", "files": {"Documents/notes.md": "Buy milk and bread.",
//	      "Documents/Taxes/2025/summary.txt": "Total due: 120 EUR"}, "folders": ["Photos/2025"]},
//	    {"name": "inbox", "writable": true, "create_folders": true}
//	  ],
//	  "default_write": "inbox"
//	}
package nassim

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

type Seed struct {
	Roots []Root `json:"roots"`
	// DefaultWrite names the writable root saves go to when the user names none.
	DefaultWrite string `json:"default_write"`
}

type Root struct {
	Name          string            `json:"name"`
	Writable      bool              `json:"writable"`
	Shared        bool              `json:"shared"`
	CreateFolders bool              `json:"create_folders"`
	Folders       []string          `json:"folders"`
	Files         map[string]string `json:"files"`
}

// Validate checks that seed has the shape the fake accepts.
func Validate(seed json.RawMessage) error {
	_, err := parse(seed)
	return err
}

func parse(raw json.RawMessage) (Seed, error) {
	var seed Seed
	if err := simkit.Decode(raw, &seed); err != nil {
		return seed, err
	}
	names := map[string]bool{}
	for index, root := range seed.Roots {
		if root.Name == "" || strings.ContainsAny(root.Name, `/\`) {
			return seed, fmt.Errorf("roots[%d] needs a name without slashes", index)
		}
		if names[root.Name] {
			return seed, fmt.Errorf("roots[%d] repeats the name %q", index, root.Name)
		}
		names[root.Name] = true
		for _, relative := range root.Folders {
			if !inside(relative) {
				return seed, fmt.Errorf("roots[%d] folder %q must be a relative path inside the root", index, relative)
			}
		}
		for relative := range root.Files {
			if !inside(relative) {
				return seed, fmt.Errorf("roots[%d] file %q must be a relative path inside the root", index, relative)
			}
		}
	}
	if seed.DefaultWrite != "" && !names[seed.DefaultWrite] {
		return seed, fmt.Errorf("default_write names no root")
	}
	return seed, nil
}

func inside(relative string) bool {
	clean := path.Clean(relative)
	return relative != "" && clean == relative && !path.IsAbs(clean) && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

// Tree is the built directory tree.
type Tree struct {
	dir  string
	seed Seed
}

// Start builds the tree in a new temporary directory; the caller closes it.
func Start(raw json.RawMessage) (*Tree, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "nassim-")
	if err != nil {
		return nil, err
	}
	tree := &Tree{dir: dir, seed: seed}
	if err := tree.build(); err != nil {
		tree.Close()
		return nil, err
	}
	return tree, nil
}

// New builds the tree for the length of a test.
func New(t testing.TB, raw json.RawMessage) *Tree {
	t.Helper()
	tree, err := Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tree.Close)
	return tree
}

func (t *Tree) build() error {
	for _, root := range t.seed.Roots {
		base := filepath.Join(t.dir, root.Name)
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
		for _, folder := range root.Folders {
			if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(folder)), 0o755); err != nil {
				return err
			}
		}
		files := make([]string, 0, len(root.Files))
		for relative := range root.Files {
			files = append(files, relative)
		}
		sort.Strings(files)
		for _, relative := range files {
			target := filepath.Join(base, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(root.Files[relative]), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Roots is the connector's roots configuration for the principal: every seeded root, as the
// principal's own.
func (t *Tree) Roots(principalID string) []nas.PrincipalRoots {
	roots := make([]nas.RootPath, 0, len(t.seed.Roots))
	for _, root := range t.seed.Roots {
		roots = append(roots, nas.RootPath{
			Name: root.Name, Path: filepath.Join(t.dir, root.Name),
			Writable: root.Writable, Shared: root.Shared, CreateFolders: root.CreateFolders,
		})
	}
	return []nas.PrincipalRoots{{PrincipalID: principalID, Roots: roots, DefaultWrite: t.seed.DefaultWrite}}
}

// Dir is the directory holding every root.
func (t *Tree) Dir() string { return t.dir }

// Close removes the tree.
func (t *Tree) Close() { _ = os.RemoveAll(t.dir) }
