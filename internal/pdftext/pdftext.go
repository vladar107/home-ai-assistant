// Package pdftext turns a PDF into plain text for the model to read. It is the one place the
// application parses PDFs, so every reader shares the same limits and refusals, and replacing the
// library touches nothing else.
package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"github.com/vladar107/home-ai-assistant/internal/clip"
)

const (
	MaximumPages     = 100
	MaximumTextBytes = 512 << 10
)

var (
	ErrEncrypted = errors.New("the PDF is password-protected")
	ErrNoText    = errors.New("the PDF has no text layer")
	ErrMalformed = errors.New("the PDF cannot be parsed")
)

// Extract returns the plain text of a PDF held in memory, page by page with a blank line between
// pages. When the document is longer than MaximumPages pages or MaximumTextBytes of text, the text
// stops on a whole rune at the limit and ends with one line saying where it stopped, so a reader
// knows the rest of the document exists.
//
// The library has no limits of its own: a small file can inflate into gigabytes of content, and
// its parser cannot be interrupted. So the PDF is parsed in a worker process with hard memory and
// CPU limits, one document at a time, and the worker is killed as soon as ctx ends. A document
// that exceeds the limits is refused as ErrMalformed.
func Extract(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case turn <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-turn }()
	return delegate(ctx, data)
}

func extract(data []byte) (text string, err error) {
	// The library panics on many malformed inputs, and a document the user sent must not be able
	// to take the process down with it.
	defer func() {
		if recovered := recover(); recovered != nil {
			text, err = "", fmt.Errorf("%w: %v", ErrMalformed, recovered)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if encrypted(err) {
			return "", ErrEncrypted
		}
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	pages := reader.NumPage()
	var builder strings.Builder
	stopped := ""
	failed := false
	for number := 1; number <= pages; number++ {
		if number > MaximumPages {
			stopped = fmt.Sprintf("[The text stops here: only the first %d of %d pages were read.]", MaximumPages, pages)
			break
		}
		page := reader.Page(number)
		if page.V.IsNull() {
			continue
		}
		content, err := page.GetPlainText(nil)
		if err != nil {
			failed = true
			continue
		}
		content = normalize(content)
		if content == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(content)
		if builder.Len() > MaximumTextBytes {
			stopped = fmt.Sprintf("[The text stops here: only the first %d KB of text were read.]", MaximumTextBytes>>10)
			break
		}
	}

	text = builder.String()
	if text == "" {
		if failed {
			return "", fmt.Errorf("%w: no page could be read", ErrMalformed)
		}
		return "", ErrNoText
	}
	if stopped != "" {
		text = strings.TrimSpace(clip.Bytes(text, MaximumTextBytes)) + "\n\n" + stopped
	}
	return text, nil
}

// encrypted recognises the library's refusals of an encrypted document, which it reports only as
// text apart from the wrong-password case.
func encrypted(err error) bool {
	return errors.Is(err, pdf.ErrInvalidPassword) || strings.Contains(strings.ToLower(err.Error()), "encrypt")
}

var ligatures = strings.NewReplacer("\ufb00", "ff", "\ufb01", "fi", "\ufb02", "fl", "\ufb03", "ffi", "\ufb04", "ffl")

// normalize makes one page's text valid UTF-8 without control characters, collapses the spaces in
// every line, and keeps at most one blank line in a row,
// since PDF text operators produce runs of empty lines.
func normalize(text string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = ligatures.Replace(text)
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t' || r == '\u00a0':
			return ' '
		case unicode.IsControl(r) || r == utf8.RuneError || r == '\ufeff':
			return -1
		default:
			return r
		}
	}, text)

	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	blank := true
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				kept = append(kept, "")
			}
			blank = true
			continue
		}
		kept = append(kept, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
