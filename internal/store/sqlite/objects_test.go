package sqlite

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/thread"
)

// Every kind of object with a tab of its own has a table, each table has the columns the tab and
// conversation queries read, and the name a conversation about each kind goes by can be read.
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
		if _, err := store.db.ExecContext(t.Context(), `SELECT id, principal_id, summary_message_id FROM `+table+` LIMIT 0`); err != nil {
			t.Errorf("%s table %s: %v", kind, table, err)
		}
		if label, err := loadSubjectLabel(t.Context(), store.db, "alice", thread.ObjectDestination(kind, "0000000000000001")); err != nil || label != "" {
			t.Errorf("%s label of an object that is not there = %q %v", kind, label, err)
		}
	}
}
