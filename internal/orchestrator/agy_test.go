package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

const agyToken = `{"token":{"access_token":"ya29.access-token-for-the-test","refresh_token":"1//refresh-token-for-the-test","expiry":"2026-11-22T18:11:45Z"},"auth_method":"consumer"}`

// An Antigravity agent gets its Google token as a file agy refreshes in
// place, and the refreshed file is stored; without one, it is told where
// to sign in.
func TestAgyCredentials(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ag := &store.Agent{Name: "gem", Harness: store.HarnessAgy, UserID: e.user.ID}

	if _, problem := e.m.credentials(ctx, ag); !strings.Contains(problem, "no Google account is signed in") || !strings.Contains(problem, "#antigravity") {
		t.Fatalf("problem without a token = %q", problem)
	}
	e.m.PutSecret(ctx, e.user.ID, store.SecretAgyToken, agyToken)
	c, problem := e.m.credentials(ctx, ag)
	if problem != "" || len(c.files) != 1 || c.files[0].Path != turn.AgyTokenPath || !c.files[0].Sync || c.files[0].Content != agyToken {
		t.Fatalf("credentials = %+v, %q", c.files, problem)
	}
	var redacted bool
	for _, r := range c.redact {
		redacted = redacted || r == "1//refresh-token-for-the-test"
	}
	if !redacted {
		t.Fatal("the refresh token is not redacted from what the agent sends")
	}

	refreshed := strings.Replace(agyToken, "access-token-for-the-test", "a-fresh-access-token", 1)
	e.m.syncFile(ctx, e.user.ID, turn.Event{Type: turn.EventFile, Path: turn.AgyTokenPath, Content: refreshed})
	if got := e.m.Secret(ctx, e.user.ID, store.SecretAgyToken); got != refreshed {
		t.Fatalf("stored after a refresh = %q", got)
	}
}

// With credentials kept out of the sandbox, an Antigravity agent says it
// cannot run rather than being handed the real token.
func TestAgyRefusesPlaceholders(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.cfg.EgressKey = []byte("egress test key")
	e.m.PutSecret(ctx, e.user.ID, store.SecretAgyToken, agyToken)
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	ag.Harness = store.HarnessAgy
	e.st.UpdateAgentSettings(ctx, ag)
	if _, err := e.m.SetEgress(ctx, e.user.ID, true); err != nil {
		t.Fatal(err)
	}
	spec, _ := e.m.Spec(ag)
	os.MkdirAll(spec.Dir, 0o755)
	os.WriteFile(filepath.Join(spec.Dir, "config.json"), []byte(`{"process":{"env":["NODE_EXTRA_CA_CERTS=/opt/hermitator/lib/egress-ca.crt"]}}`), 0o644)
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "hello", true))
	if got := e.rec.wait(t, "text").body; !strings.Contains(got, "Antigravity cannot run while") {
		t.Fatalf("reply = %q", got)
	}
	e.idle(t)
}

func TestAgyModelsAndEfforts(t *testing.T) {
	if got := Efforts(store.HarnessAgy); strings.Join(got, ",") != "low,medium,high,xhigh" {
		t.Fatalf("efforts = %v", got)
	}
	if !validEffort(store.HarnessAgy, "xhigh") || validEffort(store.HarnessAgy, "max") {
		t.Fatal("agy's thinking levels are low to xhigh")
	}
	if got := harnessName(store.HarnessAgy); got != "Antigravity" {
		t.Fatalf("name = %q", got)
	}
}
