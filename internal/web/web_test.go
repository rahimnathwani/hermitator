package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rahimnathwani/hermitator/internal/auth"
	"github.com/rahimnathwani/hermitator/internal/gateway"
	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/secrets"
	"github.com/rahimnathwani/hermitator/internal/store"
)

type noGateway struct{}

// guestBinary is hermitator-guest, built once: preview links wake agents,
// and waking runs it.
var guestBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hermitator-guest-")
	if err != nil {
		panic(err)
	}
	guestBinary = filepath.Join(dir, "hermitator-guest")
	build := exec.Command("go", "build", "-o", guestBinary, "../../cmd/hermitator-guest")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		panic("building the guest: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

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
	srv    *httptest.Server
	store  *store.Store
	users  map[string]*store.User
	images string
	server *Server
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
	if os.Getenv("WEBTEST_LOG") != "" {
		log = slog.Default()
	}
	tools, err := sandbox.InstallTools(filepath.Join(root, "tools"), guestBinary, fstest.MapFS{"pi-login.mjs": {Data: []byte("")}})
	if err != nil {
		t.Fatal(err)
	}
	mgr := orchestrator.New(orchestrator.Config{
		Tools:         tools,
		AgentsDir:     filepath.Join(root, "agents"),
		Images:        &sandbox.Images{Dir: images},
		Subnet:        netip.MustParsePrefix("10.88.0.0/16"),
		PreviewDomain: "preview.test", PreviewKey: []byte("preview test key"), PreviewLoopback: true,
	}, st, box, &sandbox.Local{}, log)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Agents: mgr, Gateway: noGateway{}, Images: &sandbox.Images{Dir: images}, Log: log, PublicURL: "http://test", PreviewDomain: "preview.test",
		GitHub: func(_ context.Context, token string) (GitHubUser, error) {
			if token != "gho_good_token_123" {
				return GitHubUser{}, errors.New("GitHub said 401 Unauthorized")
			}
			return GitHubUser{ID: 42, Login: "ada"}, nil
		}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{srv: httptest.NewServer(h), store: st, users: map[string]*store.User{}, images: images, server: s}
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
	if code, body := c.get("/"); code != http.StatusOK || !strings.Contains(body, "Get set up") {
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

func TestModelAndEffortSettings(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	id := alice.createAgent("bot")
	base := url.Values{"name": {"bot"}, "harness": {"claude"}, "provider": {"openai-codex"}, "idle_minutes": {"5"}, "memory_mb": {"4096"}}
	set := func(extra url.Values) *store.Agent {
		t.Helper()
		form := url.Values{}
		for k, v := range base {
			form[k] = v
		}
		for k, v := range extra {
			form[k] = v
		}
		if code, _ := alice.post("/agents/"+id+"/settings", form); code != http.StatusSeeOther {
			t.Fatalf("save: %d", code)
		}
		a, _ := f.store.Agent(context.Background(), id)
		return a
	}
	page := func() string {
		_, body := alice.get("/agents/" + id + "/settings")
		return body
	}

	// The segmented form posts one model choice per harness and provider;
	// only the selected one counts.
	a := set(url.Values{"model:claude": {"opus"}, "model:pi-deepseek": {"deepseek-flash"}, "effort_claude": {"high"}, "effort_pi": {"minimal"}})
	if a.Model != "opus" || a.Effort != "high" {
		t.Fatalf("saved %q/%q", a.Model, a.Effort)
	}
	body := page()
	for _, want := range []string{
		`name="model:claude" value="opus" checked`,
		`name="effort_claude" value="high" checked`,
		`name="model:pi-deepseek" value="deepseek-flash"`,
		`title="~anthropic/claude-opus-latest"`,
		`name="memory_mb" value="4096" checked`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("agent page lacks %s", want)
		}
	}
	if strings.Contains(body, "<select") {
		t.Fatal("agent page still has a dropdown")
	}

	if a := set(url.Values{"model:claude": {"+other"}, "model-other:claude": {"claude-opus-5-5"}, "effort_claude": {"ludicrous"}}); a.Model != "claude-opus-5-5" || a.Effort != "" {
		t.Fatalf("other model: saved %q/%q", a.Model, a.Effort)
	}
	// html/template writes the "+" of "+other" as &#43; inside attributes.
	if body := page(); !strings.Contains(body, `value="&#43;other" checked`) || !strings.Contains(body, `value="claude-opus-5-5"`) {
		t.Fatal("a model off the list does not show as other…")
	}

	// Scripts can still post a plain model field.
	if a := set(url.Values{"model": {"sonnet"}}); a.Model != "sonnet" {
		t.Fatalf("plain model field: saved %q", a.Model)
	}

	pi := set(url.Values{"harness": {"pi"}, "provider": {"deepseek"}, "model:pi-deepseek": {"deepseek-flash"}, "effort_pi": {"minimal"}, "model:claude": {"opus"}})
	if pi.Model != "deepseek-flash" || pi.Effort != "minimal" || pi.PiProvider != "deepseek" {
		t.Fatalf("pi: saved %q/%q/%q", pi.Model, pi.Effort, pi.PiProvider)
	}
}

func TestNewAgentTakesModelAndEffort(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	// The agents page links to the form instead of carrying it.
	if _, body := alice.get("/"); strings.Contains(body, `action="/agents"`) || !strings.Contains(body, `href="/agents/new"`) {
		t.Fatal("agents page should link to /agents/new, not hold the form")
	}
	_, body := alice.get("/agents/new")
	if strings.Contains(body, "<select") || !strings.Contains(body, `name="model:pi-openai-codex"`) {
		t.Fatal("new-agent form is not segmented")
	}
	form := url.Values{"name": {"fresh"}, "harness": {"claude"}, "provider": {"openai-codex"}, "model:claude": {"haiku"}, "effort_claude": {"low"}}
	code, loc := alice.post("/agents", form)
	if code != http.StatusSeeOther {
		t.Fatalf("create: %d", code)
	}
	a, _ := f.store.Agent(context.Background(), strings.TrimPrefix(loc, "/agents/"))
	if a.Model != "haiku" || a.Effort != "low" {
		t.Fatalf("created with %q/%q", a.Model, a.Effort)
	}
}

// Keys live on the account's Settings page, not the agent's, so the agent
// forms say whether the chosen provider has one and link to its card.
func TestProviderHintLinksToTheKey(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	id := alice.createAgent("bot")
	alice.post("/agents/"+id+"/settings", url.Values{"name": {"bot"}, "harness": {"pi"}, "provider": {"deepseek"}, "idle_minutes": {"5"}, "memory_mb": {"4096"}})

	_, body := alice.get("/agents/" + id + "/settings")
	for _, want := range []string{
		`data-provider-hint="deepseek">No DeepSeek key yet: add one in <a href="/settings#deepseek">Settings</a>`,
		`data-provider-hint="openai-codex" hidden>ChatGPT is not connected yet`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("agent settings lack %s", want)
		}
	}
	_, settings := alice.get("/settings")
	for _, p := range orchestrator.PiProviders {
		if !strings.Contains(body, `href="/settings#`+p.ID+`"`) || !strings.Contains(settings, `id="`+p.ID+`"`) {
			t.Fatalf("no link from the agent form to a Settings card with id %q", p.ID)
		}
	}

	alice.post("/settings/secret", url.Values{"field": {"deepseek"}, "value": {"sk-test"}})
	if _, body := alice.get("/agents/" + id + "/settings"); !strings.Contains(body, `data-provider-hint="deepseek">Uses your DeepSeek key from`) {
		t.Fatal("the hint does not see the saved key")
	}
	if _, body := alice.get("/agents/new"); !strings.Contains(body, `data-provider-hint="deepseek" hidden>Uses your DeepSeek key from`) ||
		!strings.Contains(body, `data-provider-hint="openai-codex">ChatGPT is not connected yet`) {
		t.Fatal("the new-agent form lacks the hints, or shows the wrong one")
	}
}

