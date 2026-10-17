package store

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkUser(t *testing.T, s *Store, email string) *User {
	t.Helper()
	u := &User{Email: email, PasswordHash: "x", Role: RoleUser}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s.Close()
	}
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mkUser(t, s, " Alice@Example.com ")
	if err := s.CreateUser(ctx, &User{Email: "alice@example.com", PasswordHash: "x", Role: RoleUser}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate email: err = %v", err)
	}
	got, err := s.UserByEmail(ctx, "ALICE@example.com")
	if err != nil || got.ID != u.ID || got.MaxAgents != 5 {
		t.Fatalf("UserByEmail = %+v, %v", got, err)
	}
	if _, err := s.User(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: err = %v", err)
	}
}

func TestSessionsAndImpersonation(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	admin := mkUser(t, s, "admin@example.com")
	alice := mkUser(t, s, "alice@example.com")
	if _, err := s.CreateSession(ctx, "h1", admin.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffectiveUser(ctx, "h1", alice.ID); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Session(ctx, "h1")
	if err != nil || !sess.Impersonating() || sess.EffectiveID != alice.ID {
		t.Fatalf("Session = %+v, %v", sess, err)
	}
	if _, err := s.CreateSession(ctx, "h2", alice.ID, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "h2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: err = %v", err)
	}
	if err := s.SetSuspended(ctx, alice.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session impersonating a suspended user survived: %v", err)
	}
}

func TestAgentAddressAllocation(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mkUser(t, s, "a@example.com")
	subnet := netip.MustParsePrefix("10.88.0.0/29")
	var got []string
	for i := 0; i < 5; i++ {
		a := &Agent{UserID: u.ID, Name: string(rune('a' + i)), Image: "universal", Harness: HarnessClaude}
		if err := s.CreateAgent(ctx, a, subnet); err != nil {
			t.Fatalf("agent %d: %v", i, err)
		}
		got = append(got, a.IP)
	}
	want := []string{"10.88.0.2", "10.88.0.3", "10.88.0.4", "10.88.0.5", "10.88.0.6"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("addresses = %v, want %v", got, want)
		}
	}
	if err := s.CreateAgent(ctx, &Agent{UserID: u.ID, Name: "z", Image: "u", Harness: HarnessPi}, subnet); err == nil {
		t.Fatal("expected subnet exhaustion")
	}
	// Freed addresses are reused.
	agents, _ := s.Agents(ctx, u.ID)
	if err := s.DeleteAgent(ctx, agents[1].ID); err != nil {
		t.Fatal(err)
	}
	a := &Agent{UserID: u.ID, Name: "reuse", Image: "u", Harness: HarnessPi}
	if err := s.CreateAgent(ctx, a, subnet); err != nil || a.IP != agents[1].IP {
		t.Fatalf("reuse: ip = %s, err = %v, want %s", a.IP, err, agents[1].IP)
	}
	if err := s.CreateAgent(ctx, &Agent{UserID: u.ID, Name: "reuse", Image: "u", Harness: HarnessPi}, netip.MustParsePrefix("10.88.0.0/16")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: err = %v", err)
	}
}

func TestRoutesAndLinkCodes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mkUser(t, s, "a@example.com")
	a := &Agent{UserID: u.ID, Name: "bot", Image: "universal", Harness: HarnessClaude}
	if err := s.CreateAgent(ctx, a, netip.MustParsePrefix("10.88.0.0/16")); err != nil {
		t.Fatal(err)
	}
	acct := &Account{UserID: u.ID, JID: "123:4@s.whatsapp.net", Status: AccountConnected}
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLinkCode(ctx, a.ID, "ABCDE", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLinkCode(ctx, a.ID, "FGHJK", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeLinkCode(ctx, "ABCDE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replaced code still valid: %v", err)
	}
	id, err := s.ConsumeLinkCode(ctx, "FGHJK")
	if err != nil || id != a.ID {
		t.Fatalf("ConsumeLinkCode = %q, %v", id, err)
	}
	if _, err := s.ConsumeLinkCode(ctx, "FGHJK"); !errors.Is(err, ErrNotFound) {
		t.Fatal("code consumed twice")
	}
	if err := s.PutRoute(ctx, &Route{AccountID: acct.ID, ChatJID: "chat@g.us", AgentID: a.ID}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Route(ctx, acct.ID, "chat@g.us")
	if err != nil || r.AgentID != a.ID {
		t.Fatalf("Route = %+v, %v", r, err)
	}
	if err := s.SetAccountFallback(ctx, acct.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Route(ctx, acct.ID, "chat@g.us"); !errors.Is(err, ErrNotFound) {
		t.Fatal("route outlived its agent")
	}
	acct, _ = s.Account(ctx, acct.ID)
	if acct.FallbackAgentID != "" {
		t.Fatal("fallback outlived its agent")
	}
}

func TestTurns(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mkUser(t, s, "a@example.com")
	a := &Agent{UserID: u.ID, Name: "bot", Image: "universal", Harness: HarnessClaude}
	s.CreateAgent(ctx, a, netip.MustParsePrefix("10.88.0.0/16"))
	t1 := &Turn{AgentID: a.ID, Harness: a.Harness, Input: "hi"}
	if err := s.CreateTurn(ctx, t1); err != nil {
		t.Fatal(err)
	}
	t2 := &Turn{AgentID: a.ID, Harness: a.Harness, Input: "again"}
	s.CreateTurn(ctx, t2)
	t1.Status, t1.Output = TurnOK, "hello"
	if err := s.FinishTurn(ctx, t1); err != nil {
		t.Fatal(err)
	}
	n, err := s.InterruptRunningTurns(ctx)
	if err != nil || n != 1 {
		t.Fatalf("InterruptRunningTurns = %d, %v", n, err)
	}
	turns, err := s.Turns(ctx, a.ID, 10)
	if err != nil || len(turns) != 2 || turns[0].ID != t2.ID || turns[0].Status != TurnInterrupted {
		t.Fatalf("Turns = %+v, %v", turns, err)
	}
}
