package nassim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/nas"
	"github.com/vladar107/home-ai-assistant/internal/testutil"
	"github.com/vladar107/home-ai-assistant/internal/testutil/nassim"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const seed = `{
  "roots": [
    {"name": "home", "files": {"Documents/notes.md": "Buy milk and bread.",
      "Documents/Taxes/2025/summary.txt": "Total due: 120 EUR"}, "folders": ["Photos/2025"]},
    {"name": "inbox", "writable": true, "create_folders": true}
  ],
  "default_write": "inbox"
}`

func call(t *testing.T, name, arguments string) tools.Result {
	t.Helper()
	tree := nassim.New(t, json.RawMessage(seed))
	state := testutil.OpenStore(t, filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	t.Cleanup(func() { _ = state.Close() })
	service, err := nas.NewService(nas.Config{Roots: tree.Roots("alice")}, state, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), "alice", tools.Call{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s failed: %s", name, result.Text)
	}
	return result
}

func TestConnectorSeesSeededTree(t *testing.T) {
	for _, check := range []struct{ tool, arguments, want string }{
		{"nas_list_files", `{"path":"home/Documents"}`, "notes.md"},
		{"nas_search_files", `{"query":"summary","path":"home"}`, "Taxes/2025/summary.txt"},
		{"nas_read_file", `{"path":"home/Documents/notes.md"}`, "Buy milk and bread."},
		{"nas_list_folders", `{"path":"home"}`, "2025"},
	} {
		if result := call(t, check.tool, check.arguments); !strings.Contains(result.Text, check.want) {
			t.Errorf("%s %s lacks %q:\n%s", check.tool, check.arguments, check.want, result.Text)
		}
	}
}

func TestValidateRejectsEscapesAndUnknownFields(t *testing.T) {
	if err := nassim.Validate(json.RawMessage(seed)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"roots":[{"name":"home","files":{"../outside.txt":"x"}}]}`,
		`{"roots":[{"name":"home","files":{"/abs.txt":"x"}}]}`,
		`{"roots":[{"name":"home"}],"nonsense":1}`,
		`{"roots":[{"name":"home"}],"default_write":"missing"}`,
	} {
		if err := nassim.Validate(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
