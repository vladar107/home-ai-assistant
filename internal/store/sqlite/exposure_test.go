package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A session exposed before migration 017 may have read text from outside the household, so it
// starts as external; a session that read nothing stays clean. Marking then records household
// content for memory alone, and outside content for both flags.
func TestConversationExternalExposure(t *testing.T) {
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
		if m.name >= "017" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	const (
		exposedSession = "11111111-1111-4111-8111-111111111111"
		cleanSession   = "22222222-2222-4222-8222-222222222222"
		exposedRequest = "33333333-3333-4333-8333-333333333333"
		cleanRequest   = "44444444-4444-4444-8444-444444444444"
	)
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice'), ('bob', 'Bob')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, created_at, updated_at, exposed_at)
		 VALUES ('` + exposedSession + `', 'alice', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '2026-09-03T10:00:00.000Z', '2026-09-03T10:00:00.000Z', '2026-09-03T10:30:00.000Z')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, created_at, updated_at)
		 VALUES ('` + cleanSession + `', 'bob', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', '2026-09-03T10:00:00.000Z', '2026-09-03T10:00:00.000Z')`,
		`INSERT INTO conversation_requests (id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message, created_at)
		 VALUES ('` + exposedRequest + `', 999, 7, 1007, 'alice', 101, '` + exposedSession + `', 'one', '2026-09-03T11:00:00.000Z')`,
		`INSERT INTO conversation_requests (id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message, created_at)
		 VALUES ('` + cleanRequest + `', 999, 8, 1008, 'bob', 202, '` + cleanSession + `', 'two', '2026-09-03T11:00:00.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}

	if external, err := store.ConversationExternal(ctx, "alice", exposedRequest); err != nil || !external {
		t.Fatalf("session exposed before the migration: external = %v %v", external, err)
	}
	assertExposure := func(stage string, wantExposed, wantExternal bool) {
		t.Helper()
		exposed, err := store.ConversationExposed(ctx, "bob", cleanRequest)
		if err != nil || exposed != wantExposed {
			t.Fatalf("%s: exposed = %v %v, want %v", stage, exposed, err, wantExposed)
		}
		external, err := store.ConversationExternal(ctx, "bob", cleanRequest)
		if err != nil || external != wantExternal {
			t.Fatalf("%s: external = %v %v, want %v", stage, external, err, wantExternal)
		}
	}
	assertExposure("after the migration", false, false)

	if err := store.MarkConversationExposed(ctx, "alice", cleanRequest, true); err != nil {
		t.Fatal(err)
	}
	assertExposure("after another principal marks the request", false, false)

	if err := store.MarkConversationExposed(ctx, "bob", cleanRequest, false); err != nil {
		t.Fatal(err)
	}
	assertExposure("after household content", true, false)

	if err := store.MarkConversationExposed(ctx, "bob", cleanRequest, true); err != nil {
		t.Fatal(err)
	}
	assertExposure("after outside content", true, true)

	if err := store.MarkConversationExposed(ctx, "bob", cleanRequest, false); err != nil {
		t.Fatal(err)
	}
	assertExposure("after household content again", true, true)

	if _, err := store.ConversationExternal(ctx, "alice", cleanRequest); err == nil {
		t.Fatal("another principal's request has an exposure answer")
	}
}

// A session from before migration 020 may have read private content, so it starts as private; a
// session created afterwards, as /new creates one, starts clean. Marking sets the flag once.
func TestConversationPrivate(t *testing.T) {
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
		if m.name >= "020" {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.checksum, "2026-10-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	const (
		oldSession = "11111111-1111-4111-8111-111111111111"
		newSession = "22222222-2222-4222-8222-222222222222"
		oldRequest = "33333333-3333-4333-8333-333333333333"
		newRequest = "44444444-4444-4444-8444-444444444444"
	)
	for _, statement := range []string{
		`INSERT INTO principals (id, display_name) VALUES ('alice', 'Alice'), ('bob', 'Bob')`,
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id, created_at, updated_at)
		 VALUES ('` + oldSession + `', 'alice', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '2026-10-01T10:00:00.000Z', '2026-10-01T10:00:00.000Z')`,
		`INSERT INTO conversation_requests (id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message, created_at)
		 VALUES ('` + oldRequest + `', 999, 7, 1007, 'alice', 101, '` + oldSession + `', 'one', '2026-10-01T11:00:00.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO conversation_sessions (id, principal_id, runtime_session_id) VALUES ('` + newSession + `', 'bob', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb')`,
		`INSERT INTO conversation_requests (id, bot_user_id, telegram_update_id, telegram_message_id, principal_id, chat_id, session_id, message, created_at)
		 VALUES ('` + newRequest + `', 999, 8, 1008, 'bob', 202, '` + newSession + `', 'two', '2026-10-02T11:00:00.000Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db}

	if private, err := store.ConversationPrivate(ctx, "alice", oldRequest); err != nil || !private {
		t.Fatalf("session from before the migration: private = %v %v", private, err)
	}
	assertPrivate := func(stage string, want bool) {
		t.Helper()
		private, err := store.ConversationPrivate(ctx, "bob", newRequest)
		if err != nil || private != want {
			t.Fatalf("%s: private = %v %v, want %v", stage, private, err, want)
		}
	}
	assertPrivate("new session", false)

	if err := store.MarkConversationPrivate(ctx, "alice", newRequest); err != nil {
		t.Fatal(err)
	}
	assertPrivate("after another principal marks the request", false)

	for range 2 {
		if err := store.MarkConversationPrivate(ctx, "bob", newRequest); err != nil {
			t.Fatal(err)
		}
		assertPrivate("after a private read", true)
	}

	if _, err := store.ConversationPrivate(ctx, "alice", newRequest); err == nil {
		t.Fatal("another principal's request has a privacy answer")
	}
}
