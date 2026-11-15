package eval

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestWorldOffersWatchesAndWebSearchLikeProduction(t *testing.T) {
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
