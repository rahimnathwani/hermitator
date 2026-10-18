package web

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

var agentName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,39}$`)

// agentView is an agent with its live status.
type agentView struct {
	*store.Agent
	Status orchestrator.Status
	Routes []routeView
}

func (a agentView) Busy() bool { return a.Status.Busy }

// State prefers the live state over the stored one.
func (a agentView) State() string {
	if a.Status.State != "" {
		return a.Status.State
	}
	return a.Agent.State
}

type routeView struct {
	*store.Route
	Account string
	Held    int
}

func (s *Server) agentFor(r *http.Request, v *viewer) (*store.Agent, error) {
	a, err := s.Store.Agent(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if a.UserID != v.User.ID {
		return nil, store.ErrNotFound
	}
	return a, nil
}

func (s *Server) view(r *http.Request, a *store.Agent, withRoutes bool) agentView {
	av := agentView{Agent: a, Status: s.Agents.Status(a.ID)}
	if withRoutes {
		routes, _ := s.Store.RoutesForAgent(r.Context(), a.ID)
		for _, rt := range routes {
			name := rt.AccountID
			if acct, err := s.Store.Account(r.Context(), rt.AccountID); err == nil {
				name = "+" + acct.Phone
			}
			av.Routes = append(av.Routes, routeView{Route: rt, Account: name, Held: s.Store.HeldCount(r.Context(), rt.AccountID, rt.ChatJID)})
		}
	}
	return av
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	agents, _ := s.Store.Agents(ctx, v.User.ID)
	accounts, _ := s.Store.Accounts(ctx, v.User.ID)
	secrets, _ := s.Store.SecretTimes(ctx, v.User.ID)
	var views []agentView
	linked := false
	for _, a := range agents {
		av := s.view(r, a, true)
		linked = linked || len(av.Routes) > 0
		views = append(views, av)
	}
	_, hasClaude := secrets[store.SecretClaudeToken]
	_, hasPi := secrets[store.SecretPiAuth]
	_, hasDeepSeek := secrets[store.SecretDeepSeekKey]
	_, hasOpenRouter := secrets[store.SecretOpenRouterKey]
	s.render(w, r, v, "dashboard.html", "Agents", "agents", map[string]any{
		"Agents":      views,
		"Accounts":    accounts,
		"Images":      s.Images.Families(),
		"Providers":   orchestrator.PiProviders,
		"CanCreate":   len(agents) < v.User.MaxAgents,
		"MaxAgents":   v.User.MaxAgents,
		"HasAccount":  len(accounts) > 0,
		"HasModel":    hasClaude || hasPi || hasDeepSeek || hasOpenRouter,
		"HasAgent":    len(agents) > 0,
		"HasLink":     linked,
		"ShowGuide":   len(accounts) == 0 || len(agents) == 0 || !linked,
		"HasClaude":   hasClaude,
		"HasAnyPiKey": hasPi || hasDeepSeek || hasOpenRouter,
	})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, v *viewer) {
	name := formValue(r, "name")
	if !agentName.MatchString(name) {
		s.back(w, r, "/", "Agent names are up to 40 letters, digits, spaces, dots, dashes or underscores.")
		return
	}
	harness := formValue(r, "harness")
	if harness != store.HarnessClaude && harness != store.HarnessPi {
		s.back(w, r, "/", "Pick a harness.")
		return
	}
	a := &store.Agent{Name: name, Harness: harness, Model: formValue(r, "model")}
	if harness == store.HarnessPi {
		a.PiProvider = formValue(r, "provider")
	}
	image := formValue(r, "image")
	if image == "" {
		image = "universal"
	}
	err := s.Agents.CreateAgent(r.Context(), v.User, a, image)
	switch {
	case errors.Is(err, orchestrator.ErrQuota):
		s.back(w, r, "/", "You already have as many agents as your account allows.")
		return
	case errors.Is(err, store.ErrConflict):
		s.back(w, r, "/", "You already have an agent with that name.")
		return
	case err != nil:
		s.back(w, r, "/", "Could not create the agent: "+err.Error())
		return
	}
	s.audit(r, v, "agent.create", "agent="+a.ID+" name="+a.Name)
	http.Redirect(w, r, "/agents/"+a.ID, http.StatusSeeOther)
}

func (s *Server) agentPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		s.fail(w, r, v, http.StatusNotFound, "No such agent.")
		return
	}
	ctx := r.Context()
	code, expires, _ := s.Agents.LinkCode(ctx, a.ID, false)
	turns, _ := s.Store.Turns(ctx, a.ID, 30)
	accounts, _ := s.Store.Accounts(ctx, v.User.ID)
	s.render(w, r, v, "agent.html", a.Name, "agents", map[string]any{
		"Agent":         s.view(r, a, true),
		"LinkCode":      code,
		"LinkExpires":   expires,
		"Turns":         turns,
		"Accounts":      accounts,
		"Providers":     orchestrator.PiProviders,
		"Memory":        []int{1024, 2048, 4096, 8192, 16384},
		"NewerImage":    s.Agents.NewerImage(a),
		"ModelLists":    modelLists(),
		"ModelList":     modelListID(a.Harness, a.PiProvider),
		"ClaudeEfforts": orchestrator.ClaudeEfforts,
		"PiEfforts":     orchestrator.PiEfforts,
	})
}

// modelLists are the model suggestions for each harness and pi provider,
// keyed by the id of the datalist the settings form uses.
func modelLists() map[string][]string {
	lists := map[string][]string{modelListID(store.HarnessClaude, ""): orchestrator.ClaudeModels}
	for _, p := range orchestrator.PiProviders {
		lists[modelListID(store.HarnessPi, p.ID)] = orchestrator.ModelSuggestions(store.HarnessPi, p.ID)
	}
	return lists
}

func modelListID(harness, provider string) string {
	if harness == store.HarnessPi {
		if provider == "" {
			provider = orchestrator.ProviderCodex
		}
		return "models-pi-" + provider
	}
	return "models-claude"
}

func (s *Server) agentCard(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.partial(w, "agent-card", s.view(r, a, true))
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := formValue(r, "name")
	if !agentName.MatchString(name) {
		s.back(w, r, "/agents/"+a.ID, "Agent names are up to 40 letters, digits, spaces, dots, dashes or underscores.")
		return
	}
	harness := formValue(r, "harness")
	if harness != store.HarnessClaude && harness != store.HarnessPi {
		harness = a.Harness
	}
	idle, _ := strconv.Atoi(formValue(r, "idle_minutes"))
	idle = min(max(idle, 1), 24*60)
	memory, _ := strconv.Atoi(formValue(r, "memory_mb"))
	memory = min(max(memory, 512), 65536)
	a.Name, a.Harness, a.Model = name, harness, formValue(r, "model")
	a.Effort = formValue(r, "effort_"+harness)
	if !slices.Contains(orchestrator.Efforts(harness), a.Effort) {
		a.Effort = ""
	}
	a.PiProvider = formValue(r, "provider")
	a.IdleTimeout = time.Duration(idle) * time.Minute
	a.MemoryMB = memory
	switch err := s.Agents.UpdateAgent(r.Context(), a); {
	case errors.Is(err, orchestrator.ErrBusy):
		s.back(w, r, "/agents/"+a.ID, "The agent is in the middle of a turn; switch harness when it is done.")
	case errors.Is(err, store.ErrConflict):
		s.back(w, r, "/agents/"+a.ID, "You already have an agent with that name.")
	case err != nil:
		s.back(w, r, "/agents/"+a.ID, "Could not save: "+err.Error())
	default:
		s.audit(r, v, "agent.update", "agent="+a.ID)
		s.back(w, r, "/agents/"+a.ID, "Saved.")
	}
}

func (s *Server) upgradeImage(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	latest, err := s.Agents.UpgradeImage(r.Context(), a.ID)
	switch {
	case errors.Is(err, orchestrator.ErrBusy):
		s.back(w, r, "/agents/"+a.ID, "The agent is busy (a turn, a terminal or a login is using it).")
	case errors.Is(err, orchestrator.ErrUpToDate):
		s.back(w, r, "/agents/"+a.ID, "Already on the newest image.")
	case err != nil:
		s.back(w, r, "/agents/"+a.ID, "Could not upgrade: "+err.Error())
	default:
		s.audit(r, v, "agent.image", "agent="+a.ID+" image="+latest)
		s.back(w, r, "/agents/"+a.ID, "Now on "+latest+". It boots from the new image with the next message.")
	}
}

func (s *Server) wakeAgent(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	start := time.Now()
	if err := s.Agents.Wake(r.Context(), a.ID); err != nil {
		s.back(w, r, "/agents/"+a.ID, "Could not wake the agent: "+err.Error())
		return
	}
	s.back(w, r, "/agents/"+a.ID, "Awake in "+humanDuration(time.Since(start))+".")
}

func (s *Server) sleepAgent(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	state, err := s.Agents.Sleep(r.Context(), a.ID)
	switch {
	case errors.Is(err, orchestrator.ErrBusy):
		s.back(w, r, "/agents/"+a.ID, "The agent is busy (a turn, a terminal or a login is using it).")
	case err != nil:
		s.back(w, r, "/agents/"+a.ID, "Could not suspend: "+err.Error())
	case state == sandbox.Checkpointed:
		s.back(w, r, "/agents/"+a.ID, "Checkpointed to disk, background processes included.")
	default:
		s.back(w, r, "/agents/"+a.ID, "Stopped.")
	}
}

func (s *Server) stopTurn(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.Agents.Abort(a.ID) {
		s.back(w, r, "/agents/"+a.ID, "Stopping the current turn.")
	} else {
		s.back(w, r, "/agents/"+a.ID, "Nothing was running.")
	}
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Agents.NewSession(r.Context(), a.ID); err != nil {
		s.back(w, r, "/agents/"+a.ID, "The agent is in the middle of a turn.")
		return
	}
	s.back(w, r, "/agents/"+a.ID, "The next message starts a fresh conversation.")
}

func (s *Server) newLinkCode(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, err := s.Agents.LinkCode(r.Context(), a.ID, true); err != nil {
		s.back(w, r, "/agents/"+a.ID, "Could not make a code: "+err.Error())
		return
	}
	s.back(w, r, "/agents/"+a.ID, "")
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	text := formValue(r, "text")
	if text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.Agents.Prompt(a.ID, text)
	s.audit(r, v, "agent.prompt", "agent="+a.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if formValue(r, "confirm") != a.Name {
		s.back(w, r, "/agents/"+a.ID, "Type the agent's name to confirm deleting it.")
		return
	}
	if err := s.Agents.DeleteAgent(r.Context(), a.ID); err != nil {
		s.back(w, r, "/agents/"+a.ID, "Could not delete: "+err.Error())
		return
	}
	s.audit(r, v, "agent.delete", "agent="+a.ID+" name="+a.Name)
	s.setFlash(w, "Deleted "+a.Name+" and everything in its sandbox.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	route, err := s.Store.Route(ctx, formValue(r, "account"), formValue(r, "chat"))
	if err != nil || route.AgentID != a.ID {
		http.NotFound(w, r)
		return
	}
	s.Store.DeleteRoute(ctx, route.AccountID, route.ChatJID)
	s.audit(r, v, "route.unlink", "agent="+a.ID+" chat="+route.ChatJID)
	s.back(w, r, "/agents/"+a.ID, "Unlinked.")
}

func (s *Server) routeTriggers(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	route, err := s.Store.Route(ctx, formValue(r, "account"), formValue(r, "chat"))
	if err != nil || route.AgentID != a.ID {
		http.NotFound(w, r)
		return
	}
	owner, others := formValue(r, "owner"), formValue(r, "others")
	if !store.ValidTrigger(owner) || !store.ValidTrigger(others) {
		http.Error(w, "unknown setting", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetRouteTriggers(ctx, route.AccountID, route.ChatJID, owner, others); err != nil {
		s.back(w, r, "/agents/"+a.ID, "Could not save: "+err.Error())
		return
	}
	s.audit(r, v, "route.triggers", "agent="+a.ID+" chat="+route.ChatJID+" owner="+owner+" others="+others)
	s.back(w, r, "/agents/"+a.ID, "Saved.")
}

// agentEvents streams an agent's live activity to its page.
func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	events, stop := s.Agents.Subscribe(a.ID)
	defer stop()
	st, err := newStream(w)
	if err != nil {
		return
	}
	status := s.Agents.Status(a.ID)
	state := status.State
	if state == "" {
		state = a.State
	}
	st.send("state", orchestrator.UIEvent{Kind: "state", State: state})
	tick := time.NewTicker(pingInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			st.send(ev.Kind, ev)
		case <-tick.C:
			st.ping()
		}
	}
}
