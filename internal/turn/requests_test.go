package turn

import (
	"testing"
	"time"
)

func TestParseAt(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 10, 17, 23, 30, 0, 0, la)
	for in, want := range map[string]string{
		"2026-10-19 09:00": "2026-10-19 09:00",
		"2026-10-19T09:15": "2026-10-19 09:15",
		"09:00":            "2026-10-18 09:00",
		"23:45":            "2026-10-17 23:45",
		"9am":              "2026-10-18 09:00",
		"9:30 PM":          "2026-10-18 21:30",
		"12am":             "2026-10-18 00:00",
	} {
		got, err := ParseAt(in, now, la)
		if err != nil || got.Format("2006-01-02 15:04") != want {
			t.Errorf("ParseAt(%q) = %s, %v; want %s", in, got.Format("2006-01-02 15:04"), err, want)
		}
	}
	for _, bad := range []string{"tomorrow", "9", "25:00", "13pm", "9:5"} {
		if _, err := ParseAt(bad, now, la); err == nil {
			t.Errorf("ParseAt(%q) accepted", bad)
		}
	}
}
