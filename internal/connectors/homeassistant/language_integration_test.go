package homeassistant_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
	"github.com/vladar107/home-ai-assistant/internal/locale/localetest"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// homeLangAllowed are the Latin-letter words the Russian texts may hold: the product name, the
// command, and the names Home Assistant and the test supply.
var homeLangAllowed = []string{"Home", "Assistant", "/new", "ha_turn_on", "Turns", "on", "a", "device", "or", "entity", "area", "Room", "name", "Living", "TV", "lock", "front_door", "code", "unlock", "Front", "door", "entity_id", "ha_nonexistent", "ha", "turn", "new", "Runs", "at", "once", "the", "household", "s", "devices"}

func homeLangRequire(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Fatalf("text lacks %q:\n%s", phrase, text)
		}
	}
	if latin := localetest.Latin(text, homeLangAllowed...); len(latin) != 0 {
		t.Fatalf("text holds Latin words %q:\n%s", latin, text)
	}
}

func (f *fixture) homeLangCall(name string, arguments any) agentruntime.ToolResult {
	f.t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := f.toolSet().Executor.Execute(locale.With(f.t.Context(), locale.Russian), agentruntime.ToolCall{Name: name, Arguments: raw})
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

// After outside content a Russian user reads the proposal of an action, and of a confirmed
// device's service call, in Russian; the model's result stays English, and so does the outcome
// text the record keeps.
func TestHomeAssistantProposalsAndOutcomesReadInRussian(t *testing.T) {
	t.Parallel()
	sim := newSimulator(livingTV)
	sim.labelFrontDoor()
	f := newFixture(t, sim)
	f.service.SetExposure(&externalFlag{external: true})
	ctx := locale.With(t.Context(), locale.Russian)

	result := f.homeLangCall("ha_turn_on", map[string]any{"name": "Living TV", "area": "Room"})
	if result.IsError || !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("the model's result = %+v", result)
	}
	action, found := f.pending()
	if !found || action.Tool != "ha_action" {
		t.Fatalf("pending = %+v %v", action, found)
	}
	homeLangRequire(t, action.Summary, "Действие Home Assistant ha_turn_on: Home Assistant: Turns on a device or entity.", "\nАргументы:\narea: Room\nname: Living TV\n",
		"В этом разговоре был прочитан внешний контент, поэтому нужно твоё подтверждение.")

	if result := f.homeLangCall("ha_service_call", map[string]any{"entity_id": "lock.front_door", "service": "unlock", "data": map[string]any{"code": "1234"}}); result.IsError || !strings.Contains(result.Text, approval.ProposedText) {
		t.Fatalf("the model's result = %+v", result)
	}
	records, err := f.state.PendingApprovals(t.Context(), "alice", 0)
	if err != nil || len(records) != 2 {
		t.Fatalf("pending = %+v %v", records, err)
	}
	var call approval.Record
	for _, record := range records {
		if record.Tool == "ha_service_call" {
			call = record
		}
	}
	homeLangRequire(t, call.Summary, "Home Assistant: unlock для Front door (lock.front_door)")

	outcome, err := f.service.ExecuteApproved(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Text != "Done: unlock Front door." || outcome.UserText != "Выполнено: unlock для Front door." {
		t.Fatalf("outcome = %+v", outcome)
	}

	_, err = f.service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: "ha_action", Arguments: json.RawMessage(`{"tool":"ha_nonexistent","arguments":{}}`)})
	var failed *approval.ActionError
	if !asActionError(err, &failed) || failed.Text != "That home action is no longer available." || failed.UserText != "Это действие по дому больше недоступно." {
		t.Fatalf("a gone action = %#v", err)
	}
	_, err = f.service.ExecuteApproved(ctx, approval.Record{PrincipalID: "alice", Tool: "ha_action", Arguments: json.RawMessage(`{`)})
	if !asActionError(err, &failed) || failed.Text != "The stored action could not be read." || failed.UserText != "Не удалось прочитать сохранённое действие." {
		t.Fatalf("an unreadable action = %#v", err)
	}
}

func asActionError(err error, target **approval.ActionError) bool {
	failed, ok := err.(*approval.ActionError)
	if ok {
		*target = failed
	}
	return ok
}
