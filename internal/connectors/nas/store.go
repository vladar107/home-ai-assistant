package nas

import (
	"context"
	"sort"
	"strings"
	"time"
)

const (
	// searchDepth and searchEntryBudget bound one walk independently of the caller's result
	// limit, so neither a very deep tree nor a very wide one can hold a turn open.
	searchDepth       = 6
	searchEntryBudget = 2000
)

// Root is one directory a principal may reach, under the name the model addresses it by. Writable
// roots are the only places a confirmed save can write.
type Root struct {
	Name     string
	Writable bool
}

// Entry is one directory entry.
type Entry struct {
	Name      string
	Directory bool
	SizeBytes int64
	Modified  time.Time
}

// Store hands out one principal's view of the NAS. A principal with no configured roots has no
// view at all, which is how the connector stays silent for a user who was never granted access.
type Store interface {
	Open(ctx context.Context, principalID string) (Session, bool, error)
}

// Session is one principal's view. It exposes no rename, move, delete, or truncate: the absence
// of those methods is the innermost guarantee that a file already on the NAS cannot be changed
// through the assistant. Writes are further confined to roots mounted read-write, so the kernel
// refuses one even if this package is wrong.
type Session interface {
	Roots() []Root
	List(context.Context, Location) ([]Entry, error)
	// Read returns a chunk starting at offset together with the file's whole size.
	Read(ctx context.Context, location Location, offset int64, limit int) ([]byte, int64, error)
	// MakeDirectory creates one directory whose parent already exists.
	MakeDirectory(context.Context, Location) error
	// Create writes a new file and fails with ErrorExists when the name is taken.
	Create(ctx context.Context, location Location, data []byte) error
	Close() error
}

// hit is one filename match from a search.
type hit struct {
	Location Location
	Entry    Entry
}

// searchTree walks breadth-first below root, matching names case-insensitively. A folder that
// cannot be opened is skipped rather than failing the search, because a partial answer over what
// the user can see is what they asked for.
func searchTree(ctx context.Context, current Session, root Location, query string, limit int) ([]hit, bool, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	queue := []Location{root}
	var hits []hit
	visited := 0
	truncated := false

	for depth := 0; depth <= searchDepth && len(queue) > 0; depth++ {
		var next []Location
		for _, location := range queue {
			entries, err := current.List(ctx, location)
			if err != nil {
				switch classify(err).Class {
				case ErrorDenied, ErrorNotFound:
					continue
				default:
					return nil, false, err
				}
			}
			for _, found := range entries {
				visited++
				if visited > searchEntryBudget {
					return sortedHits(hits), true, nil
				}
				child := location.Child(found.Name)
				if strings.Contains(strings.ToLower(found.Name), needle) {
					if len(hits) == limit {
						truncated = true
						continue
					}
					hits = append(hits, hit{Location: child, Entry: found})
				}
				if found.Directory && depth < searchDepth {
					next = append(next, child)
				}
			}
		}
		queue = next
	}
	return sortedHits(hits), truncated, nil
}

func sortedHits(hits []hit) []hit {
	sort.SliceStable(hits, func(first, second int) bool {
		return hits[first].Location.String() < hits[second].Location.String()
	})
	return hits
}
