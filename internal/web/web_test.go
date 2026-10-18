package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/auth"
	"github.com/rahimnathwani/hermitator/internal/gateway"
	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/secrets"
	"github.com/rahimnathwani/hermitator/internal/store"
)

type noGateway struct{}

func (noGateway) Connected(string) bool { return false }
func (noGateway) StartPairing(context.Context, string) (*gateway.Pairing, error) {
	return nil, gateway.ErrNotPaired
}
func (noGateway) Pairing(string, string) (*gateway.Pairing, error) { return nil, gateway.ErrNotPaired }
func (noGateway) PairPhone(context.Context, *gateway.Pairing, string) (string, error) {
	return "", gateway.ErrNotPaired
}
func (noGateway) Remove(context.Context, string) error    { return nil }
func (noGateway) ApplyKind(context.Context, string) error { return nil }

const password = "a long enough password"

type fixture struct {
	srv   *httptest.Server
	store *store.Store
	users map[string]*store.User
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	images := filepath.Join(root, "images")
	os.MkdirAll(filepath.Join(images, "universal-test", "rootfs"), 0o755)
	os.Symlink("universal-test", filepath.Join(images, "universal"))
	box, _ := secrets.New(make([]byte, secrets.KeySize))
	log := slog.New(slog.DiscardHandler)
	mgr := orchestrator.New(orchestrator.Config{
		AgentsDir: filepath.Join(root, "agents"),
		Images:    &sandbox.Images{Dir: images},
		Subnet:    netip.MustParsePrefix("10.88.0.0/16"),
	}, st, box, &sandbox.Local{}, log)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Agents: mgr, Gateway: noGateway{}, Images: &sandbox.Images{Dir: images}, Log: log, PublicURL: "http://test"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{srv: httptest.NewServer(h), store: st, users: map[string]*store.User{}}
	t.Cleanup(f.srv.Close)
	hash, _ := auth.HashPassword(password)
	for _, u := range []struct{ email, role string }{{"admin@example.com", store.RoleAdmin}, {"alice@example.com", store.RoleUser}, {"bob@example.com", store.RoleUser}} {
		user := &store.User{Email: u.email, PasswordHash: hash, Role: u.role}
		if err := st.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		f.users[u.email] = user
	}
	return f
}

// client is a browser session.
type client struct {
	t    *testing.T
	f    *fixture
	http *http.Client
}

func (f *fixture) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, f: f, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (f *fixture) login(t *testing.T, email string) *client {
	c := f.client(t)
	if code, _ := c.post("/login", url.Values{"email": {email}, "password": {password}}); code != http.StatusSeeOther {
		t.Fatalf("login %s: %d", email, code)
	}
	return c
}

func (c *client) get(path string) (int, string) {
	c.t.Helper()
	resp, err := c.http.Get(c.f.srv.URL + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b) + resp.Header.Get("Location")
}

func (c *client) post(path string, form url.Values, headers ...string) (int, string) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.f.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b) + resp.Header.Get("Location")
}

func (c *client) createAgent(name string) string {
	c.t.Helper()
	code, loc := c.post("/agents", url.Values{"name": {name}, "harness": {"claude"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/agents/") {
		c.t.Fatalf("create agent: %d %s", code, loc)
	}
	return strings.TrimPrefix(loc, "/agents/")
}

func TestLogin(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	if code, _ := c.get("/"); code != http.StatusSeeOther {
		t.Fatalf("anonymous dashboard: %d", code)
	}
	if code, _ := c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"wrong password!"}}); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := c.post("/login", url.Values{"email": {"nobody@example.com"}, "password": {password}}); code != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", code)
	}
	c = f.login(t, "ALICE@example.com")
	if code, body := c.get("/"); code != http.StatusOK || !strings.Contains(body, "Getting started") {
		t.Fatalf("dashboard: %d", code)
	}
	c.post("/logout", nil)
	if code, _ := c.get("/"); code != http.StatusSeeOther {
		t.Fatalf("dashboard after logout: %d", code)
	}
}

