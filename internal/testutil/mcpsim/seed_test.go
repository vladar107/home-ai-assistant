package mcpsim_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connectors/mcp"
	"github.com/vladar107/home-ai-assistant/internal/testutil/mcpsim"
)

const seed = `{
  "tools": [{
    "name": "lookup_recipe", "description": "Finds a recipe by dish name.",
    "input_schema": {"type": "object", "properties": {"dish": {"type": "string"}}, "required": ["dish"]},
    "annotations": {"readOnlyHint": true},
    "text": "No recipe found.",
    "answers": [{"arguments": {"dish": "soup"}, "text": "Soup: simmer vegetables for 30 minutes."}]
  }, {
    "name": "broken", "description": "Always fails.", "text": "service down", "is_error": true
  }]
}`

func TestSeededServerListsToolsAndAnswersPerCall(t *testing.T) {
	sim := mcpsim.NewSeeded(t, json.RawMessage(seed))
	client, err := mcp.New(mcp.Options{URL: sim.URL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	listed, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Name != "lookup_recipe" || listed[0].Description != "Finds a recipe by dish name." {
		t.Fatalf("tools %+v", listed)
	}
	for arguments, want := range map[string]string{
		`{"dish":"soup"}`:  "Soup: simmer vegetables for 30 minutes.",
		`{"dish":"bread"}`: "No recipe found.",
	} {
		result, err := client.CallTool(ctx, "lookup_recipe", json.RawMessage(arguments))
		if err != nil {
			t.Fatal(err)
		}
		if texts := result.Texts(); len(texts) != 1 || texts[0] != want || result.IsError {
			t.Errorf("%s: got %+v", arguments, result)
		}
	}
	result, err := client.CallTool(ctx, "broken", json.RawMessage(`{}`))
	if err != nil || !result.IsError {
		t.Fatalf("got %+v, %v", result, err)
	}
}

func TestValidateRejectsUnknownFields(t *testing.T) {
	if err := mcpsim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"tools":[],"nonsense":1}`, `{"tools":[{"name":"a"},{"name":"a"}]}`, `{"tools":[{"name":"a","input_schema":[]}]}`} {
		if err := mcpsim.Validate(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
