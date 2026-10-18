package sqlite

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/vladar107/home-ai-assistant/internal/connectors/bus"
	"github.com/vladar107/home-ai-assistant/internal/connectors/grafana"
	"github.com/vladar107/home-ai-assistant/internal/connectors/homeassistant"
	"github.com/vladar107/home-ai-assistant/internal/connectors/ns"
	"github.com/vladar107/home-ai-assistant/internal/connectors/web"
	"github.com/vladar107/home-ai-assistant/internal/integration"
	"github.com/vladar107/home-ai-assistant/internal/judgment"
)

// Migration 040 moves the switches out of integration_connections, where a sentinel row meant off
// for the integrations on by default and on for the TypeSafe opt-in; every principal keeps the
// state they had, and the connected accounts stay where they are.
func TestMigrationIntegrationSwitchesKeepsEachUsersState(t *testing.T) {
	ctx := t.Context()
	path, err := filepath.Abs(filepath.Join(t.TempDir(), "assistant.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.name >= "040" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-10-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	connection := `INSERT INTO integration_connections (principal_id, integration, token_kind, workspace_id, workspace_name, bot_name, writes_enabled)
		VALUES (?, ?, ?, ?, ?, '', ?)`
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice')`,
		`INSERT INTO principals (id, display_name) VALUES ('bob', 'Bob')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][]any{
		{"alice", "ns", "internal", "ns-trains-off", "Trains off", 0},
		{"alice", "grafana", "internal", "grafana-off", "Grafana off", 0},
		{"alice", "typesafe", "internal", "typesafe-judgments-on", "TypeSafe safety checks on", 0},
		{"bob", "homeassistant", "internal", "homeassistant-off", "Home off", 0},
		{"bob", "web", "internal", "web-access-off", "Web access off", 0},
		{"bob", "bus", "internal", "bus-off", "Buses off", 0},
		{"alice", "notion", "personal", "workspace-1", "Notes", 1},
		{"bob", "nas", "internal", "nas", "NAS files", 1},
	} {
		if _, err := db.ExecContext(ctx, connection, row...); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	switches, err := integration.NewSwitches(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		sw         integration.Switch
		alice, bob bool
	}{
		{ns.Switch, false, true},
		{grafana.Switch, false, true},
		{homeassistant.Switch, true, false},
		{web.Switch, true, false},
		{bus.Switch, true, false},
		{judgment.OptInSwitch, true, false},
	} {
		for principal, on := range map[string]bool{"alice": want.alice, "bob": want.bob} {
			if enabled, err := switches.Enabled(ctx, principal, want.sw); err != nil || enabled != on {
				t.Fatalf("%s for %s after migration = %v %v, want %v", want.sw.Integration, principal, enabled, err, on)
			}
		}
	}

	if count := countRows(t, store, `SELECT count(*) FROM integration_connections`); count != 2 {
		t.Fatalf("connections after migration = %d, want the Notion and NAS rows", count)
	}
	if record, found, err := store.LoadIntegrationConnection(ctx, "alice", "notion"); err != nil || !found || record.WorkspaceID != "workspace-1" || !record.WritesEnabled {
		t.Fatalf("Notion connection after migration = %+v %v %v", record, found, err)
	}
	if record, found, err := store.LoadIntegrationConnection(ctx, "bob", "nas"); err != nil || !found || !record.WritesEnabled {
		t.Fatalf("NAS record after migration = %+v %v %v", record, found, err)
	}

	if err := switches.SetEnabled(ctx, "alice", ns.Switch, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := switches.Enabled(ctx, "alice", ns.Switch); err != nil || !enabled {
		t.Fatalf("trains for alice after turning them back on = %v %v", enabled, err)
	}
	if count := countRows(t, store, `SELECT count(*) FROM integration_connections WHERE integration = 'ns'`); count != 0 {
		t.Fatalf("turning a switch on wrote %d connection rows", count)
	}
}