func TestAgentTabs(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	bob := f.login(t, "bob@example.com")
	id := alice.createAgent("bot")
	for _, tab := range []string{"", "/chats", "/settings", "/terminal"} {
		code, body := alice.get("/agents/" + id + tab)
		if code != http.StatusOK || !strings.Contains(body, `href="/agents/`+id+tab+`" aria-current="page"`) {
			t.Fatalf("tab %q: %d, or not marked current", tab, code)
		}
		if code, _ := bob.get("/agents/" + id + tab); code != http.StatusNotFound {
			t.Fatalf("someone else's tab %q: %d", tab, code)
		}
	}
}

func TestRemovingANumberTakesItsLastFourDigits(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.login(t, "alice@example.com")
	acct := &store.Account{UserID: f.users["alice@example.com"].ID, JID: "1:1@s.whatsapp.net", Phone: "447700900999", Status: store.AccountConnected}
	f.store.CreateAccount(ctx, acct)
	removed := func() bool {
		log, _ := f.store.AuditLog(ctx, 50)
		for _, e := range log {
			if e.Action == "account.remove" {
				return true
			}
		}
		return false
	}
	for _, confirm := range []string{"", "9999", "447700900999"} {
		alice.post("/accounts/"+acct.ID+"/delete", url.Values{"confirm": {confirm}})
		if removed() {
			t.Fatalf("removed with confirm=%q", confirm)
		}
	}
	if _, body := alice.get("/accounts"); !strings.Contains(body, `data-confirm-type="0999"`) {
		t.Fatal("the remove button does not ask for the last four digits")
	}
	alice.post("/accounts/"+acct.ID+"/delete", url.Values{"confirm": {"0999"}})
	if !removed() {
		t.Fatal("not removed with the right digits")
	}
}

