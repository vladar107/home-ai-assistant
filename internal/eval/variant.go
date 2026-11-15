package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/prompt"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// DefaultVariantsDir is where variant files live.
const DefaultVariantsDir = "evals/variants"

var variantName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Variant is a named set of overrides of the assistant's fixed text: sections of the system
// prompt, the guidance of the connectors that offer named tools, and the default model and effort.
// It lives in evals/variants/<name>.json. Production never sees one: it reaches the runtime only
// through the harness, on RunRequest.PromptSections and the broker's guidance overrides.
type Variant struct {
	Name        string `json:"-"`
	Description string `json:"description"`
	// Sections maps a name from prompt.SectionNames to its replacement text.
	Sections map[string]string `json:"sections,omitempty"`
	// ToolGuidance maps the name of a tool to the text that replaces the guidance of the connector
	// offering it, write guidance included.
	ToolGuidance map[string]string `json:"tool_guidance,omitempty"`
	// Model and Effort replace the run's default model and effort; a case that pins its own keeps it.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`

	// File is the variant's path and SHA256 the hash of its bytes.
	File   string `json:"-"`
	SHA256 string `json:"-"`
}

// VariantInfo is how a result names the variant it ran.
type VariantInfo struct {
	Name        string `json:"name"`
	SHA256      string `json:"sha256"`
	Description string `json:"description,omitempty"`
}

// LoadVariant reads and validates evals/variants/<name>.json under the directory.
func LoadVariant(dir, name string) (*Variant, error) {
	if !variantName.MatchString(name) {
		return nil, fmt.Errorf("variant name %q: use lowercase letters, digits and dashes", name)
	}
	path := filepath.Join(dir, name+".json")
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("variant %q: %w", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	v := &Variant{Name: name, File: path}
	if err := decoder.Decode(v); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", path, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", path)
	}
	sum := sha256.Sum256(contents)
	v.SHA256 = hex.EncodeToString(sum[:])
	if err := v.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func (v *Variant) validate() error {
	if strings.TrimSpace(v.Description) == "" {
		return errors.New("description is required")
	}
	if len(v.Sections) == 0 && len(v.ToolGuidance) == 0 && v.Model == "" && v.Effort == "" {
		return errors.New("a variant must override at least a section, a tool's guidance, the model or the effort")
	}
	for _, name := range slices.Sorted(maps.Keys(v.Sections)) {
		if _, ok := prompt.Default.With(name, ""); !ok {
			return fmt.Errorf("sections.%s: unknown section; want one of %s", name, strings.Join(prompt.SectionNames(), ", "))
		}
		if strings.TrimSpace(v.Sections[name]) == "" {
			return fmt.Errorf("sections.%s: empty text", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(v.ToolGuidance)) {
		if strings.TrimSpace(name) == "" {
			return errors.New("tool_guidance: empty tool name")
		}
	}
	return nil
}

// ValidateTools checks that every tool named in tool_guidance is among the offered tool names.
func (v *Variant) ValidateTools(offered []string) error {
	for _, name := range slices.Sorted(maps.Keys(v.ToolGuidance)) {
		if !slices.Contains(offered, name) {
			return fmt.Errorf("%s: tool_guidance.%s names a tool no case's world offers", v.File, name)
		}
	}
	return nil
}

// Info is the variant as a result records it; nil for no variant.
func (v *Variant) Info() *VariantInfo {
	if v == nil {
		return nil
	}
	return &VariantInfo{Name: v.Name, SHA256: v.SHA256, Description: v.Description}
}

// promptSections is the section set requests carry; nil when the variant replaces none.
func (v *Variant) promptSections() *prompt.Sections {
	if v == nil || len(v.Sections) == 0 {
		return nil
	}
	sections := prompt.Default
	for name, text := range v.Sections {
		sections, _ = sections.With(name, text)
	}
	return &sections
}

func (v *Variant) guidance() map[string]string {
	if v == nil {
		return nil
	}
	return v.ToolGuidance
}

// OfferedTools builds the case's world on a runtime that never calls a model and returns the names
// of the tools the broker offers the eval principal there.
func OfferedTools(ctx context.Context, c Case, options WorldOptions) ([]string, error) {
	options.Runtime = noModel{}
	options.Variant = nil
	world, err := NewWorld(ctx, c, options)
	if err != nil {
		return nil, err
	}
	defer world.Close()
	set, err := world.Assembly.Tools.ToolsFor(ctx, PrincipalID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(set.Definitions))
	for _, definition := range set.Definitions {
		names = append(names, definition.Name)
	}
	return names, nil
}

// noModel is the runtime of a world that only lists tools.
type noModel struct{}

func (noModel) Run(context.Context, agentruntime.RunRequest, agentruntime.EventSink) (agentruntime.RunResult, error) {
	return agentruntime.RunResult{}, errors.New("no model in this world")
}
