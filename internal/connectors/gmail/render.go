package gmail

import (
	"encoding/base64"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maximumReadBytes    = 12 << 10
	maximumSnippetRunes = 300
	maximumHeaderRunes  = 300
)

var (
	// shownLabels are the system labels worth telling the model about; user labels arrive as
	// opaque ids and are left out.
	shownLabels = map[string]bool{
		"UNREAD": true, "IMPORTANT": true, "STARRED": true, "INBOX": true, "SENT": true, "DRAFT": true, "SPAM": true,
		"CATEGORY_PERSONAL": true, "CATEGORY_SOCIAL": true, "CATEGORY_PROMOTIONS": true, "CATEGORY_UPDATES": true, "CATEGORY_FORUMS": true,
	}

	hiddenElements = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)\s*>`)
	lineBreaks     = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6]|/table|hr)\b[^>]*>`)
	tags           = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRuns      = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankLines     = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

func renderSearch(query string, messages []Message, more bool, failures []string) string {
	var builder strings.Builder
	if len(messages) == 0 {
		fmt.Fprintf(&builder, "No messages match %q.", query)
	} else {
		fmt.Fprintf(&builder, "%d messages match %q, newest first", len(messages), query)
		if more {
			builder.WriteString(" (more exist; narrow the search to see others)")
		}
		builder.WriteString(":\n")
	}
	for _, message := range messages {
		builder.WriteString("\n")
		writeHeaders(&builder, message)
		if snippet := strings.TrimSpace(html.UnescapeString(message.Snippet)); snippet != "" {
			fmt.Fprintf(&builder, "Snippet: %s\n", truncateRunes(snippet, maximumSnippetRunes))
		}
	}
	for _, failure := range failures {
		fmt.Fprintf(&builder, "\n%s\n", failure)
	}
	return builder.String()
}

func writeHeaders(builder *strings.Builder, message Message) {
	fmt.Fprintf(builder, "id %s (thread %s)\n", message.ID, message.ThreadID)
	if message.Account != "" {
		fmt.Fprintf(builder, "Account: %s\n", message.Account)
	}
	for _, name := range metadataHeaders {
		if value := header(message.Payload, name); value != "" {
			fmt.Fprintf(builder, "%s: %s\n", name, truncateRunes(value, maximumHeaderRunes))
		}
	}
	if received := receivedAt(message); received != "" {
		fmt.Fprintf(builder, "Received: %s\n", received)
	}
	var labels []string
	for _, label := range message.LabelIDs {
		if shownLabels[label] {
			labels = append(labels, label)
		}
	}
	if len(labels) > 0 {
		fmt.Fprintf(builder, "Labels: %s\n", strings.Join(labels, ", "))
	}
}

// renderMessage prints the headers, the attachments by name, and a window of the body text.
func renderMessage(message Message, offset int) string {
	var builder strings.Builder
	writeHeaders(&builder, message)
	var attachments []string
	collectAttachments(message.Payload, &attachments)
	if len(attachments) > 0 {
		fmt.Fprintf(&builder, "Attachments (not readable): %s\n", strings.Join(attachments, ", "))
	}

	body := messageText(message.Payload)
	if strings.TrimSpace(body) == "" {
		builder.WriteString("\nThe message has no readable text.")
		return builder.String()
	}
	if offset >= len(body) {
		fmt.Fprintf(&builder, "\nThe text ends before that position; it is %d bytes.", len(body))
		return builder.String()
	}
	window := body[offset:]
	truncated := len(window) > maximumReadBytes
	if truncated {
		window = window[:maximumReadBytes]
	}
	window = wholeRunes(window)
	next := offset + len(window)
	builder.WriteString("\n")
	builder.WriteString(strings.ToValidUTF8(window, ""))
	if truncated {
		fmt.Fprintf(&builder, "\n\n[truncated: %d of %d bytes shown; continue with offset_bytes %d]", next, len(body), next)
	}
	return builder.String()
}

// messageText prefers the plain-text alternative and falls back to HTML reduced to text.
func messageText(part Part) string {
	if text := firstBody(part, "text/plain"); text != "" {
		return strings.TrimSpace(text)
	}
	if markup := firstBody(part, "text/html"); markup != "" {
		return htmlToText(markup)
	}
	return ""
}

func firstBody(part Part, mimeType string) string {
	if strings.EqualFold(part.MimeType, mimeType) && part.Filename == "" && part.Body.Data != "" {
		if decoded, err := decodeBody(part.Body.Data); err == nil {
			return decoded
		}
	}
	for _, child := range part.Parts {
		if text := firstBody(child, mimeType); text != "" {
			return text
		}
	}
	return ""
}

func decodeBody(data string) (string, error) {
	decoded, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "="))
	}
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(decoded), "�"), nil
}

func htmlToText(markup string) string {
	text := hiddenElements.ReplaceAllString(markup, "")
	text = lineBreaks.ReplaceAllString(text, "\n")
	text = tags.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = blankRuns.ReplaceAllString(text, " ")
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}
	text = blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(text)
}

func collectAttachments(part Part, names *[]string) {
	if part.Filename != "" {
		*names = append(*names, fmt.Sprintf("%s (%s)", truncateRunes(part.Filename, 100), formatSize(part.Body.Size)))
	}
	for _, child := range part.Parts {
		collectAttachments(child, names)
	}
}

func header(part Part, name string) string {
	for _, candidate := range part.Headers {
		if strings.EqualFold(candidate.Name, name) {
			return strings.TrimSpace(candidate.Value)
		}
	}
	return ""
}

func receivedAt(message Message) string {
	millis, err := strconv.ParseInt(message.InternalDate, 10, 64)
	if err != nil || millis <= 0 {
		return ""
	}
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}

func formatSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%d KB", size>>10)
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// wholeRunes drops a rune cut in half at the end of a window, so the next window starts on it.
func wholeRunes(text string) string {
	for cut := 0; cut < utf8.UTFMax && len(text) > 0; cut++ {
		if last, size := utf8.DecodeLastRuneInString(text); last != utf8.RuneError || size > 1 {
			break
		}
		text = text[:len(text)-1]
	}
	return text
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}