func TestUpgradeAll(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.login(t, "alice@example.com")
	ids := []string{alice.createAgent("one"), alice.createAgent("two")}
	if _, body := alice.get("/"); strings.Contains(body, "/agents/upgrade-all") {
		t.Fatal("offers an upgrade with nothing newer")
	}
	os.MkdirAll(filepath.Join(f.images, "universal-test.1", "rootfs"), 0o755)
	os.Remove(filepath.Join(f.images, "universal"))
	os.Symlink("universal-test.1", filepath.Join(f.images, "universal"))
	if _, body := alice.get("/"); !strings.Contains(body, `action="/agents/upgrade-all" data-confirm=`) {
		t.Fatal("no confirmed Upgrade all for outdated agents")
	}
	if code, _ := alice.post("/agents/upgrade-all", nil); code != http.StatusSeeOther {
		t.Fatalf("upgrade all: %d", code)
	}
	for _, id := range ids {
		if a, _ := f.store.Agent(ctx, id); a.Image != "universal-test.1" {
			t.Fatalf("%s still on %s", a.Name, a.Image)
		}
	}
}

func TestLimitsAndTasks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.login(t, "alice@example.com")
	bob := f.login(t, "bob@example.com")
	aliceID := f.users["alice@example.com"].ID

	alice.post("/settings/limits", url.Values{"timezone": {"Mars/Olympus"}, "daily_cap": {"5"}})
	if u, _ := f.store.User(ctx, aliceID); u.Timezone != "" || u.DailyCap != 0 {
		t.Fatal("saved an unknown time zone")
	}
	alice.post("/settings/limits", url.Values{"timezone": {"Europe/London"}, "daily_cap": {"2.50"}, "monthly_cap": {""}})
	if u, _ := f.store.User(ctx, aliceID); u.Timezone != "Europe/London" || u.DailyCap != 2.5 || u.MonthlyCap != 0 {
		t.Fatalf("limits = %q %v %v", u.Timezone, u.DailyCap, u.MonthlyCap)
	}

	id := alice.createAgent("bot")
	task := &store.Task{AgentID: id, Name: "standup", Prompt: "p", Cron: "0 9 * * 1-5", NextRun: time.Now().Add(time.Hour), CreatedBy: store.TaskByAgent}
	f.store.CreateTask(ctx, task)
	if _, body := alice.get("/agents/" + id); !strings.Contains(body, "standup") || !strings.Contains(body, "cron 0 9 * * 1-5") {
		t.Fatal("the overview does not list the task")
	}
	if code, _ := bob.post("/agents/"+id+"/tasks/"+task.ID+"/delete", nil); code != http.StatusNotFound {
		t.Fatalf("bob deleted alice's task: %d", code)
	}
	alice.post("/agents/"+id+"/tasks/"+task.ID+"/pause", nil)
	if got, _ := f.store.Task(ctx, task.ID); !got.Paused {
		t.Fatal("not paused")
	}
	alice.post("/agents/"+id+"/tasks/"+task.ID+"/delete", nil)
	if _, err := f.store.Task(ctx, task.ID); err == nil {
		t.Fatal("not deleted")
	}
}

