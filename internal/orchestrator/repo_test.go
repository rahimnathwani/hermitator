package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{
		"rahimnathwani/hermitator":                         "https://github.com/rahimnathwani/hermitator hermitator",
		" https://github.com/a/b.git ":                     "https://github.com/a/b.git b",
		"https://gitlab.com/group/sub/proj":                "https://gitlab.com/group/sub/proj proj",
		"https://github.com/a/b/":                          "https://github.com/a/b/ b",
		"http://github.com/a/b":                            "",
		"https://tok@github.com/a/b":                       "",
		"git@github.com:a/b.git":                           "",
		"https://github.com/":                              "",
		"https://github.com/a/b?x=1":                       "",
		"file:///etc":                                      "",
		"a/b; rm -rf /":                                    "",
		"https://github.com/a/" + strings.Repeat("x", 101): "",
	} {
		u, dir, err := ParseRepo(in)
		got := u + " " + dir
		if err != nil {
			got = ""
		}
		if got != want {
			t.Errorf("ParseRepo(%q) = %q (%v), want %q", in, got, err, want)
		}
	}
}

func TestStartFromRepo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	src := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(src, "README.md"), []byte("# shop"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "one")

	waitPrompt := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			turns, _ := e.st.Turns(ctx, e.agent.ID, 5)
			for _, tr := range turns {
				if strings.Contains(tr.Prompt, want) {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no turn asked %q", want)
	}

	e.m.StartFromRepo(e.agent.ID, "file://"+src, "shop", true)
	waitPrompt("cloned into /workspace/shop")
	spec, _ := e.m.Spec(e.agent)
	if b, err := os.ReadFile(filepath.Join(spec.Dir, "workspace", "shop", "README.md")); err != nil || string(b) != "# shop" {
		t.Fatalf("the clone: %q, %v", b, err)
	}
	e.idle(t)

	// Cloning over an existing directory fails, and the agent is told.
	e.m.StartFromRepo(e.agent.ID, "file://"+src, "shop", false)
	waitPrompt("shop already exists in the workspace")
}