func TestUsersCannotSeeEachOthersAgents(t *testing.T) {
	f := setup(t)
	alice, bob := f.login(t, "alice@example.com"), f.login(t, "bob@example.com")
	id := alice.createAgent("secret-project")
	if code, body := alice.get("/agents/" + id); code != http.StatusOK || !strings.Contains(body, "secret-project") {
		t.Fatalf("owner view: %d", code)
	}
	for _, path := range []string{"/agents/" + id, "/agents/" + id + "/card", "/agents/" + id + "/terminal"} {
		if code, _ := bob.get(path); code != http.StatusNotFound {
			t.Errorf("bob GET %s: %d", path, code)
		}
	}
	for _, path := range []string{"/agents/" + id + "/delete", "/agents/" + id + "/settings", "/agents/" + id + "/prompt", "/agents/" + id + "/wake"} {
		if code, _ := bob.post(path, url.Values{"confirm": {"secret-project"}, "name": {"x"}}); code != http.StatusNotFound {
			t.Errorf("bob POST %s: %d", path, code)
		}
	}
	if _, err := f.store.Agent(context.Background(), id); err != nil {
		t.Fatal("bob deleted alice's agent")
	}
}

func TestAdminBoundaries(t *testing.T) {
	f := setup(t)
	admin, alice := f.login(t, "admin@example.com"), f.login(t, "alice@example.com")
	id := alice.createAgent("bot")

	if code, loc := admin.get("/"); code != http.StatusSeeOther || !strings.Contains(loc, "/admin/users") {
		t.Fatalf("admin dashboard: %d %s", code, loc)
	}
	if code, _ := admin.get("/agents/" + id); code != http.StatusForbidden {
		t.Fatalf("admin agent page without impersonating: %d", code)
	}
	if code, _ := admin.post("/agents", url.Values{"name": {"mine"}, "harness": {"claude"}}); code != http.StatusForbidden {
		t.Fatalf("admin creating an agent: %d", code)
	}
	if code, _ := alice.get("/admin/users"); code != http.StatusForbidden {
		t.Fatalf("user on admin page: %d", code)
	}
	if code, _ := alice.post("/admin/impersonate/"+f.users["bob@example.com"].ID, nil); code != http.StatusForbidden {
		t.Fatalf("user impersonating: %d", code)
	}
}

func TestImpersonation(t *testing.T) {
	f := setup(t)
	admin, alice := f.login(t, "admin@example.com"), f.login(t, "alice@example.com")
	id := alice.createAgent("bot")
	aliceID := f.users["alice@example.com"].ID

	if code, _ := admin.post("/admin/impersonate/"+f.users["admin@example.com"].ID, nil); code != http.StatusSeeOther {
		t.Fatalf("impersonate self: %d", code)
	}
	if code, _ := admin.post("/admin/impersonate/"+aliceID, nil); code != http.StatusSeeOther {
		t.Fatalf("impersonate: %d", code)
	}
	code, body := admin.get("/agents/" + id)
	if code != http.StatusOK || !strings.Contains(body, "Acting as <strong>alice@example.com</strong>") {
		t.Fatalf("agent page while impersonating: %d", code)
	}
	if code, _ := admin.get("/admin/users"); code != http.StatusForbidden {
		t.Fatalf("admin pages stay closed while impersonating: %d", code)
	}
	admin.post("/agents/"+id+"/new-session", nil)
	admin.post("/admin/stop-impersonating", nil)
	if code, _ := admin.get("/admin/users"); code != http.StatusOK {
		t.Fatalf("after stopping: %d", code)
	}

	entries, _ := f.store.AuditLog(context.Background(), 50)
	var started bool
	for _, e := range entries {
		if e.Action == "impersonate.start" && e.ActorID == f.users["admin@example.com"].ID && e.EffectiveID == aliceID {
			started = true
		}
	}
	if !started {
		t.Fatal("impersonation was not audited with both identities")
	}
}

func TestCrossOriginPostsAreRejected(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	code, _ := alice.post("/agents", url.Values{"name": {"evil"}, "harness": {"claude"}}, "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example")
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin create: %d", code)
	}
	agents, _ := f.store.Agents(context.Background(), f.users["alice@example.com"].ID)
	if len(agents) != 0 {
		t.Fatal("a cross-origin request created an agent")
	}
}

