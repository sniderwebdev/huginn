package session

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMessagesRebuildDoesNotCorruptViews pins the bug that took down spaces on
// a real install.
//
// migrateMessagesTypeThreadEventV1 rebuilds `messages` the usual SQLite way:
// rename to messages_old, create the new table, copy, drop the old. But
// SQLite's ALTER TABLE ... RENAME TO REWRITES VIEW DEFINITIONS to follow the
// rename unless legacy_alter_table is on. So the rename silently repointed
// every view over `messages` at `messages_old`, and the DROP then left them
// dangling.
//
// Nothing noticed for months, because nothing resolved those views. Then a
// later migration touched one, SQLite raised "no such table: main.messages_old",
// the whole spaces migration aborted, and the API started answering
// "spaces not configured" — with the cause four migrations and an unrelated
// subsystem away from the symptom.
func TestMessagesRebuildDoesNotCorruptViews(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// A pre-rebuild messages table (no thread_event in the CHECK), plus a view
	// over it — exactly the shape a real install had.
	if _, err := db.Exec(`
		CREATE TABLE messages (
			id             TEXT NOT NULL PRIMARY KEY,
			container_type TEXT NOT NULL,
			container_id   TEXT NOT NULL,
			tenant_id      TEXT NOT NULL DEFAULT '',
			seq            INTEGER NOT NULL,
			ts             TEXT NOT NULL DEFAULT '',
			role           TEXT NOT NULL,
			content        TEXT NOT NULL DEFAULT '',
			agent          TEXT NOT NULL DEFAULT '',
			tool_name      TEXT NOT NULL DEFAULT '',
			tool_call_id   TEXT NOT NULL DEFAULT '',
			tool_calls_json TEXT NOT NULL DEFAULT '',
			type           TEXT NOT NULL DEFAULT '' CHECK (type IN ('', 'cost')),
			prompt_tokens  INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd       REAL NOT NULL DEFAULT 0,
			model          TEXT NOT NULL DEFAULT '',
			parent_message_id TEXT,
			triggering_message_id TEXT,
			thread_reply_count INTEGER NOT NULL DEFAULT 0,
			thread_last_reply_at TEXT
		);
		CREATE VIEW v_costs AS
			SELECT container_id, SUM(cost_usd) AS total FROM messages
			WHERE type = 'cost' GROUP BY container_id;
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateMessagesTypeThreadEventV1(tx); err != nil {
		tx.Rollback()
		t.Fatalf("migrate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var viewSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='view' AND name='v_costs'`).Scan(&viewSQL); err != nil {
		t.Fatalf("view disappeared entirely: %v", err)
	}
	if strings.Contains(viewSQL, "messages_old") {
		t.Fatalf("the rebuild repointed the view at messages_old, which it then dropped:\n%s", viewSQL)
	}
	// The real symptom was not a bad definition string but an unusable view, so
	// assert the thing that actually broke: querying it.
	if _, err := db.Exec(`SELECT COUNT(*) FROM v_costs`); err != nil {
		t.Fatalf("view is dangling after the rebuild: %v", err)
	}
}

// TestRepairDanglingMessageViews covers the databases that ALREADY ran the
// unguarded rebuild. The legacy_alter_table guard prevents new damage; it does
// nothing for installs already carrying a view that points at a table which no
// longer exists. Those fail the next time any migration makes SQLite resolve
// the view — which is exactly how this was discovered.
func TestRepairDanglingMessageViews(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Reproduce the damaged state: a real messages table, and a view left
	// pointing at the long-gone messages_old.
	if _, err := db.Exec(`
		CREATE TABLE messages (container_id TEXT, cost_usd REAL, type TEXT);
		CREATE VIEW v_costs AS
			SELECT container_id, SUM(cost_usd) AS total FROM "messages_old"
			WHERE type = 'cost' GROUP BY container_id;
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec(`SELECT COUNT(*) FROM v_costs`); err == nil {
		t.Fatal("seed is wrong: the view should be dangling before the repair")
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateRepairDanglingMessageViewsV1(tx); err != nil {
		tx.Rollback()
		t.Fatalf("repair: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := db.Exec(`SELECT COUNT(*) FROM v_costs`); err != nil {
		t.Fatalf("view still dangling after the repair: %v", err)
	}
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='view' AND name='v_costs'`).Scan(&ddl); err != nil {
		t.Fatalf("view vanished instead of being repaired: %v", err)
	}
	if strings.Contains(ddl, "messages_old") {
		t.Fatalf("definition still names messages_old:\n%s", ddl)
	}
	// The repair must preserve the view's semantics, not just make it parse.
	if !strings.Contains(ddl, "SUM(cost_usd)") || !strings.Contains(ddl, "GROUP BY container_id") {
		t.Fatalf("repair altered the view's logic:\n%s", ddl)
	}
}

// TestRepairDanglingMessageViewsIsIdempotent — migrations must be safe to run
// against a healthy database, and this one runs on every existing install.
func TestRepairDanglingMessageViewsIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE messages (container_id TEXT, cost_usd REAL, type TEXT);
		CREATE VIEW v_costs AS SELECT container_id FROM messages;
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var before string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='v_costs'`).Scan(&before)

	for i := 0; i < 2; i++ {
		tx, _ := db.Begin()
		if err := migrateRepairDanglingMessageViewsV1(tx); err != nil {
			tx.Rollback()
			t.Fatalf("run %d: %v", i, err)
		}
		tx.Commit()
	}

	var after string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='v_costs'`).Scan(&after)
	if before != after {
		t.Fatalf("a healthy view was rewritten:\nbefore: %s\nafter:  %s", before, after)
	}
}
