package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// GitHubUser is who a GitHub token signs in as.
type GitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// gitHubUser asks GitHub who a token belongs to.
func gitHubUser(ctx context.Context, token string) (GitHubUser, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return GitHubUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("GitHub said %s", resp.Status)
	}
	var u GitHubUser
	err = json.NewDecoder(resp.Body).Decode(&u)
	if err == nil && u.Login == "" {
		err = errors.New("GitHub did not say whose token it is")
	}
	return u, err
}

func (s *Server) saveGitHub(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	if r.FormValue("remove") != "" {
		s.Agents.PutSecret(ctx, v.User.ID, store.SecretGitHubToken, "")
		s.Store.SetGitHubLogin(ctx, v.User.ID, "")
		s.Agents.ForgetSecrets(v.User.ID)
		s.audit(r, v, "secret.delete", store.SecretGitHubToken)
		s.back(w, r, "/settings", "GitHub disconnected. Agents can no longer push or use gh as you.")
		return
	}
	token := strings.TrimSpace(r.FormValue("value"))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		s.back(w, r, "/settings", "Paste a GitHub token: run `gh auth token`, or make a fine-grained personal access token.")
		return
	}
	lookup := s.GitHub
	if lookup == nil {
		lookup = gitHubUser
	}
	u, err := lookup(ctx, token)
	if err != nil {
		s.back(w, r, "/settings", "GitHub did not accept that token: "+err.Error())
		return
	}
	if err := s.Agents.PutSecret(ctx, v.User.ID, store.SecretGitHubToken, token); err != nil {
		s.back(w, r, "/settings", "Could not save: "+err.Error())
		return
	}
	s.Store.SetGitHubLogin(ctx, v.User.ID, u.Login)
	s.Agents.ForgetSecrets(v.User.ID)
	msg := "Connected to GitHub as @" + u.Login + "."
	if v.User.GitName == "" || v.User.GitEmail == "" {
		name, email := u.Name, u.Email
		if name == "" {
			name = u.Login
		}
		if email == "" {
			// GitHub's own no-reply address still links commits to the account.
			email = fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
		}
		s.Store.SetGitIdentity(ctx, v.User.ID, name, email)
		msg += " Agents commit as " + name + " <" + email + ">; change that below."
	}
	s.audit(r, v, "secret.put", store.SecretGitHubToken+" @"+u.Login)
	s.back(w, r, "/settings", msg)
}

func (s *Server) saveIdentity(w http.ResponseWriter, r *http.Request, v *viewer) {
	name, email := formValue(r, "git_name"), formValue(r, "git_email")
	if (name == "") != (email == "") || (email != "" && !strings.Contains(email, "@")) || len(name) > 200 || len(email) > 200 {
		s.back(w, r, "/settings", "Give both a name and an email address for commits, or neither.")
		return
	}
	s.Store.SetGitIdentity(r.Context(), v.User.ID, name, email)
	s.back(w, r, "/settings", "Saved.")
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// reservedEnv are variables hermitator or the harnesses set themselves.
var reservedEnv = []string{"PATH", "HOME", "TZ", "USER", "SHELL", "PWD", "LANG", "IS_SANDBOX", "DISABLE_AUTOUPDATER",
	"GH_TOKEN", "GITHUB_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "DEEPSEEK_API_KEY", "OPENROUTER_API_KEY"}

func reserved(name string) bool {
	for _, prefix := range []string{"HERMITATOR_", "ANTHROPIC_", "CLAUDE_", "GIT_", "LD_", "NODE_OPTIONS", "PI_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return slices.Contains(reservedEnv, name)
}

func (s *Server) saveEnvSecret(w http.ResponseWriter, r *http.Request, v *viewer) {
	name := strings.ToUpper(formValue(r, "name"))
	value := r.FormValue("value")
	switch {
	case !envName.MatchString(name):
		s.back(w, r, "/settings", "A secret's name is an environment variable name: capital letters, digits and underscores, like STRIPE_KEY.")
		return
	case reserved(name):
		s.back(w, r, "/settings", name+" is set by hermitator itself; choose another name.")
		return
	case strings.TrimSpace(value) == "" || len(value) > 8192:
		s.back(w, r, "/settings", "Paste the secret's value (at most 8 KB).")
		return
	}
	var hosts []string
	for _, h := range strings.FieldsFunc(strings.ToLower(formValue(r, "hosts")), func(r rune) bool { return r == ',' || r == ' ' }) {
		if !orchestrator.ValidHost(h) {
			s.back(w, r, "/settings", h+" is not a host name; give names like api.stripe.com, separated by spaces.")
			return
		}
		hosts = append(hosts, h)
	}
	if err := s.Agents.PutSecret(r.Context(), v.User.ID, store.SecretEnvPrefix+name, value); err != nil {
		s.back(w, r, "/settings", "Could not save: "+err.Error())
		return
	}
	s.Store.SetSecretHosts(r.Context(), v.User.ID, name, hosts)
	s.Agents.ForgetSecrets(v.User.ID)
	s.audit(r, v, "secret.put", store.SecretEnvPrefix+name)
	s.back(w, r, "/settings", "Saved "+name+". Agents have it from their next message.")
}

func (s *Server) deleteEnvSecret(w http.ResponseWriter, r *http.Request, v *viewer) {
	name := formValue(r, "name")
	if !envName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	s.Agents.PutSecret(r.Context(), v.User.ID, store.SecretEnvPrefix+name, "")
	s.Store.SetSecretHosts(r.Context(), v.User.ID, name, nil)
	s.Agents.ForgetSecrets(v.User.ID)
	s.audit(r, v, "secret.delete", store.SecretEnvPrefix+name)
	s.back(w, r, "/settings", "Removed "+name+".")
}

// envSecrets lists the user's own secrets with when each was saved.
type envSecret struct {
	Name  string
	Saved time.Time
	Hosts []string
}

func envSecrets(times map[string]time.Time, hosts map[string][]string) []envSecret {
	var out []envSecret
	for name, at := range times {
		if key, ok := strings.CutPrefix(name, store.SecretEnvPrefix); ok {
			out = append(out, envSecret{key, at, hosts[key]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// saveEgress turns the egress proxy on or off for the user's agents.
func (s *Server) saveEgress(w http.ResponseWriter, r *http.Request, v *viewer) {
	on := formValue(r, "egress") == "on"
	waiting, err := s.Agents.SetEgress(r.Context(), v.User.ID, on)
	switch {
	case errors.Is(err, orchestrator.ErrNoEgress):
		s.back(w, r, "/settings", "This server runs agents without gVisor, so there is no egress proxy to turn on.")
		return
	case err != nil:
		s.back(w, r, "/settings", "Could not save: "+err.Error())
		return
	}
	s.audit(r, v, "settings.egress", fmt.Sprint(on))
	msg := "Credentials go into your agents' sandboxes as before."
	if on {
		msg = "Your agents now hold placeholders instead of credentials, from their next message."
		if waiting > 0 {
			msg += fmt.Sprintf(" %d of them are busy and keep real credentials until they next boot afresh.", waiting)
		}
	}
	s.back(w, r, "/settings", msg)
}
