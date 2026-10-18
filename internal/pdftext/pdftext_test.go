package pdftext_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/pdftext"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// build writes the smallest PDF the library accepts, one page per entry, so the caps can be
// exercised without committing a hundred-page fixture.
func build(pages []string) []byte {
	contents := make([]content, len(pages))
	for index, text := range pages {
		contents[index] = content{data: []byte(fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text))}
	}
	return assemble(contents)
}

// content is one page's content stream, with the stream dictionary entries besides its length.
type content struct {
	entries string
	data    []byte
}

func assemble(pages []content) []byte {
	var out bytes.Buffer
	var offsets []int
	object := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n")
	kids := make([]string, len(pages))
	for index := range pages {
		kids[index] = fmt.Sprintf("%d 0 R", 4+2*index)
	}
	object("<< /Type /Catalog /Pages 2 0 R >>")
	object(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	object("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	for index, page := range pages {
		object(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*index))
		object(fmt.Sprintf("<< /Length %d %s >>\nstream\n%s\nendstream", len(page.data), page.entries, page.data))
	}
	start := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, start)
	return out.Bytes()
}

// bomb writes a PDF whose one page has a content stream of inflated MiB, compressed to a small
// fraction of that: operands with no operator, which the library keeps on its stack, each in a
// value many times its two bytes. Once the window holds nothing but the repeated pattern, every
// flushed block of the compressor decodes the same way, so one block repeated stands for the whole
// stream without compressing all of it.
func bomb(t *testing.T, inflated int) []byte {
	t.Helper()
	var stream bytes.Buffer
	writer, err := zlib.NewWriterLevel(&stream, zlib.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("1 "), 1<<19)
	write := func(data []byte) {
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	write(append([]byte("BT "), chunk...))
	head := stream.Len()
	write(chunk)
	block := bytes.Clone(stream.Bytes()[head:])
	for range inflated - 2 {
		stream.Write(block)
	}
	return assemble([]content{{entries: "/Filter /FlateDecode", data: stream.Bytes()}})
}

func TestTextIsExtractedPageByPage(t *testing.T) {
	text, err := pdftext.Extract(t.Context(), fixture(t, "handbook.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Household handbook, page 1", "Household handbook, page 5", "Ümlauts and café stay intact."} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\n\n\n") || !utf8.ValidString(text) {
		t.Fatalf("text is not normalised:\n%q", text)
	}
	if strings.Index(text, "page 1") > strings.Index(text, "page 2") {
		t.Fatal("pages are out of order")
	}
}

func TestLongDocumentsStopAtThePageCap(t *testing.T) {
	pages := make([]string, pdftext.MaximumPages+5)
	for index := range pages {
		pages[index] = fmt.Sprintf("Page number %d", index+1)
	}
	text, err := pdftext.Extract(t.Context(), build(pages))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, fmt.Sprintf("Page number %d", pdftext.MaximumPages)) ||
		strings.Contains(text, fmt.Sprintf("Page number %d", pdftext.MaximumPages+1)) {
		t.Fatalf("page cap not applied:\n%s", text[len(text)-200:])
	}
	if !strings.HasSuffix(text, "only the first 100 of 105 pages were read.]") {
		t.Fatalf("text does not say it stopped:\n%s", text[len(text)-200:])
	}
}

func TestLongTextStopsAtTheTextCap(t *testing.T) {
	page := strings.Repeat("abcdefghij ", 25<<10)
	text, err := pdftext.Extract(t.Context(), build([]string{page, page, page}))
	if err != nil {
		t.Fatal(err)
	}
	body, marker, found := strings.Cut(text, "\n\n[The text stops here")
	if !found || len(body) > pdftext.MaximumTextBytes || len(body) < pdftext.MaximumTextBytes-16 {
		t.Fatalf("text cap not applied: body %d bytes, marker found %v", len(body), found)
	}
	if !strings.Contains(marker, "first 512 KB") {
		t.Fatalf("marker = %q", marker)
	}
}

func TestRefusals(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{
		{"scanned.pdf", pdftext.ErrNoText},
		{"encrypted.pdf", pdftext.ErrEncrypted},
		{"truncated.pdf", pdftext.ErrMalformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			text, err := pdftext.Extract(t.Context(), fixture(t, test.name))
			if !errors.Is(err, test.want) || text != "" {
				t.Fatalf("Extract = %q, %v; want %v", text, err, test.want)
			}
		})
	}
}

func TestCancelledContextStopsExtraction(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := pdftext.Extract(ctx, fixture(t, "handbook.pdf")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Extract error = %v, want context.Canceled", err)
	}
}

func TestInflatingContentIsRefusedWithinTheWorkerLimits(t *testing.T) {
	document := bomb(t, 256)
	if len(document) > 1<<20 {
		t.Fatalf("the generated PDF is %d bytes; it should stay small", len(document))
	}
	started := time.Now()
	text, err := pdftext.Extract(t.Context(), document)
	if !errors.Is(err, pdftext.ErrMalformed) || text != "" {
		t.Fatalf("Extract = %d bytes of text, %v; want ErrMalformed", len(text), err)
	}
	t.Logf("refused a %d KB PDF that inflates to 256 MiB in %s: %v", len(document)>>10, time.Since(started), err)
}

func TestCancelledExtractionDoesNotHoldUpTheNext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := pdftext.Extract(ctx, bomb(t, 256)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Extract error = %v, want context.DeadlineExceeded", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := pdftext.Extract(ctx, fixture(t, "handbook.pdf")); err != nil {
		t.Fatalf("the next extraction failed: %v", err)
	}
}
