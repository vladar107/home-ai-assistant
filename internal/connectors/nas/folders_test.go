package nas_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func foldersMake(f *fixture, directory string, paths ...string) {
	f.t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(directory, filepath.FromSlash(path)), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestFolderTreeListsFoldersOnly(t *testing.T) {
	f := newFixture(t)
	foldersMake(f, f.home, "Documents/Finance", "Photos")
	f.write(f.home, "Documents/notes.md", "x")
	f.write(f.home, "readme.txt", "x")
	f.contribute()

	result := f.execute("nas_list_folders", `{"path":"home"}`)
	want := "Folders under home, 3 levels: 3\n- home/Documents\n- home/Documents/Finance\n- home/Photos"
	if result.IsError || result.Text != want {
		t.Fatalf("folders = %+v, want %q", result, want)
	}
}

func TestFolderTreeStopsAtTheRequestedDepthAndSaysSo(t *testing.T) {
	f := newFixture(t)
	foldersMake(f, f.home, "A/B/C/D", "Flat")
	f.contribute()

	shallow := f.execute("nas_list_folders", `{"path":"home","depth":2}`)
	if shallow.IsError || strings.Contains(shallow.Text, "home/A/B/C") ||
		!strings.Contains(shallow.Text, "- home/A/B\n") || !strings.Contains(shallow.Text, "more folders not shown") {
		t.Fatalf("shallow = %+v", shallow)
	}
	full := f.execute("nas_list_folders", `{"path":"home","depth":4}`)
	if full.IsError || !strings.Contains(full.Text, "- home/A/B/C/D") || strings.Contains(full.Text, "more folders not shown") {
		t.Fatalf("full = %+v", full)
	}
	exact := f.execute("nas_list_folders", `{"path":"home/A","depth":3}`)
	if exact.IsError || strings.Contains(exact.Text, "more folders not shown") {
		t.Fatalf("a tree that fits exactly was reported as cut: %+v", exact)
	}
	if bad := f.execute("nas_list_folders", `{"path":"home","depth":7}`); !bad.IsError {
		t.Fatalf("depth beyond the maximum = %+v", bad)
	}
}

func TestFolderTreeSkipsSystemFoldersAndSymlinks(t *testing.T) {
	f := newFixture(t)
	foldersMake(f, f.home, "Real", ".hidden/Inner", "@recycle", "#snapshot")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.home, "Linked")); err != nil {
		t.Fatal(err)
	}
	f.contribute()

	result := f.execute("nas_list_folders", `{"path":"home"}`)
	if result.IsError || result.Text != "Folders under home, 3 levels: 1\n- home/Real" {
		t.Fatalf("folders = %+v", result)
	}
}

func TestFolderTreeFailsForAMissingPath(t *testing.T) {
	f := newFixture(t)
	f.contribute()
	for _, path := range []string{"home/Nowhere", "nowhere"} {
		if result := f.execute("nas_list_folders", fmt.Sprintf(`{"path":%q}`, path)); !result.IsError {
			t.Fatalf("%s = %+v", path, result)
		}
	}
}

func TestFolderTreeIsBoundedAndSaysSo(t *testing.T) {
	f := newFixture(t)
	for index := range 1500 {
		foldersMake(f, f.home, fmt.Sprintf("folder-with-a-fairly-long-name-%04d", index))
	}
	f.contribute()

	result := f.execute("nas_list_folders", `{"path":"home","depth":1}`)
	if result.IsError || !strings.Contains(result.Text, "more folders not shown") || len(result.Text) > 10<<10 {
		t.Fatalf("bounded = %d bytes, error %v", len(result.Text), result.IsError)
	}
}

func TestFolderTreeIsOfferedWithoutWrites(t *testing.T) {
	f := newFixture(t)
	if !slices.Contains(names(f.contribute()), "nas_list_folders") {
		t.Fatal("the folder tree tool was not offered")
	}
}
