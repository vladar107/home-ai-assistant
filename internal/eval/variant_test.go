package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

func writeVariant(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadVariantValidates(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown-section":  {`{"description":"d","sections":{"nonsense":"x"}}`, "sections.nonsense: unknown section"},
		"secrecy-locked":   {`{"description":"d","sections":{"secrecy":"x"}}`, "sections.secrecy: unknown section"},
		"unknown-field":    {`{"description":"d","sections":{"intro":"x"},"extra":1}`, `unknown field "extra"`},
		"no-description":   {`{"sections":{"intro":"x"}}`, "description is required"},
		"no-overrides":     {`{"description":"d"}`, "must override at least"},
		"empty-section":    {`{"description":"d","sections":{"intro":" "}}`, "sections.intro: empty text"},
		"malformed":        {`{`, "decode"},
		"two-documents":    {`{"description":"d","effort":"low"} {}`, "more than one JSON value"},
		"empty-tool-names": {`{"description":"d","tool_guidance":{" ":"x"}}`, "empty tool name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadVariant(writeVariant(t, name, tc.body), name)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), name+".json") && name != "malformed" {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := LoadVariant(t.TempDir(), "Bad Name"); err == nil {
		t.Error("an invalid variant name was accepted")
	}
	if _, err := LoadVariant(t.TempDir(), "missing"); err == nil {
		t.Error("a missing variant was accepted")
	}
}

func TestLoadVariantRecordsTheFileHash(t *testing.T) {
	dir := writeVariant(t, "brief", `{"description":"d","sections":{"intro":"Hi. "},"model":"m","effort":"high"}`)
	a, err := LoadVariant(dir, "brief")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.SHA256) != 64 || a.Info().Name != "brief" || a.Model != "m" || a.Effort != "high" {
		t.Fatalf("variant = %+v", a)
	}
	if err := os.WriteFile(filepath.Join(dir, "brief.json"), []byte(`{"description":"d","sections":{"intro":"Hello. "}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := LoadVariant(dir, "brief")
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 == b.SHA256 {
		t.Error("the hash did not change with the file")
	}
}

func TestRepositoryVariantsLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "evals", "variants")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
			if _, err := LoadVariant(dir, name); err != nil {
				t.Error(err)
			}
		}
	}
}

func TestLookBeforeAskingOnlyAddsToTheInitiativeSection(t *testing.T) {
	v, err := LoadVariant(filepath.Join("..", "..", "evals", "variants"), "look-before-asking")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Sections) != 1 || len(v.ToolGuidance) != 0 {
		t.Fatalf("variant = %+v", v)
	}
	text := v.Sections["initiative"]
	start := strings.Index(text, "Before asking the user")
	end := strings.Index(text, "Take the next step")
	if start < 0 || end < start || text[:start]+text[end:] != prompt.Default.Initiative {
		t.Fatalf("the variant is not the default initiative section plus one added passage:\n%s", text)
	}
}

func TestFingerprintFollowsSectionsAndGuidanceNotMemory(t *testing.T) {
	base := agentruntime.RunRequest{Tools: agentruntime.ToolSet{
		Definitions: []agentruntime.ToolDefinition{{Name: "t"}}, Guidance: "Guidance.",
	}}
	reference := Fingerprint(base)
	if len(reference) != 64 {
		t.Fatalf("fingerprint = %q", reference)
	}

	withMemory := base
	withMemory.Memory, withMemory.Jobs = "Likes tea.", "A job."
	if Fingerprint(withMemory) != reference {
		t.Error("memory and jobs changed the fingerprint")
	}

	sections, _ := prompt.Default.With("initiative", "Be brief. ")
	withSection := base
	withSection.PromptSections = &sections
	if Fingerprint(withSection) == reference {
		t.Error("a replaced section left the fingerprint unchanged")
	}

	withGuidance := base
	withGuidance.Tools.Guidance = "Other guidance."
	if Fingerprint(withGuidance) == reference {
		t.Error("replaced tool guidance left the fingerprint unchanged")
	}

	defaults := prompt.Default
	explicit := base
	explicit.PromptSections = &defaults
	if Fingerprint(explicit) != reference {
		t.Error("the default sections set explicitly changed the fingerprint")
	}
}

func TestWorldAppliesAVariantOnlyWhenGiven(t *testing.T) {
	c := Case{
		ID: "variant-world", Title: "Variant world", Kind: KindHappy, Clock: Clock{Local: clockLocal, Zone: clockZone},
		Steps: []Step{{Say: "hello"}}, Trials: 1,
	}
	offered := worldOffers(t, c)
	if len(offered) == 0 {
		t.Fatal("the world offers no tools")
	}
	tool := offered[0]

	plain := newCaseWorld(t, c, "").play(Step{Say: "hello"}).Runs[0].Request
	if plain.PromptSections != nil {
		t.Fatal("a world without a variant set sections on the request")
	}

	v := &Variant{
		Name: "v", Description: "d", Sections: map[string]string{"initiative": "Be brief. "},
		ToolGuidance: map[string]string{tool: "REPLACED GUIDANCE"},
	}
	varied := newVariantWorld(t, c, "", v).play(Step{Say: "hello"}).Runs[0].Request
	if varied.PromptSections == nil || varied.PromptSections.Initiative != "Be brief. " || varied.PromptSections.Intro != prompt.Default.Intro {
		t.Fatalf("sections = %+v", varied.PromptSections)
	}
	if !strings.Contains(varied.Tools.Guidance, "REPLACED GUIDANCE") || strings.Contains(plain.Tools.Guidance, "REPLACED GUIDANCE") {
		t.Fatalf("guidance: variant %q, default %q", varied.Tools.Guidance, plain.Tools.Guidance)
	}
	if Fingerprint(varied) == Fingerprint(plain) {
		t.Error("the variant has the production fingerprint")
	}
	if !strings.Contains(varied.SystemPrompt(), "Be brief. ") || strings.Contains(plain.SystemPrompt(), "Be brief. ") {
		t.Error("the rendered prompts do not follow the variant")
	}

	guidanceOnly := &Variant{Name: "g", Description: "d", ToolGuidance: map[string]string{tool: "REPLACED GUIDANCE"}}
	only := newVariantWorld(t, c, "", guidanceOnly).play(Step{Say: "hello"}).Runs[0].Request
	if only.PromptSections != nil || Fingerprint(only) == Fingerprint(plain) {
		t.Error("a guidance-only variant must keep the sections and still have its own fingerprint")
	}
}

func TestValidateToolsNamesTheUnknownTool(t *testing.T) {
	v := &Variant{File: "v.json", ToolGuidance: map[string]string{"known": "x", "unknown_tool": "y"}}
	err := v.ValidateTools([]string{"known"})
	if err == nil || !strings.Contains(err.Error(), "unknown_tool") || !strings.Contains(err.Error(), "v.json") {
		t.Fatalf("got %v", err)
	}
	if err := v.ValidateTools([]string{"known", "unknown_tool"}); err != nil {
		t.Fatal(err)
	}
}
