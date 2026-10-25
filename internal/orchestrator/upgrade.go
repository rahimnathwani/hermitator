package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// upgradeCommand is /upgrade: the Upgrade button on an agent's page, from
// the chat, for when a new image is announced. Like the button it asks
// first, since the sandbox stops and its background processes with it.
func (m *Manager) upgradeCommand(ctx context.Context, acct *store.Account, ag *store.Agent, arg string, reply func(string)) {
	words := strings.Fields(strings.ToLower(arg))
	all := len(words) > 0 && words[0] == "all"
	yes := len(words) > 0 && words[len(words)-1] == "yes"
	if !all {
		latest := m.NewerImage(ag)
		if latest == "" {
			reply(fmt.Sprintf("*%s* already runs the newest image, %s.", ag.Name, ag.Image))
			return
		}
		if !yes {
			reply(fmt.Sprintf("Upgrading *%s* from %s to %s stops its sandbox now, ending anything running in the background (a dev server, say). Every file is kept, and it boots from the new image with the next message; there is no going back to %s.\n\nSend /upgrade yes to go ahead.", ag.Name, ag.Image, latest, ag.Image))
			return
		}
		_, err := m.UpgradeImage(ctx, ag.ID)
		switch {
		case errors.Is(err, ErrBusy):
			reply("*" + ag.Name + "* is busy (a turn, a terminal or a login is using it). Try again when it is done.")
		case errors.Is(err, ErrUpToDate):
			reply("*" + ag.Name + "* already runs the newest image.")
		case err != nil:
			reply("⚠️ Could not upgrade: " + err.Error())
		default:
			m.store.Audit(ctx, acct.UserID, acct.UserID, "agent.image", "agent="+ag.ID+" image="+latest)
			reply(fmt.Sprintf("⬆️ *%s* now runs %s, from its next message.", ag.Name, latest))
		}
		return
	}

	agents, _ := m.store.Agents(ctx, acct.UserID)
	var behind []string
	for _, a := range agents {
		if m.NewerImage(a) != "" {
			behind = append(behind, a.Name)
		}
	}
	if len(behind) == 0 {
		reply("All your agents run the newest image.")
		return
	}
	if !yes {
		sandboxes := "their sandboxes"
		if len(behind) == 1 {
			sandboxes = "its sandbox"
		}
		reply(fmt.Sprintf("Upgrading %s stops %s now, ending anything running in the background. Every file is kept, and each boots from the new image with its next message.\n\nSend /upgrade all yes to go ahead.",
			strings.Join(behind, ", "), sandboxes))
		return
	}
	var done, busy, failed []string
	for _, a := range agents {
		latest, err := m.UpgradeImage(ctx, a.ID)
		switch {
		case errors.Is(err, ErrUpToDate):
		case errors.Is(err, ErrBusy):
			busy = append(busy, a.Name)
		case err != nil:
			failed = append(failed, a.Name)
		default:
			done = append(done, a.Name)
			m.store.Audit(ctx, acct.UserID, acct.UserID, "agent.image", "agent="+a.ID+" image="+latest)
		}
	}
	text := "Nothing was upgraded."
	if len(done) > 0 {
		text = "⬆️ Upgraded " + strings.Join(done, ", ") + "; each runs the new image from its next message."
	}
	if len(busy) > 0 {
		text += " Busy, so not upgraded: " + strings.Join(busy, ", ") + "; send /upgrade all yes again when they are done."
	}
	if len(failed) > 0 {
		text += " Could not upgrade: " + strings.Join(failed, ", ") + "."
	}
	reply(text)
}
