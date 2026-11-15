package mcpsim

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

// Seed describes a catalogue MCP server for a case: the tools it lists and what each call answers.
// A tool answers the first of its "answers" whose "arguments" all equal the call's arguments, and
// otherwise its own "text"; a tool with neither answers an empty result.
//
// Example seed (an invented tool):
//
//	{
//	  "tools": [{
//	    "name": "lookup_recipe", "description": "Finds a recipe by dish name.",
//	    "input_schema": {"type": "object", "properties": {"dish": {"type": "string"}}, "required": ["dish"]},
//	    "annotations": {"readOnlyHint": true},
//	    "text": "No recipe found.",
//	    "answers": [{"arguments": {"dish": "soup"}, "text": "Soup: simmer vegetables for 30 minutes."}]
//	  }]
//	}
type Seed struct {
	Tools []SeedTool `json:"tools"`
	// PageSize serves the tool list that many tools at a time; zero serves one page.
	PageSize int `json:"page_size"`
}

type SeedTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Annotations map[string]any  `json:"annotations"`
	Text        string          `json:"text"`
	IsError     bool            `json:"is_error"`
	Answers     []SeedAnswer    `json:"answers"`
}

type SeedAnswer struct {
	Arguments map[string]any `json:"arguments"`
	Text      string         `json:"text"`
	IsError   bool           `json:"is_error"`
}

// Validate checks that seed has the shape the simulator accepts.
func Validate(seed json.RawMessage) error {
	_, err := parseSeed(seed)
	return err
}

func parseSeed(raw json.RawMessage) (Seed, error) {
	var seed Seed
	if err := simkit.Decode(raw, &seed); err != nil {
		return seed, err
	}
	names := map[string]bool{}
	for index, tool := range seed.Tools {
		if tool.Name == "" || names[tool.Name] {
			return seed, fmt.Errorf("tools[%d] needs a unique name", index)
		}
		names[tool.Name] = true
		if len(tool.InputSchema) > 0 {
			if err := simkit.Object(fmt.Sprintf("tools[%d].input_schema", index), tool.InputSchema); err != nil {
				return seed, err
			}
		}
	}
	if seed.PageSize < 0 {
		return seed, fmt.Errorf("page_size must not be negative")
	}
	return seed, nil
}

// Start runs a simulator serving the seed's tools; the caller closes it. It is the form a command
// uses; tests use NewSeeded.
func Start(raw json.RawMessage) (*Simulator, error) {
	seed, err := parseSeed(raw)
	if err != nil {
		return nil, err
	}
	s := start()
	s.apply(seed)
	return s, nil
}

// NewSeeded starts a simulator serving the seed's tools for the length of a test.
func NewSeeded(t interface {
	Helper()
	Fatal(args ...any)
	Cleanup(func())
}, raw json.RawMessage) *Simulator {
	t.Helper()
	s, err := Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func (s *Simulator) apply(seed Seed) {
	tools := make([]Tool, 0, len(seed.Tools))
	for _, item := range seed.Tools {
		tool := Tool{Name: item.Name, Title: item.Title, Description: item.Description, Annotations: item.Annotations}
		if len(item.InputSchema) > 0 {
			tool.RawInputSchema = item.InputSchema
		}
		tools = append(tools, tool)
		item := item
		s.mu.Lock()
		s.responders[item.Name] = func(arguments json.RawMessage) Result {
			var given map[string]any
			_ = json.Unmarshal(arguments, &given)
			for _, answer := range item.Answers {
				if answers(given, answer.Arguments) {
					return textResult(answer.Text, answer.IsError)
				}
			}
			return textResult(item.Text, item.IsError)
		}
		s.mu.Unlock()
	}
	s.SetTools(tools, seed.PageSize)
}

func answers(given, wanted map[string]any) bool {
	for key, value := range wanted {
		if !reflect.DeepEqual(given[key], value) {
			return false
		}
	}
	return true
}

func textResult(text string, isError bool) Result {
	if text == "" {
		return Result{IsError: isError}
	}
	return Result{Content: []Content{Text(text)}, IsError: isError}
}
