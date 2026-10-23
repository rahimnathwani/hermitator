package orchestrator

import (
	"strings"
	"time"
)

// Texts to a chat are written down before they are sent and crossed off
// once they have gone, so one that fails, or that a restart cuts off, is
// sent again: an agent's reply is often the only record of an hour's work.
// A resent text may be a duplicate (the first try may have arrived after
// all), and says so.

const (
	// A failed text is tried again after Config.ResendInterval, then after
	// twice as long each time, up to resendMax, until resendGiveUp after it
	// was written.
	resendMax    = 10 * time.Minute
	resendGiveUp = 24 * time.Hour
	// sendingFor is how long a text being sent is left alone by the
	// resender, in case the send itself is slow.
	sendingFor = 2 * time.Minute
)

const resendNote = "♻️ Sending again, in case this did not arrive:\n\n"

// resender retries texts that did not go out.
func (m *Manager) resender() {
	tick := time.NewTicker(m.cfg.ResendInterval)
	defer tick.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-tick.C:
			m.resend(now)
		}
	}
}

func (m *Manager) resend(now time.Time) {
	due, err := m.store.DueSends(m.ctx, now)
	if err != nil {
		return
	}
	// One failure in a chat holds back its later texts, keeping their order.
	stuck := map[string]bool{}
	for _, u := range due {
		chat := u.AccountID + "\x00" + u.ChatJID
		switch {
		case stuck[chat]:
			continue
		case now.Sub(u.CreatedAt) > resendGiveUp:
			m.log.Warn("gave up sending a text", "chat", u.ChatJID, "attempts", u.Attempts, "text", clip(u.Text, 80))
			m.store.DeleteSend(m.ctx, u.ID)
			continue
		}
		text := u.Text
		if !strings.HasPrefix(text, resendNote) {
			text = resendNote + text
		}
		if _, err := m.msgr.SendText(m.ctx, u.AccountID, u.ChatJID, text); err != nil {
			stuck[chat] = true
			m.store.SendFailed(m.ctx, u.ID, now.Add(min(m.cfg.ResendInterval<<min(u.Attempts, 10), resendMax)))
			continue
		}
		m.store.DeleteSend(m.ctx, u.ID)
	}
}
