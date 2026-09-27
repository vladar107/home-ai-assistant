package web

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/vladar107/home-ai-assistant/internal/pdftext"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	// maximumReadBytes matches the Gmail, attachment, and NAS readers, so every window of text
	// the model reads is the same size.
	maximumReadBytes = 12 << 10
	maximumTextBytes = 2 << 20
	// maximumPDFBytes matches the attachment file limit; a PDF is read whole to be parsed.
	maximumPDFBytes   = 10 << 20
	maximumURLBytes   = 2048
	maximumTitleBytes = 300

	pageAccept = "text/html,application/xhtml+xml,text/plain;q=0.9,text/markdown;q=0.9,application/json;q=0.9,application/pdf;q=0.9,*/*;q=0.1"
)

type pageKind int

const (
	kindUnreadable pageKind = iota
	kindHTML
	kindText
	kindPDF
)

var mediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)

// page is the readable text of one fetched address.
type page struct {
	finalURL  string
	title     string
	text      string
	extracted bool
}

// fetchPage fetches an address and returns one window of its text. A failure is a result the
// model can act on; nothing from the page is kept, so each window fetches the page again.
func (s *Service) fetchPage(ctx context.Context, principalID, address string, offset int) tools.Result {
	address = strings.TrimSpace(address)
	if address == "" || len(address) > maximumURLBytes {
		return errorResult(fmt.Sprintf("url must be an http or https address of at most %d characters", maximumURLBytes))
	}
	target, err := url.Parse(address)
	if err != nil {
		return errorResult("url is not a valid address")
	}
	target.Fragment = ""
	result, err := s.readPage(ctx, target)
	if err != nil {
		var refusal pageRefusal
		if errors.As(err, &refusal) {
			return errorResult(string(refusal))
		}
		s.logger.InfoContext(ctx, "web.fetch.failed", "principal_id", principalID, "error_class", errorClass(err))
		return errorResult(failureText(err))
	}
	return renderWindow(result, offset)
}

// pageRefusal is a failure already worded for the model.
type pageRefusal string

func (r pageRefusal) Error() string { return string(r) }

func (s *Service) readPage(ctx context.Context, target *url.URL) (page, error) {
	resp, err := s.guard.get(ctx, target, pageAccept)
	if err != nil {
		return page{}, err
	}
	defer resp.body.Close()
	if resp.status < 200 || resp.status > 299 {
		return page{}, pageRefusal(fmt.Sprintf("The address returned HTTP status %d, so there is no page to read.", resp.status))
	}

	mediaType, params, err := mime.ParseMediaType(resp.contentType)
	if err != nil {
		mediaType = ""
	}
	body := bufio.NewReaderSize(resp.body, 512)
	kind := classify(mediaType, resp.finalURL)
	if mediaType == "" && kind == kindUnreadable {
		sniffed, _ := body.Peek(512)
		mediaType, params, _ = mime.ParseMediaType(http.DetectContentType(sniffed))
		kind = classify(mediaType, resp.finalURL)
	}
	if kind == kindUnreadable {
		described := "an unknown type of"
		if mediaTypePattern.MatchString(mediaType) {
			described = mediaType
		}
		return page{}, pageRefusal(fmt.Sprintf("The address returns %s content, which cannot be read as text.", described))
	}

	limit, limitText := int64(maximumTextBytes), "2 MiB"
	if kind == kindPDF {
		limit, limitText = maximumPDFBytes, "10 MiB"
	}
	data, err := readLimited(body, limit)
	if errors.Is(err, errTooLarge) {
		return page{}, pageRefusal(fmt.Sprintf("The page is larger than the %s download limit, so it was not read.", limitText))
	}
	if err != nil {
		return page{}, err
	}

	result := page{finalURL: displayURL(resp.finalURL)}
	switch kind {
	case kindHTML:
		reader, err := charset.NewReader(bytes.NewReader(data), resp.contentType)
		if err != nil {
			return page{}, pageRefusal("The page uses a text encoding that cannot be read.")
		}
		title, text, err := reduceHTML(reader, resp.finalURL)
		if err != nil {
			return page{}, pageRefusal("The page's HTML could not be read.")
		}
		result.title, result.text = clip(strings.ToValidUTF8(title, ""), maximumTitleBytes), strings.ToValidUTF8(text, "")
		if result.text == "" {
			return page{}, pageRefusal("The page has no readable text; it may need JavaScript to show its content, which cannot be run here.")
		}
	case kindText:
		text, err := decodeText(data, params["charset"])
		if err != nil {
			return page{}, err
		}
		if strings.TrimSpace(text) == "" {
			return page{}, pageRefusal("The page is empty.")
		}
		result.text = text
	case kindPDF:
		text, err := pdftext.Extract(ctx, data)
		if err != nil {
			return page{}, pdfFailure(err)
		}
		result.text, result.extracted = text, true
	}
	return result, nil
}

