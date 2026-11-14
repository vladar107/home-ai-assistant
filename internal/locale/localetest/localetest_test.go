package localetest

import (
	"slices"
	"testing"
)

func TestLatinFlagsWordsWrittenOnlyInLatinLetters(t *testing.T) {
	got := Latin("Включено: /settings и Telegram, задача x в Notion, Undo-кнопка", "telegram", "notion")
	if want := []string{"settings", "Undo"}; !slices.Equal(got, want) {
		t.Fatalf("Latin = %v, want %v", got, want)
	}
	if got := Latin("Привет, мир 12"); len(got) != 0 {
		t.Fatalf("Latin flagged %v in Russian text", got)
	}
}