func TestSuspensionEndsSessions(t *testing.T) {
	f := setup(t)
	admin, alice := f.login(t, "admin@example.com"), f.login(t, "alice@example.com")
	if code, _ := admin.post("/admin/users/"+f.users["alice@example.com"].ID+"/suspend", nil); code != http.StatusSeeOther {
		t.Fatalf("suspend: %d", code)
	}
	if code, _ := alice.get("/"); code != http.StatusSeeOther {
		t.Fatalf("suspended user still logged in: %d", code)
	}
	if code, _ := alice.post("/login", url.Values{"email": {"alice@example.com"}, "password": {password}}); code != http.StatusUnauthorized {
		t.Fatalf("suspended user logged in again: %d", code)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/agents/x":          "/agents/x",
		"https://evil.com/":  "/",
		"//evil.com/":        "/",
		"/\\evil.com":        "/",
		"":                   "/",
		"/settings?tab=keys": "/settings?tab=keys",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	for i := 0; i < 12; i++ {
		c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"nope nope nope"}})
	}
	if code, body := c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {password}}); code != http.StatusUnauthorized || !strings.Contains(body, "Too many attempts") {
		t.Fatalf("after 12 failures a correct password got %d", code)
	}
}

func TestOwnerNumbersAndAccountKind(t *testing.T) {
	f := setup(t)
	alice, bob := f.login(t, "alice@example.com"), f.login(t, "bob@example.com")
	ctx := context.Background()
	if code, _ := alice.post("/settings/numbers", url.Values{"phone": {"+44 7700 900123"}}); code != http.StatusSeeOther {
		t.Fatalf("add number: %d", code)
	}
	if code, body := alice.get("/settings"); code != http.StatusOK || !strings.Contains(body, "+447700900123") {
		t.Fatalf("settings page does not list the number: %d", code)
	}
	if !f.store.IsOwnerNumber(ctx, f.users["alice@example.com"].ID, "447700900123") || f.store.IsOwnerNumber(ctx, f.users["bob@example.com"].ID, "447700900123") {
		t.Fatal("number saved for the wrong user")
	}

	acct := &store.Account{UserID: f.users["alice@example.com"].ID, JID: "1:1@s.whatsapp.net", Phone: "15550001111", Status: store.AccountConnected}
	f.store.CreateAccount(ctx, acct)
	if code, _ := bob.post("/accounts/"+acct.ID+"/kind", url.Values{"kind": {"personal"}}); code != http.StatusNotFound {
		t.Fatalf("bob changed alice's account: %d", code)
	}
	if code, _ := alice.post("/accounts/"+acct.ID+"/kind", url.Values{"kind": {"personal"}}); code != http.StatusSeeOther {
		t.Fatalf("set kind: %d", code)
	}
	if got, _ := f.store.Account(ctx, acct.ID); !got.Personal() {
		t.Fatal("kind not saved")
	}
	id := alice.createAgent("bot")
	if _, body := alice.get("/agents/" + id); !strings.Contains(body, "+15550001111") {
		t.Fatal("agent page does not explain where to send the link code")
	}
}

func TestRouteTriggers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice, bob := f.login(t, "alice@example.com"), f.login(t, "bob@example.com")
	id := alice.createAgent("bot")
	acct := &store.Account{UserID: f.users["alice@example.com"].ID, JID: "1:1@s.whatsapp.net", Phone: "15550001111", Status: store.AccountConnected}
	f.store.CreateAccount(ctx, acct)
	f.store.PutRoute(ctx, &store.Route{AccountID: acct.ID, ChatJID: "team@g.us", ChatName: "team", AgentID: id})
	f.store.Hold(ctx, &store.HeldMessage{AccountID: acct.ID, ChatJID: "team@g.us", MessageID: "m1", Sender: "x", Text: "lunch?"})

	if _, body := alice.get("/agents/" + id); !strings.Contains(body, "1 waiting") {
		t.Fatal("agent page does not show the held message")
	}
	form := url.Values{"account": {acct.ID}, "chat": {"team@g.us"}, "owner": {"mentions"}, "others": {"all"}}
	if code, _ := bob.post("/agents/"+id+"/routes/triggers", form); code != http.StatusNotFound {
		t.Fatalf("bob changed alice's chat: %d", code)
	}
	bad := url.Values{"account": {acct.ID}, "chat": {"team@g.us"}, "owner": {"sometimes"}, "others": {"all"}}
	if code, _ := alice.post("/agents/"+id+"/routes/triggers", bad); code != http.StatusBadRequest {
		t.Fatalf("invalid setting accepted: %d", code)
	}
	if code, _ := alice.post("/agents/"+id+"/routes/triggers", form); code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	r, _ := f.store.Route(ctx, acct.ID, "team@g.us")
	if r.OwnerTrigger != store.TriggerMentions || r.OthersTrigger != store.TriggerAll {
		t.Fatalf("triggers = %s/%s", r.OwnerTrigger, r.OthersTrigger)
	}
}
