package turn

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// OutboxMeta is the sidecar whatsapp-send writes next to a file it queues,
// or on its own for a text message.
type OutboxMeta struct {
	File    string `json:"file,omitempty"`
	Caption string `json:"caption,omitempty"`
	Text    string `json:"text,omitempty"`
}

// HostRequest is something a guest command asks the host to do.
type HostRequest struct {
	Op     string `json:"op"` // schedule, unschedule
	Cron   string `json:"cron,omitempty"`
	At     string `json:"at,omitempty"` // a local date and time, or a time of day
	In     string `json:"in,omitempty"` // a duration such as 2h30m
	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Task   string `json:"task,omitempty"` // for unschedule
	// Question and Options are for ask: a poll.
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
	// Port is for preview.
	Port int `json:"port,omitempty"`
}

// HostAnswer is the host's answer, written next to the request.
type HostAnswer struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// TaskInfo is one entry in TasksFile.
type TaskInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	NextRun  string `json:"next_run"`
	Paused   bool   `json:"paused,omitempty"`
	By       string `json:"by"`
	Prompt   string `json:"prompt"`
}

// ParseAt reads a time for a one-off task in loc: "2026-10-19 09:00",
// "2026-10-19T09:00", or a time of day ("09:00", "9am", "9:30pm"),
// which means the next time the clock shows it.
func ParseAt(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	hour, minute, ok := parseClock(strings.ToLower(s))
	if !ok {
		return time.Time{}, fmt.Errorf("%q is not a time; use 2026-10-19 09:00 or 09:00", s)
	}
	now = now.In(loc)
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, loc)
	}
	return t, nil
}

func parseClock(s string) (hour, minute int, ok bool) {
	pm, am := strings.HasSuffix(s, "pm"), strings.HasSuffix(s, "am")
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "pm"), "am"))
	h, m, hasMin := strings.Cut(s, ":")
	hour, err := strconv.Atoi(h)
	if err != nil {
		return 0, 0, false
	}
	if hasMin {
		if minute, err = strconv.Atoi(m); err != nil || len(m) != 2 {
			return 0, 0, false
		}
	} else if !pm && !am {
		return 0, 0, false // a bare "9" is too ambiguous
	}
	if pm || am {
		if hour < 1 || hour > 12 {
			return 0, 0, false
		}
		hour %= 12
		if pm {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 || minute < 0 {
		return 0, 0, false
	}
	return hour, minute, true
}
