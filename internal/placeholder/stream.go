package placeholder

import (
	"context"
	"strings"
	"unicode/utf8"
)

// StreamResolver resolves tokens in streamed text deltas. It holds text from ⟦ until ⟧, so a
// token split across deltas is resolved whole, and releases held text unchanged once it reaches
// the length of the longest well-formed token without ⟧. It is not safe for concurrent use.
type StreamResolver struct {
	ctx     context.Context
	mapper  *Mapper
	held    strings.Builder
	runes   int
	partial string
}

// Stream returns a resolver for one stream of text deltas.
func (m *Mapper) Stream(ctx context.Context) *StreamResolver {
	return &StreamResolver{ctx: ctx, mapper: m}
}

// Write takes the next delta and returns the text that is ready to show.
func (s *StreamResolver) Write(chunk string) (string, error) {
	chunk = s.partial + chunk
	s.partial = ""
	if cut := incompleteTail(chunk); cut < len(chunk) {
		chunk, s.partial = chunk[:cut], chunk[cut:]
	}
	var out strings.Builder
	for _, r := range chunk {
		switch {
		case r == '⟦':
			out.WriteString(s.release())
			s.hold(r)
		case s.runes == 0:
			out.WriteRune(r)
		case r == '⟧':
			s.hold(r)
			resolved, err := s.mapper.ResolveText(s.ctx, s.release())
			if err != nil {
				return "", err
			}
			out.WriteString(resolved)
		default:
			s.hold(r)
			if s.runes >= maxToken {
				out.WriteString(s.release())
			}
		}
	}
	return out.String(), nil
}

// Flush returns whatever the resolver still holds, at the end of the stream.
func (s *StreamResolver) Flush() (string, error) {
	held := s.release() + s.partial
	s.partial = ""
	return held, nil
}

func (s *StreamResolver) hold(r rune) {
	s.held.WriteRune(r)
	s.runes++
}

func (s *StreamResolver) release() string {
	held := s.held.String()
	s.held.Reset()
	s.runes = 0
	return held
}

// incompleteTail returns where a UTF-8 sequence cut off at the end of chunk begins, or len(chunk).
func incompleteTail(chunk string) int {
	for i := len(chunk) - 1; i >= 0 && i >= len(chunk)-utf8.UTFMax; i-- {
		if utf8.RuneStart(chunk[i]) {
			if !utf8.FullRuneInString(chunk[i:]) {
				return i
			}
			break
		}
	}
	return len(chunk)
}
