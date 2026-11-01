package sqlite

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// Every kind of object with a tab of its own has a table, and each table has the columns the tab
// and conversation queries read.
func TestEveryObjectKindHasItsTable(t *testing.T) {
	kinds := thread.ObjectKinds()
	slices.Sort(kinds)
	if tables := slices.Sorted(maps.Keys(objectTables)); !slices.Equal(tables, kinds) {
		t.Fatalf("object tables cover %v, want %v", tables, kinds)
	}
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "state", "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for kind, table := range objectTables {
		if _, err := store.db.ExecContext(t.Context(), `SELECT id, principal_id, title, summary_message_id FROM `+table+` LIMIT 0`); err != nil {
			t.Errorf("%s table %s: %v", kind, table, err)
		}
	}
}
