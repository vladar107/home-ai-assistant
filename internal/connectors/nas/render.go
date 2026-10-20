package nas

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	// maximumEntries and maximumListingBytes both bound one listing, because a folder can be long
	// in either sense: many entries, or few entries with very long names. Without the byte bound a
	// listing could exceed the broker's result limit, which the model cannot recover from because
	// listing takes no cursor.
	maximumEntries      = 100
	maximumListingBytes = 10 << 10
	// maximumReadBytes matches the attachment reader, so a file read from the NAS and the same
	// file sent through Telegram behave alike.
	maximumReadBytes = 12 << 10
	// maximumPDFBytes matches the attachment file limit; a PDF is read whole to be parsed.
	maximumPDFBytes = 10 << 20
	maximumHits     = 25
)

// renderRoots lists the places this principal can reach, marking whose they are and which a
// confirmed save can write to so the model does not propose a save into a read-only folder.
func renderRoots(roots []Root) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Folders on the NAS this user can reach: %d\n", len(roots))
	if len(roots) == 0 {
		builder.WriteString("(none)\n")
	}
	for _, root := range roots {
		owner, access := "yours", "read-only"
		if root.Shared {
			owner = "shared with the household"
		}
		if root.Writable {
			access = "can be saved to"
		}
		fmt.Fprintf(&builder, "- [folder, %s, %s] %s\n", owner, access, root.Name)
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderListing(location Location, entries []Entry) string {
	var builder strings.Builder
	shown := entries
	if len(shown) > maximumEntries {
		shown = shown[:maximumEntries]
	}
	fmt.Fprintf(&builder, "%s: %d entries", location, len(entries))
	if len(shown) < len(entries) {
		fmt.Fprintf(&builder, ", showing the first %d", len(shown))
	}
	builder.WriteString("\n")
	if len(entries) == 0 {
		builder.WriteString("(empty)\n")
	}
	written := 0
	for _, found := range shown {
		line := renderEntry(location.Child(found.Name), found)
		if builder.Len()+len(line) > maximumListingBytes {
			fmt.Fprintf(&builder, "(%d more entries not shown; list a subfolder to see them)\n", len(entries)-written)
			break
		}
		builder.WriteString(line)
		written++
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderSearch(root Location, query string, hits []hit, truncated bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Names matching %q under %s: %d", query, root, len(hits))
	if truncated {
		builder.WriteString(" (more matches exist; narrow the query or search a subfolder)")
	}
	builder.WriteString("\n")
	if len(hits) == 0 {
		builder.WriteString("(none)\n")
	}
	for index, found := range hits {
		line := renderEntry(found.Location, found.Entry)
		if builder.Len()+len(line) > maximumListingBytes {
			fmt.Fprintf(&builder, "(%d more matches not shown; narrow the query)\n", len(hits)-index)
			break
		}
		builder.WriteString(line)
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderEntry(location Location, found Entry) string {
	kind := "file"
	if found.Directory {
		kind = "folder"
	}
	line := fmt.Sprintf("- [%s] %s", kind, location)
	if !found.Directory {
		line += "; " + formatSize(found.SizeBytes)
	}
	if !found.Modified.IsZero() {
		line += "; modified " + found.Modified.UTC().Format(time.RFC3339)
	}
	return line + "\n"
}

func renderRead(location Location, chunk []byte, offset, size int64) string {
	header := fmt.Sprintf("%s (%s)", location, formatSize(size))
	end := offset + int64(len(chunk))
	if end < size {
		header += fmt.Sprintf(", bytes %d to %d of %d; continue with offset_bytes %d", offset, end, size, end)
	}
	return header + ":\n" + string(chunk)
}

// renderPDFRead is renderRead for a PDF, whose offsets count bytes of the extracted text rather
// than of the file.
func renderPDFRead(location Location, chunk []byte, offset, textSize, fileSize int64) string {
	header := fmt.Sprintf("%s (PDF, %s), text extracted from the PDF", location, formatSize(fileSize))
	end := offset + int64(len(chunk))
	if end < textSize {
		header += fmt.Sprintf(", bytes %d to %d of %d of the extracted text; continue with offset_bytes %d", offset, end, textSize, end)
	}
	return header + ":\n" + string(chunk)
}

func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

func isPDF(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".pdf")
}

// readableAsText decides from the name alone whether a file is worth reading as text. The NAS
// reports no media type, so the extension is the only hint before the bytes themselves are
// checked for valid UTF-8.
func readableAsText(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".jsonl", ".yaml", ".yml", ".xml",
		".log", ".ini", ".toml", ".conf", ".sh", ".sql", ".go", ".py", ".js", ".ts", ".html", ".css":
		return true
	}
	return false
}
