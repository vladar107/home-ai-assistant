package nas

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	maximumPathRunes    = 1024
	maximumSegmentRunes = 255
)

// Location is a resolved place on the NAS: the named root it belongs to and a slash-separated
// path inside that root. An empty Path is the root itself. A location can never name anything
// outside its root, which is what makes the root grant enforceable after resolution.
type Location struct {
	Root string
	Path string
}

func (l Location) String() string {
	if l.Path == "" {
		return l.Root
	}
	return l.Root + "/" + l.Path
}

// Child returns the location of a named entry inside this one. The name is assumed to have come
// from a listing rather than from the model.
func (l Location) Child(name string) Location {
	if l.Path == "" {
		return Location{Root: l.Root, Path: name}
	}
	return Location{Root: l.Root, Path: l.Path + "/" + name}
}

// Parent returns the location holding this one; a root is its own parent.
func (l Location) Parent() Location {
	index := strings.LastIndex(l.Path, "/")
	if index < 0 {
		return Location{Root: l.Root}
	}
	return Location{Root: l.Root, Path: l.Path[:index]}
}

// Name returns the last segment, which is the root's own name at a root.
func (l Location) Name() string {
	if l.Path == "" {
		return l.Root
	}
	if index := strings.LastIndex(l.Path, "/"); index >= 0 {
		return l.Path[index+1:]
	}
	return l.Path
}

// Ancestors returns the directories between the root and this location, outermost first and
// excluding the location itself, so a save can create the folders it needs in order.
func (l Location) Ancestors() []Location {
	if l.Path == "" {
		return nil
	}
	segments := strings.Split(l.Path, "/")
	ancestors := make([]Location, 0, len(segments)-1)
	for index := 1; index < len(segments); index++ {
		ancestors = append(ancestors, Location{Root: l.Root, Path: strings.Join(segments[:index], "/")})
	}
	return ancestors
}

// Resolve joins the location onto a directory and refuses anything that would leave it. The path
// has already been validated segment by segment, so this is the second of two independent checks:
// one on the shape of the input, one on the result of joining it.
func (l Location) Resolve(directory string) (string, error) {
	full := filepath.Join(directory, filepath.FromSlash(l.Path))
	if full != directory && !strings.HasPrefix(full, directory+string(filepath.Separator)) {
		return "", &Error{Class: ErrorNotFound}
	}
	return full, nil
}

// parseLocation resolves a model-supplied path of the form root/folder/file. Every problem is a
// refusal rather than a repair: a path is never normalized into something that happens to be
// allowed, so traversal cannot be smuggled through a rewrite.
func parseLocation(raw string) (Location, error) {
	segments, err := parseSegments(raw)
	if err != nil {
		return Location{}, err
	}
	return Location{Root: segments[0], Path: strings.Join(segments[1:], "/")}, nil
}

// parseRelative resolves a path inside an already-chosen root, which is how a save names its
// target when the user did not name a folder: the root comes from configuration, not the model.
func parseRelative(root, raw string) (Location, error) {
	segments, err := parseSegments(raw)
	if err != nil {
		return Location{}, err
	}
	return Location{Root: root, Path: strings.Join(segments, "/")}, nil
}

func parseSegments(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "":
		return nil, fmt.Errorf("a path is required")
	case utf8.RuneCountInString(trimmed) > maximumPathRunes:
		return nil, fmt.Errorf("that path is too long")
	case strings.ContainsRune(trimmed, '\\'):
		return nil, fmt.Errorf("separate folders with /")
	case strings.HasPrefix(trimmed, "/"):
		return nil, fmt.Errorf("a path starts with the name of a folder the assistant can reach, not with /")
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		if err := validateSegment(segment); err != nil {
			return nil, err
		}
	}
	return segments, nil
}

func validateSegment(segment string) error {
	switch {
	case segment == "":
		return fmt.Errorf("a path cannot contain an empty folder name")
	case segment == "." || segment == "..":
		return fmt.Errorf("a path cannot contain . or ..")
	case utf8.RuneCountInString(segment) > maximumSegmentRunes:
		return fmt.Errorf("a folder or file name is too long")
	case strings.ContainsAny(segment, `<>:"|?*`):
		return fmt.Errorf(`a folder or file name cannot contain < > : " | ? or *`)
	case strings.HasSuffix(segment, " ") || strings.HasSuffix(segment, "."):
		return fmt.Errorf("a folder or file name cannot end with a space or a dot")
	}
	for _, symbol := range segment {
		if symbol < 0x20 || symbol == 0x7f {
			return fmt.Errorf("a folder or file name cannot contain control characters")
		}
	}
	return nil
}

// safeName turns a file name the user's own device chose into one segment the NAS accepts, so a
// save does not fail over a character the sender's phone allowed.
func safeName(name string) string {
	cleaned := strings.Map(func(symbol rune) rune {
		switch {
		case symbol < 0x20 || symbol == 0x7f:
			return -1
		case strings.ContainsRune(`<>:"|?*/\`, symbol):
			return '-'
		default:
			return symbol
		}
	}, strings.TrimSpace(name))
	cleaned = strings.TrimRight(strings.TrimSpace(cleaned), " .")
	if runes := []rune(cleaned); len(runes) > maximumSegmentRunes {
		cleaned = string(runes[:maximumSegmentRunes])
		cleaned = strings.TrimRight(cleaned, " .")
	}
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "file"
	}
	return cleaned
}
