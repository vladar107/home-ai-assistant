package pi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

func TestSystemPromptSectionsReachPiOnlyWhenTheRequestSetsThem(t *testing.T) {
	cfg := config(t)
	r := newRuntime(t, cfg)
	if _, err := r.Run(context.Background(), request("default-sections", "hello"), &eventSink{}); err != nil {
		t.Fatal(err)
	}
	if got, want := readReport(t, cfg, testPrincipal, "default-sections").SystemPrompt, prompt.System(false, "", false); got != want {
		t.Errorf("default prompt = %q, want %q", got, want)
	}

	variant, ok := prompt.Default.With("no_tools", "Variant text. ")
	if !ok {
		t.Fatal("no_tools is not a section")
	}
	changed := request("variant-sections", "hello")
	changed.PromptSections = &variant
	changed.Memory = "Memory line."
	if _, err := r.Run(context.Background(), changed, &eventSink{}); err != nil {
		t.Fatal(err)
	}
	want := prompt.WithSections(variant.System(false, "", false), "Memory line.")
	if got := readReport(t, cfg, testPrincipal, "variant-sections").SystemPrompt; got != want || !strings.Contains(got, "Variant text. ") {
		t.Errorf("variant prompt = %q, want %q", got, want)
	}
}