func TestGitHubAndSecrets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.login(t, "alice@example.com")
	aliceID := f.users["alice@example.com"].ID

	alice.post("/settings/github", url.Values{"value": {"gho_bad"}})
	if u, _ := f.store.User(ctx, aliceID); u.GitHubLogin != "" {
		t.Fatal("saved a token GitHub refused")
	}
	alice.post("/settings/github", url.Values{"value": {"gho_good_token_123"}})
	u, _ := f.store.User(ctx, aliceID)
	if u.GitHubLogin != "ada" || u.GitName != "ada" || u.GitEmail != "42+ada@users.noreply.github.com" {
		t.Fatalf("after connecting: login %q, identity %q <%q>", u.GitHubLogin, u.GitName, u.GitEmail)
	}
	if f.server.Agents.Secret(ctx, aliceID, store.SecretGitHubToken) != "gho_good_token_123" {
		t.Fatal("token not stored")
	}

	for name, want := range map[string]bool{"STRIPE_KEY": true, "stripe_key": true, "GH_TOKEN": false, "GIT_ASKPASS": false, "PATH": false, "2FA": false, "A-B": false} {
		alice.post("/settings/env", url.Values{"name": {name}, "value": {"value-for-" + name}})
		got := f.server.Agents.Secret(ctx, aliceID, store.SecretEnvPrefix+strings.ToUpper(name)) != ""
		if got != want {
			t.Errorf("secret %s saved = %v, want %v", name, got, want)
		}
	}
	_, body := alice.get("/settings")
	if !strings.Contains(body, "STRIPE_KEY") || strings.Contains(body, "value-for-") || strings.Contains(body, "gho_good") {
		t.Fatal("the settings page lists secrets wrongly, or shows a value")
	}
	alice.post("/settings/env/delete", url.Values{"name": {"STRIPE_KEY"}})
	if f.server.Agents.Secret(ctx, aliceID, store.SecretEnvPrefix+"STRIPE_KEY") != "" {
		t.Fatal("secret not removed")
	}
	alice.post("/settings/github", url.Values{"remove": {"1"}})
	if u, _ := f.store.User(ctx, aliceID); u.GitHubLogin != "" || f.server.Agents.Secret(ctx, aliceID, store.SecretGitHubToken) != "" {
		t.Fatal("GitHub not disconnected")
	}
}

func TestWorkAtRiskEndpoint(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	bob := f.login(t, "bob@example.com")
	id := alice.createAgent("bot")
	// The web tests have no sandbox to look in, so this says it could not
	// check; the orchestrator's tests cover the check itself.
	if code, body := alice.get("/agents/" + id + "/risk"); code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(body), "<li>") {
		t.Fatalf("risk = %d %q", code, body)
	}
	if code, _ := bob.get("/agents/" + id + "/risk"); code != http.StatusNotFound {
		t.Fatalf("bob checked alice's agent: %d", code)
	}
	if _, body := alice.get("/agents/" + id + "/settings"); !strings.Contains(body, `data-load="/agents/`+id+`/risk"`) {
		t.Fatal("the delete dialog does not check for unpushed work")
	}
}

func TestRouteHistorySwitch(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.login(t, "alice@example.com")
	bob := f.login(t, "bob@example.com")
	id := alice.createAgent("bot")
	acct := &store.Account{UserID: f.users["alice@example.com"].ID, JID: "1:7@s.whatsapp.net", Phone: "15550007777", Status: store.AccountConnected}
	f.store.CreateAccount(ctx, acct)
	f.store.PutRoute(ctx, &store.Route{AccountID: acct.ID, ChatJID: "team@g.us", ChatName: "Team", AgentID: id, OwnerTrigger: store.TriggerAll, OthersTrigger: store.TriggerNone})
	form := url.Values{"account": {acct.ID}, "chat": {"team@g.us"}, "history": {"on"}}
	if code, _ := bob.post("/agents/"+id+"/routes/history", form); code != http.StatusNotFound {
		t.Fatalf("bob switched alice's history: %d", code)
	}
	alice.post("/agents/"+id+"/routes/history", form)
	if r, _ := f.store.Route(ctx, acct.ID, "team@g.us"); !r.History {
		t.Fatal("history not on")
	}
	if _, body := alice.get("/agents/" + id + "/chats"); !strings.Contains(body, `name="history" value="on" checked`) {
		t.Fatal("the Chats tab does not show history as on")
	}
}

