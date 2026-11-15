package prompt

import (
	"strings"
	"testing"
)

func TestSectionsWithReplacesOneSectionOnly(t *testing.T) {
	for _, name := range SectionNames() {
		replaced, ok := Default.With(name, "REPLACED ")
		if !ok {
			t.Fatalf("%s is not a section", name)
		}
		if replaced == Default {
			t.Errorf("%s changed nothing", name)
		}
	}
	if _, ok := Default.With("secrecy", "x"); ok {
		t.Error("the secrecy section can be replaced")
	}
	if _, ok := Default.With("nonsense", "x"); ok {
		t.Error("an unknown section was accepted")
	}
}

func TestDefaultSectionsRenderTheProductionPrompt(t *testing.T) {
	for _, tc := range []struct {
		tools, web bool
		guidance   string
	}{{false, false, ""}, {false, true, ""}, {true, false, ""}, {true, true, "Guidance."}} {
		if got, want := Default.System(tc.tools, tc.guidance, tc.web), System(tc.tools, tc.guidance, tc.web); got != want {
			t.Errorf("tools=%v web=%v: %q != %q", tc.tools, tc.web, got, want)
		}
	}
	if !strings.HasSuffix(System(false, "", false), secrecy) {
		t.Error("the prompt does not end with the secrecy sentence")
	}
}

func TestVariantSectionChangesOnlyItsPrompt(t *testing.T) {
	variant, _ := Default.With("initiative", "Be brief. ")
	if variant.System(true, "", false) == System(true, "", false) {
		t.Error("the initiative section did not change the tools prompt")
	}
	if variant.System(false, "", false) != System(false, "", false) {
		t.Error("the initiative section changed the no-tools prompt")
	}
}
