package prompt

import "testing"

// A reply that is only the sentinel stays silent however the model spells or wraps it; a reply
// with any other text is sent.
func TestSilentToleratesTheSentinelsVariantsButNotOtherText(t *testing.T) {
	for _, reply := range []string{
		"[nothing-to-add]",
		"  [nothing-to-add]\n",
		"[Nothing-to-add]",
		"[NOTHING-TO-ADD]",
		"[nothing-to-add].",
		"[ nothing-to-add ]",
		"\"[nothing-to-add]\"",
		"nothing-to-add",
		"[nothing-to-add] [nothing-to-add]",
	} {
		if !Silent(reply) {
			t.Errorf("Silent(%q) = false, want true", reply)
		}
	}
	for _, reply := range []string{
		"",
		"Nothing to add.",
		"Done. [nothing-to-add]",
		"[nothing-to-add] The gate is locked.",
		"[The unlock is ready for your confirmation.]",
		"nothing-to-add-yet",
	} {
		if Silent(reply) {
			t.Errorf("Silent(%q) = true, want false", reply)
		}
	}
}
