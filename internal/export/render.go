package export

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// pageUnits is how much of Telegram's 4096-unit message one preview page fills, leaving room
	// for its page number.
	pageUnits     = 3500
	previewLayout = "2006-01-02 15:04"
)

// previewText is the preview as plain text. It retells what the user's file holds, so it is sent
// with no markup read from it: a link, a quote, or code in a memory entry shows as written and can
// hide nothing.
func (s *Service) previewText(report Report) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Restore preview for your export of %s. Nothing changes until you confirm.\n", report.ExportedAt.In(s.cfg.Location).Format(previewLayout))
	for _, section := range report.Sections {
		builder.WriteString("\n" + section.Label + ": ")
		if section.Refused != "" {
			builder.WriteString("not restored, " + section.Refused + ".\n")
			continue
		}
		builder.WriteString(sectionCounts(section) + ".\n")
		for _, added := range section.Added {
			if added.Text == "" {
				builder.WriteString("+ " + added.Title + "\n")
				continue
			}
			builder.WriteString("+ " + added.Title + ":\n" + added.Text + "\n")
		}
		for _, skipped := range section.Skipped {
			builder.WriteString("Skipped: " + skipped.Title + ": " + skipped.Reason + "\n")
		}
	}
	if report.Additions() == 0 {
		builder.WriteString("\nEverything in this file is already here, so there is nothing to restore.")
	} else {
		builder.WriteString("\nOnly what is listed with + is added; nothing already here is changed.")
	}
	return builder.String()
}

func sectionCounts(section SectionResult) string {
	var parts []string
	if added := len(section.Added); added > 0 {
		parts = append(parts, fmt.Sprintf("%d to add", added))
	}
	if section.Present > 0 {
		parts = append(parts, fmt.Sprintf("%d already here", section.Present))
	}
	if skipped := len(section.Skipped); skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	if len(parts) == 0 {
		return "nothing in the file"
	}
	return strings.Join(parts, ", ")
}

// summaryText is the confirmation, short enough for one message whatever the file holds.
func (s *Service) summaryText(report Report) string {
	var parts []string
	for _, section := range report.Sections {
		if added := len(section.Added); added > 0 && section.Refused == "" {
			parts = append(parts, fmt.Sprintf("%s %d", section.Label, added))
		}
	}
	return fmt.Sprintf("Restore %s from your export of %s: %s. Everything it adds is listed above.",
		records(report.Additions()), report.ExportedAt.In(s.cfg.Location).Format(previewLayout), strings.Join(parts, ", "))
}

// restoredText is the outcome of a confirmed restore.
func restoredText(report Report) string {
	var restored, failed []string
	skipped := 0
	for _, section := range report.Sections {
		if section.Refused != "" {
			continue
		}
		if section.Failed {
			failed = append(failed, section.Label)
			continue
		}
		skipped += len(section.Skipped)
		if added := len(section.Added); added > 0 {
			restored = append(restored, fmt.Sprintf("%s %d", section.Label, added))
		}
	}
	var text string
	if len(restored) == 0 {
		text = "Nothing was added."
	} else {
		text = "Restored " + records(report.Additions()) + ": " + strings.Join(restored, ", ") + "."
	}
	if skipped > 0 {
		text += fmt.Sprintf(" Skipped %s.", records(skipped))
	}
	if len(failed) > 0 {
		text += " Could not restore " + strings.Join(failed, ", ") + ", so nothing of it was added; try again."
	}
	return text
}

func records(count int) string {
	if count == 1 {
		return "1 record"
	}
	return fmt.Sprintf("%d records", count)
}

// pages splits the preview into messages at line breaks, cutting a line only when it alone is
// longer than a page, and numbers them when there is more than one.
func pages(text string) []string {
	var result []string
	var page strings.Builder
	units := 0
	flush := func() {
		if page.Len() > 0 {
			result = append(result, strings.TrimRight(page.String(), "\n"))
			page.Reset()
			units = 0
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		for _, part := range cut(line + "\n") {
			cost := utf16Units(part)
			if units+cost > pageUnits {
				flush()
			}
			page.WriteString(part)
			units += cost
		}
	}
	flush()
	if len(result) > 1 {
		for index := range result {
			result[index] = fmt.Sprintf("(%d/%d) %s", index+1, len(result), result[index])
		}
	}
	return result
}

// cut splits a line longer than a page into pieces that fit one.
func cut(line string) []string {
	if utf16Units(line) <= pageUnits {
		return []string{line}
	}
	var parts []string
	var part strings.Builder
	units := 0
	for _, r := range line {
		cost := utf16.RuneLen(r)
		if cost < 0 {
			cost = 1
		}
		if units+cost > pageUnits {
			parts = append(parts, part.String())
			part.Reset()
			units = 0
		}
		part.WriteRune(r)
		units += cost
	}
	return append(parts, part.String())
}

func utf16Units(text string) int {
	units := 0
	for _, r := range text {
		if r == utf8.RuneError {
			units++
			continue
		}
		cost := utf16.RuneLen(r)
		if cost < 0 {
			cost = 1
		}
		units += cost
	}
	return units
}