func classify(mediaType string, final *url.URL) pageKind {
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		return kindHTML
	case mediaType == "text/plain" || mediaType == "text/markdown" || mediaType == "text/x-markdown" ||
		mediaType == "text/csv" || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return kindText
	case mediaType == "application/pdf" || mediaType == "application/x-pdf":
		return kindPDF
	case (mediaType == "" || mediaType == "application/octet-stream") && strings.HasSuffix(strings.ToLower(final.Path), ".pdf"):
		return kindPDF
	}
	return kindUnreadable
}

// decodeText returns a text response as UTF-8, converting it from the charset it declares.
func decodeText(data []byte, label string) (string, error) {
	label = strings.ToLower(strings.TrimSpace(label))
	if label != "" && label != "utf-8" && label != "utf8" && label != "us-ascii" {
		encoding, _ := charset.Lookup(label)
		if encoding == nil {
			return "", pageRefusal("The page uses a text encoding that cannot be read.")
		}
		decoded, err := encoding.NewDecoder().Bytes(data)
		if err != nil {
			return "", pageRefusal("The page uses a text encoding that cannot be read.")
		}
		data = decoded
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return "", pageRefusal("The page is not valid UTF-8 text, so it cannot be read.")
	}
	return string(data), nil
}

func pdfFailure(err error) error {
	switch {
	case errors.Is(err, pdftext.ErrNoText):
		return pageRefusal("The PDF at that address has no text layer; it is probably a scanned document, so its content cannot be read as text.")
	case errors.Is(err, pdftext.ErrEncrypted):
		return pageRefusal("The PDF at that address is password-protected, so its content cannot be read.")
	case errors.Is(err, pdftext.ErrMalformed):
		return pageRefusal("The file at that address is damaged or not a valid PDF, so its content cannot be read.")
	case errors.Is(err, context.DeadlineExceeded):
		return pageRefusal("The PDF at that address could not be read in time.")
	}
	return err
}

// renderWindow serves one window of a page's text with a header saying where it came from and
// how to continue. For a PDF the offsets count bytes of the extracted text, not of the file.
func renderWindow(result page, offset int) tools.Result {
	if offset >= len(result.text) {
		return errorResult(fmt.Sprintf("The page's text ends before that position; it is %d bytes.", len(result.text)))
	}
	for offset > 0 && !utf8.RuneStart(result.text[offset]) {
		offset++
	}
	chunk := result.text[offset:]
	truncated := len(chunk) > maximumReadBytes
	if truncated {
		chunk = clip(chunk, maximumReadBytes)
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "Page: %s\n", result.finalURL)
	if result.title != "" {
		fmt.Fprintf(&builder, "Title: %s\n", result.title)
	}
	if result.extracted {
		builder.WriteString("Text extracted from the PDF.\n")
	}
	end := offset + len(chunk)
	switch {
	case truncated:
		fmt.Fprintf(&builder, "Bytes %d to %d of %d of the text; continue with offset_bytes %d.\n", offset, end, len(result.text), end)
	case offset > 0:
		fmt.Fprintf(&builder, "Bytes %d to %d of %d of the text, the end of the page.\n", offset, end, len(result.text))
	}
	builder.WriteString("\n")
	builder.WriteString(chunk)
	return tools.Result{Text: builder.String()}
}

func displayURL(final *url.URL) string {
	shown := *final
	shown.User = nil
	return clip(shown.String(), maximumURLBytes)
}

// clip cuts text to at most limit bytes on a rune boundary.
func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// errorClass names a failure for the log without its text, which can carry the address.
func errorClass(err error) string {
	switch {
	case errors.Is(err, errPrivateAddress):
		return "private_address"
	case errors.Is(err, errUnsupportedURL):
		return "unsupported_url"
	case errors.Is(err, errTooManyRedirects):
		return "too_many_redirects"
	case errors.Is(err, errUnresolvable):
		return "unresolvable"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "timeout"
	default:
		return "unreachable"
	}
}

func errorResult(text string) tools.Result {
	return tools.Result{Text: text, IsError: true}
}
