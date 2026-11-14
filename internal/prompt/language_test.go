package prompt

import (
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/locale"
)

// A user who never chose reads their unattended turns as before, so English adds no line.
func TestInterfaceLanguageIsToldOnlyWhenRussianWasChosen(t *testing.T) {
	if line := InterfaceLanguage(locale.English); line != "" {
		t.Fatalf("english line = %q", line)
	}
	checkGolden(t, "interface-language-russian", Prepend(InterfaceLanguage(locale.Russian), "Do the task."))
}
