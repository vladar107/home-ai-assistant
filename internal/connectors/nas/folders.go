package nas

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const defaultFolderDepth = 3

// folderTree is the folders found under one starting location. Incomplete means the walk stopped
// before seeing every folder, either because the entry budget ran out or because folders lie
// deeper than the requested depth.
type folderTree struct {
	Folders    []Location
	Incomplete bool
}

// systemFolderName reports names that hold housekeeping data rather than the user's own folders:
// hidden folders and the recycle and metadata folders of the storage system.
func systemFolderName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")
}

// listFolders walks breadth-first below root and collects folders only. The starting location
// must be listable, so a wrong path is reported; a deeper folder that cannot be opened is skipped
// like it is in a search.
func listFolders(ctx context.Context, current Session, root Location, depth int) (folderTree, error) {
	var tree folderTree
	queue := []Location{root}
	visited := 0

	for level := 1; level <= depth+1 && len(queue) > 0; level++ {
		var next []Location
		for _, location := range queue {
			entries, err := current.List(ctx, location)
			if err != nil {
				if location != root {
					switch classify(err).Class {
					case ErrorDenied, ErrorNotFound:
						continue
					}
				}
				return folderTree{}, err
			}
			for _, found := range entries {
				visited++
				if visited > searchEntryBudget {
					tree.Incomplete = true
					return tree, nil
				}
				if !found.Directory || systemFolderName(found.Name) {
					continue
				}
				if level > depth {
					tree.Incomplete = true
					return tree, nil
				}
				child := location.Child(found.Name)
				tree.Folders = append(tree.Folders, child)
				next = append(next, child)
			}
		}
		queue = next
	}
	return tree, nil
}

// renderFolders keeps folders in the order the walk found them until the byte budget runs out, so
// a cut drops the deepest folders rather than the last ones alphabetically, and only then sorts.
func renderFolders(root Location, depth int, tree folderTree) string {
	levels := "levels"
	if depth == 1 {
		levels = "level"
	}
	header := fmt.Sprintf("Folders under %s, %d %s: %d\n", root, depth, levels, len(tree.Folders))
	const moreFolders = "(more folders not shown; list a subfolder, or raise depth if it is below the maximum)\n"
	size := len(header) + len(moreFolders)
	var lines []string
	for _, folder := range tree.Folders {
		line := "- " + folder.String() + "\n"
		if size+len(line) > maximumListingBytes {
			tree.Incomplete = true
			break
		}
		size += len(line)
		lines = append(lines, line)
	}
	sort.Strings(lines)

	var builder strings.Builder
	builder.WriteString(header)
	if len(tree.Folders) == 0 {
		builder.WriteString("(none)\n")
	}
	for _, line := range lines {
		builder.WriteString(line)
	}
	if tree.Incomplete {
		builder.WriteString(moreFolders)
	}
	return strings.TrimRight(builder.String(), "\n")
}
