package store

import (
	"context"
	"database/sql"
	"net/netip"
	"path/filepath"
	"testing"
)

// Until migration 19 an agent had one conversation, kept on the agent.
// It carries on in the chat whose message it last answered, compactions
// aside; the agent's other chats start afresh, and each earlier
// conversation is listed in the chat it began in.
func TestThreadsMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	t.Cleanup(func() { s.Close() })
	if err := s.migrateTo(ctx, 18); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash, role, created_at) VALUES ('u', 'a@example.com', 'x', 'user', '2026-10-01T00:00:00.000000Z')`)
	exec(`INSERT INTO agents (id, user_id, name, image, harness, ip, created_at, session_id, session_cost, context_tokens, review_turns, last_active_at)
		VALUES ('a', 'u', 'bot', 'img', 'claude', '10.0.0.2', '2026-10-01T00:00:00.000000Z', 's2', 1.5, 70000, 4, '2026-10-01T12:30:00.000000Z')`)
	turn := func(id, session, account, chat, at string) {
		exec(`INSERT INTO turns (id, agent_id, account_id, chat_jid, harness, status, input, session_id, started_at) VALUES (?, 'a', ?, ?, 'claude', 'ok', 'x', ?, ?)`,
			id, account, chat, session, at)
	}
	turn("t1", "s1", "acct", "a@s.whatsapp.net", "2026-10-01T10:00:00.000000Z")
	turn("t2", "s2", "acct", "a@s.whatsapp.net", "2026-10-01T11:00:00.000000Z")
	turn("t3", "s2", "acct", "b@g.us", "2026-10-01T12:00:00.000000Z")
	turn("compact-t4", "s2", "", "", "2026-10-01T12:30:00.000000Z")
	for _, sid := range []string{"s1", "s2"} {
		exec(`INSERT INTO agent_sessions (agent_id, session_id, harness, first_prompt, turns, started_at, last_at) VALUES ('a', ?, 'claude', 'x', 1, '2026-10-01T10:00:00.000000Z', '2026-10-01T12:00:00.000000Z')`, sid)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}

	a := ThreadKey{AgentID: "a", AccountID: "acct", ChatJID: "a@s.whatsapp.net"}
	b := ThreadKey{AgentID: "a", AccountID: "acct", ChatJID: "b@g.us"}
	if th, _ := s.Thread(ctx, b); th.SessionID != "s2" || th.SessionCost != 1.5 || th.ContextTokens != 70000 || th.ReviewTurns != 4 || th.LastAt.IsZero() {
		t.Fatalf("the chat answered last: %+v", th)
	}
	if th, _ := s.Thread(ctx, a); th.SessionID != "" {
		t.Fatalf("the other chat: %+v", th)
	}
	for k, want := range map[ThreadKey]string{a: "s1", b: "s2"} {
		if list, _ := s.Sessions(ctx, k, 10); len(list) != 1 || list[0].SessionID != want {
			t.Fatalf("sessions in %s = %+v, want only %s", k.ChatJID, list, want)
		}
	}
	if ag, err := s.Agent(ctx, "a"); err != nil || ag.Name != "bot" {
		t.Fatalf("agent after the migration: %+v, %v", ag, err)
	}
}

func TestThreads(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	ag := &Agent{UserID: mkUser(t, s, "a@example.com").ID, Name: "bot", Image: "u", Harness: HarnessClaude}
	if err := s.CreateAgent(ctx, ag, netip.MustParsePrefix("10.0.0.0/24")); err != nil {
		t.Fatal(err)
	}
	k := ThreadKey{AgentID: ag.ID, AccountID: "acct", ChatJID: "c"}
	if th, err := s.Thread(ctx, k); err != nil || th.SessionID != "" || th.ThreadKey != k {
		t.Fatalf("a conversation not yet started: %+v, %v", th, err)
	}
	for _, err := range []error{s.SetThreadSessionCost(ctx, k, "s", 0.4), s.SetThreadContext(ctx, k, 500), s.CountReviewTurn(ctx, k), s.TouchThread(ctx, k)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	th, _ := s.Thread(ctx, k)
	if th.SessionID != "s" || th.SessionCost != 0.4 || th.ContextTokens != 500 || th.ReviewTurns != 1 || th.LastAt.IsZero() {
		t.Fatalf("thread = %+v", th)
	}
	s.SetThreadSession(ctx, k, "")
	if th, _ := s.Thread(ctx, k); th.SessionID != "" || th.SessionCost != 0 || th.ContextTokens != 0 || th.ReviewTurns != 1 {
		t.Fatalf("after /new: %+v", th)
	}
	s.ClearThreads(ctx, ag.ID)
	if list, _ := s.Threads(ctx, ag.ID); len(list) != 0 {
		t.Fatalf("after switching harness: %+v", list)
	}
}
