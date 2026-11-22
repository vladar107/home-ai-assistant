package eval

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestWorldOffersWatchesAndWebSearchLikeProduction(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionTrains: json.RawMessage(trainsSeed),
		SectionPages:  json.RawMessage(pagesSeed),
	}})
	offered := offeredTools(t, w)
	for _, tool := range []string{"watch_propose", "watch_list", "web_search", "fetch_page"} {
		if !slices.Contains(offered, tool) {
			t.Errorf("%s is not offered: %v", tool, offered)
		}
	}

	plain := offeredTools(t, newTestWorld(t, worldConfig{}))
	if slices.Contains(plain, "web_search") {
		t.Errorf("a world without a web connector offers web_search: %v", plain)
	}
}

func TestMailSearchUnderstandsGmailOperatorsOnTheCaseClock(t *testing.T) {
	t.Parallel()
	w := newTestWorld(t, worldConfig{sections: map[string]any{
		SectionGmail: json.RawMessage(`{"messages": [
			{"id": "m1", "from": "Northwind Energy <billing@northwind.example>", "subject": "Invoice", "received": "2026-03-10T07:00", "body": "Due soon.", "labels": ["INBOX", "UNREAD"]},
			{"id": "m2", "from": "Film Digest <digest@filmdigest.example>", "subject": "Picks", "received": "2026-03-01T07:00", "body": "Films.", "labels": ["INBOX"]}]}`),
	}})
	record := w.play(Step{Say: call("gmail_search", `{"query":"in:inbox from:northwind newer_than:1d is:unread"}`) + "\n" +
		call("gmail_search", `{"query":"newer_than:1d"}`)})
	var found []string
	for _, c := range record.ToolCalls {
		found = append(found, c.Result)
	}
	for i, result := range found {
		if !strings.Contains(result, "Invoice") || strings.Contains(result, "Picks") {
			t.Errorf("search %d: want only the invoice, got %q", i, result)
		}
	}
	if len(found) != 2 {
		t.Fatalf("got %d searches", len(found))
	}
}
