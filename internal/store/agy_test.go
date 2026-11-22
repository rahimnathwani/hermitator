package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Migration 23 rebuilds agents to let them run Antigravity. Nothing that
// refers to an agent may be lost on the way, and those references must
// point at the new table, so deleting an agent still takes its rows with it.
func TestAgyHarnessMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	t.Cleanup(func() { s.Close() })
	if err := s.migrateTo(ctx, 22); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	at := "2026-11-01T00:00:00.000000Z"
	exec(`INSERT INTO users (id, email, password_hash, role, created_at) VALUES ('u', 'a@example.com', 'x', 'user', ?)`, at)
	exec(`INSERT INTO agents (id, user_id, name, image, harness, model, effort, vocabulary, ip, created_at, last_active_at)
		VALUES ('a', 'u', 'bot', 'img', 'claude', 'opus', 'high', 'hermitator', '10.0.0.2', ?, ?)`, at, at)
	exec(`INSERT INTO whatsapp_accounts (id, user_id, status, fallback_agent_id, created_at) VALUES ('w', 'u', 'connected', 'a', ?)`, at)
	exec(`INSERT INTO routes (account_id, chat_jid, agent_id, created_at) VALUES ('w', 'c@s.whatsapp.net', 'a', ?)`, at)
	exec(`INSERT INTO turns (id, agent_id, harness, status, input, started_at) VALUES ('t', 'a', 'claude', 'ok', 'hi', ?)`, at)
	exec(`INSERT INTO chat_threads (agent_id, account_id, chat_jid, session_id) VALUES ('a', 'w', 'c@s.whatsapp.net', 's')`)
	exec(`INSERT INTO tasks (id, agent_id, name, prompt, created_by, created_at) VALUES ('k', 'a', 'n', 'p', 'owner', ?)`, at)
	if _, err := db.ExecContext(ctx, `UPDATE agents SET harness = 'agy' WHERE id = 'a'`); err == nil {
		t.Fatal("the old schema took an agy agent")
	}

	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(table string) (n int) {
		t.Helper()
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n)
		return n
	}
	for _, table := range []string{"agents", "routes", "turns", "chat_threads", "tasks"} {
		if count(table) != 1 {
			t.Fatalf("%s lost its row", table)
		}
	}
	if ag, err := s.Agent(ctx, "a"); err != nil || ag.Model != "opus" || ag.Effort != "high" || ag.Vocabulary != "hermitator" || ag.IP != "10.0.0.2" {
		t.Fatalf("agent after the migration: %+v, %v", ag, err)
	}
	var fallback string
	db.QueryRowContext(ctx, `SELECT fallback_agent_id FROM whatsapp_accounts WHERE id = 'w'`).Scan(&fallback)
	if fallback != "a" {
		t.Fatalf("fallback agent = %q", fallback)
	}
	rows, _ := db.QueryContext(ctx, `SELECT sql FROM sqlite_schema WHERE sql LIKE '%agents_before_agy%'`)
	if rows.Next() {
		t.Fatal("a table still refers to the old agents table")
	}
	rows.Close()
	var problems int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&problems)
	if problems != 0 {
		t.Fatalf("%d foreign key problems", problems)
	}

	exec(`UPDATE agents SET harness = 'agy' WHERE id = 'a'`)
	if _, err := db.ExecContext(ctx, `UPDATE agents SET harness = 'other' WHERE id = 'a'`); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("an unknown harness was taken: %v", err)
	}
	exec(`DELETE FROM agents WHERE id = 'a'`)
	for _, table := range []string{"routes", "turns", "chat_threads", "tasks"} {
		if count(table) != 0 {
			t.Fatalf("deleting the agent left its %s", table)
		}
	}
}
