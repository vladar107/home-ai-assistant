package export

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	// pageUnits is how much of Telegram's 4096-unit message one preview page fills, leaving room
	// for its page number.
	pageUnits     = 3500
	previewLayout = "2006-01-02 15:04"
)

var (
	previewHeading    = locale.T("Restore preview for your export of %s. Nothing changes until you confirm.\n", "Предпросмотр восстановления из вашего экспорта от %s. Пока вы не подтвердите, ничего не изменится.\n")
	sectionRefused    = locale.T("not restored, %s.\n", "не восстановлено, %s.\n")
	skippedLine       = locale.T("Skipped: %s: %s\n", "Пропущено: %s: %s\n")
	allPresent        = locale.T("\nEverything in this file is already here, so there is nothing to restore.", "\nВсё, что есть в этом файле, уже есть здесь, поэтому восстанавливать нечего.")
	onlyListedAdded   = locale.T("\nOnly what is listed with + is added; nothing already here is changed.", "\nДобавляется только то, что отмечено знаком +; уже имеющееся не меняется.")
	countToAdd        = locale.N("%d to add", "%d to add", "%d к добавлению", "%d к добавлению", "%d к добавлению")
	countPresent      = locale.N("%d already here", "%d already here", "%d уже есть", "%d уже есть", "%d уже есть")
	countSkipped      = locale.N("%d skipped", "%d skipped", "%d пропущена", "%d пропущены", "%d пропущено")
	countNothing      = locale.T("nothing in the file", "в файле ничего нет")
	restoreSummary    = locale.T("Restore %s from your export of %s: %s. Everything it adds is listed above.", "Восстановить %s из вашего экспорта от %s: %s. Всё, что добавляется, перечислено выше.")
	restoredNothing   = locale.T("Nothing was added.", "Ничего не добавлено.")
	restoredLine      = locale.T("Restored %[1]s: %[2]s.", "Восстановлено: %[1]s (%[2]s).")
	restoredSkipped   = locale.T("Skipped %s.", "Пропущено: %s.")
	restoredFailed    = locale.T("Could not restore %s, so nothing of it was added; try again.", "Не удалось восстановить: %s; из этого ничего не добавлено, попробуйте ещё раз.")
	recordsCount      = locale.N("%d record", "%d records", "%d запись", "%d записи", "%d записей")
	sectionLabelTexts = map[string]locale.Text{
		"jobs":          locale.T("Scheduled jobs and reminders", "Задания по расписанию и напоминания"),
		"collections":   locale.T("Collections", "Коллекции"),
		"memory":        locale.T("Memory", "Память"),
		"settings":      locale.T("Memory settings", "Настройки памяти"),
		"places":        locale.T("Saved places", "Сохранённые места"),
		"intake":        locale.T("Kept facts from incoming content", "Сохранённые факты из входящего контента"),
		"projects":      locale.T("Projects", "Проекты"),
		"switches":      locale.T("Integration switches", "Переключатели интеграций"),
		"language":      locale.T("Interface language", "Язык интерфейса"),
		"trips":         locale.T("Trips", "Поездки"),
		"watches":       locale.T("Watches", "Наблюдения"),
		"event_sources": locale.T("Event sources", "Источники событий"),
		"routes":        locale.T("Saved routes", "Сохранённые маршруты"),
	}
)

// labelOf is a section's label in the context's language; a section this file does not know keeps
// the label its exporter gave.
func labelOf(ctx context.Context, section SectionResult) string {
	return labelNamed(ctx, section.Section, section.Label)
}

func labelNamed(ctx context.Context, name, label string) string {
	if text, known := sectionLabelTexts[name]; known {
		return text.Format(ctx)
	}
	return label
}

// previewText is the preview as plain text. It retells what the user's file holds, so it is sent
// with no markup read from it: a link, a quote, or code in a memory entry shows as written and can
// hide nothing.
func (s *Service) previewText(ctx context.Context, report Report) string {
	var builder strings.Builder
	builder.WriteString(previewHeading.Format(ctx, report.ExportedAt.In(s.cfg.Location).Format(previewLayout)))
	for _, section := range report.Sections {
		builder.WriteString("\n" + labelOf(ctx, section) + ": ")
		if section.Refused != "" {
			builder.WriteString(sectionRefused.Format(ctx, section.Refused))
			continue
		}
		builder.WriteString(sectionCounts(ctx, section) + ".\n")
		for _, added := range section.Added {
			if added.Text == "" {
				builder.WriteString("+ " + added.Title + "\n")
				continue
			}
			builder.WriteString("+ " + added.Title + ":\n" + added.Text + "\n")
		}
		for _, skipped := range section.Skipped {
			builder.WriteString(skippedLine.Format(ctx, skipped.Title, skipped.Reason))
		}
	}
	if report.Additions() == 0 {
		builder.WriteString(allPresent.Format(ctx))
	} else {
		builder.WriteString(onlyListedAdded.Format(ctx))
	}
	return builder.String()
}

func sectionCounts(ctx context.Context, section SectionResult) string {
	var parts []string
	if added := len(section.Added); added > 0 {
		parts = append(parts, countToAdd.Format(ctx, added))
	}
	if section.Present > 0 {
		parts = append(parts, countPresent.Format(ctx, section.Present))
	}
	if skipped := len(section.Skipped); skipped > 0 {
		parts = append(parts, countSkipped.Format(ctx, skipped))
	}
	if len(parts) == 0 {
		return countNothing.Format(ctx)
	}
	return strings.Join(parts, ", ")
}

// summaryText is the confirmation, short enough for one message whatever the file holds.
func (s *Service) summaryText(ctx context.Context, report Report) string {
	var parts []string
	for _, section := range report.Sections {
		if added := len(section.Added); added > 0 && section.Refused == "" {
			parts = append(parts, fmt.Sprintf("%s %d", labelOf(ctx, section), added))
		}
	}
	return restoreSummary.Format(ctx,
		recordsCount.Format(ctx, report.Additions()), report.ExportedAt.In(s.cfg.Location).Format(previewLayout), strings.Join(parts, ", "))
}

// restoredText is the outcome of a confirmed restore in the context's language. The model's record
// of what happened is the English one.
func restoredText(ctx context.Context, report Report) string {
	var restored, failed []string
	skipped := 0
	for _, section := range report.Sections {
		if section.Refused != "" {
			continue
		}
		if section.Failed {
			failed = append(failed, labelOf(ctx, section))
			continue
		}
		skipped += len(section.Skipped)
		if added := len(section.Added); added > 0 {
			restored = append(restored, fmt.Sprintf("%s %d", labelOf(ctx, section), added))
		}
	}
	var text string
	if len(restored) == 0 {
		text = restoredNothing.Format(ctx)
	} else {
		text = restoredLine.Format(ctx, recordsCount.Format(ctx, report.Additions()), strings.Join(restored, ", "))
	}
	if skipped > 0 {
		text += " " + restoredSkipped.Format(ctx, recordsCount.Format(ctx, skipped))
	}
	if len(failed) > 0 {
		text += " " + restoredFailed.Format(ctx, strings.Join(failed, ", "))
	}
	return text
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