func TestPreviewLinks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.login(t, "alice@example.com")
	bob := f.login(t, "bob@example.com")
	id := alice.createAgent("bot")

	var seen *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		fmt.Fprint(w, "hello from the dev server")
	}))
	defer upstream.Close()
	port := upstream.Listener.Addr().(*net.TCPAddr).Port
	host := fmt.Sprintf("%d-%s.preview.test", port, id)
	link, err := f.server.Agents.NewPreview(ctx, id, port)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	if u.Host != host || u.Query().Get(orchestrator.PreviewParam) == "" {
		t.Fatalf("link = %s", link)
	}

	visitor := f.client(t)
	get := func(c *client, target string, cookies ...string) (int, string, *http.Response) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+target, nil)
		req.Host = host
		for _, ck := range cookies {
			req.Header.Add("Cookie", ck)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp
	}
	if code, _, _ := get(visitor, "/"); code != http.StatusForbidden {
		t.Fatalf("without the secret: %d", code)
	}
	if code, _, _ := get(visitor, "/?"+orchestrator.PreviewParam+"=wrong"); code != http.StatusForbidden {
		t.Fatalf("with a wrong secret: %d", code)
	}
	code, _, resp := get(visitor, "/app?x=1&"+orchestrator.PreviewParam+"="+u.Query().Get(orchestrator.PreviewParam))
	if code != http.StatusSeeOther || resp.Header.Get("Location") != "/app?x=1" {
		t.Fatalf("opening the link: %d to %q", code, resp.Header.Get("Location"))
	}
	if code, body, _ := get(visitor, "/app?x=1", "theirs=kept"); code != http.StatusOK || body != "hello from the dev server" {
		t.Fatalf("through the link: %d %q", code, body)
	}
	if seen.Host != fmt.Sprintf("localhost:%d", port) || strings.Contains(seen.Header.Get("Cookie"), "hermitator") || !strings.Contains(seen.Header.Get("Cookie"), "theirs=kept") {
		t.Fatalf("the dev server saw host %q, cookies %q", seen.Host, seen.Header.Get("Cookie"))
	}

	allowed := func(domain string, fwd bool) int {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/preview-allowed?domain="+domain, nil)
		if fwd {
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		return resp.StatusCode
	}
	if allowed(host, false) != http.StatusOK || allowed("1-"+id+".preview.test", false) != http.StatusNotFound || allowed(host, true) != http.StatusNotFound {
		t.Fatal("Caddy's question is answered wrongly")
	}
	if _, body := alice.get("/agents/" + id + "/settings"); !strings.Contains(body, fmt.Sprintf("Port %d", port)) {
		t.Fatal("settings do not list the link")
	}
	if code, _ := bob.post("/agents/"+id+"/previews/delete", nil); code != http.StatusNotFound {
		t.Fatalf("bob turned off alice's links: %d", code)
	}
	alice.post("/agents/"+id+"/previews/delete", nil)
	if code, _, _ := get(visitor, "/app"); code != http.StatusNotFound {
		t.Fatalf("after turning links off: %d", code)
	}
	again, _ := f.server.Agents.NewPreview(ctx, id, port)
	if again == link {
		t.Fatal("a new link is the old one")
	}
}

func TestNewAgentRejectsABadRepository(t *testing.T) {
	f := setup(t)
	alice := f.login(t, "alice@example.com")
	code, loc := alice.post("/agents", url.Values{"name": {"bot"}, "harness": {"claude"}, "repo": {"https://tok@github.com/a/b"}})
	if code != http.StatusSeeOther || loc != "/agents/new" {
		t.Fatalf("create with a token in the URL: %d %s", code, loc)
	}
	if agents, _ := f.store.Agents(context.Background(), f.users["alice@example.com"].ID); len(agents) != 0 {
		t.Fatal("an agent was created anyway")
	}
	if _, body := alice.get("/agents/new"); !strings.Contains(body, `name="repo"`) {
		t.Fatal("the form has no repository field")
	}
}
