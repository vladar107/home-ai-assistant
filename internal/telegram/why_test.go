package telegram

import "testing"

// Only questions about where a message came from are taken as asking for its record; the case,
// spacing, and punctuation or emoji after them do not matter, and a bare "why?" is left to the
// conversation.
func TestIsProvenanceQuestion(t *testing.T) {
	for text, want := range map[string]bool{
		"How do you know?":           true,
		"  how   do you KNOW that?!": true,
		"Where is that from? 🤔":      true,
		"what’s that based on?":      true,
		"Source?":                    true,
		"ОТКУДА ТЫ ЭТО ЗНАЕШЬ??":     true,
		"На чём это основано?":       true,
		"Hoe weet je dat?":           true,
		"why?":                       false,
		"Why":                        false,
		"почему?":                    false,
		"waarom?":                    false,
		"how do you know the way?":   false,
		"":                           false,
	} {
		if got := isProvenanceQuestion(text); got != want {
			t.Errorf("isProvenanceQuestion(%q) = %v, want %v", text, got, want)
		}
	}
}
