// Package fonts embeds the Inter typefaces the rendered images draw their text with, parsed once
// for every renderer.
package fonts

import (
	"embed"
	"fmt"
	"sync"

	"golang.org/x/image/font/opentype"
)

//go:embed inter/*.ttf
var files embed.FS

// Set is the regular and semibold weights of Inter.
type Set struct {
	Regular, SemiBold *opentype.Font
}

// Inter returns the parsed fonts; the error can only come from a broken embed.
var Inter = sync.OnceValues(func() (Set, error) {
	var set Set
	var err error
	if set.Regular, err = load("inter/Inter-Regular.ttf"); err != nil {
		return Set{}, err
	}
	if set.SemiBold, err = load("inter/Inter-SemiBold.ttf"); err != nil {
		return Set{}, err
	}
	return set, nil
})

func load(name string) (*opentype.Font, error) {
	data, err := files.ReadFile(name)
	if err != nil {
		return nil, err
	}
	parsed, err := opentype.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}
