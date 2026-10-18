package schedule

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	base := time.Date(2026, 10, 17, 23, 30, 0, 0, la) // a Saturday
	cases := []struct{ expr, want string }{
		{"0 9 * * *", "2026-10-18 09:00"},
		{"0 9 * * 1-5", "2026-10-19 09:00"}, // Monday
		{"*/15 * * * *", "2026-10-17 23:45"},
		{"30 23 * * *", "2026-10-18 23:30"}, // strictly after
		{"0 0 1 * *", "2026-11-01 00:00"},
		{"0 12 13 * fri", "2026-10-23 12:00"}, // the 13th or any Friday, whichever is first
		{"@weekly", "2026-10-18 00:00"},
		{"0 9 * jan-mar mon", "2027-01-04 09:00"},
		{"0 9 * * 7", "2026-10-18 09:00"}, // 7 is Sunday
	}
	for _, c := range cases {
		cr, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.expr, err)
		}
		if got := cr.Next(base, la).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("%q next after %s = %s, want %s", c.expr, base.Format("2006-01-02 15:04"), got, c.want)
		}
	}
	// Across the change from daylight saving time, 09:00 stays 09:00.
	cr, _ := Parse("0 9 * * *")
	if got := cr.Next(time.Date(2026, 11, 1, 10, 0, 0, 0, la), la); got.Format("15:04 MST") != "09:00 PST" {
		t.Errorf("after DST ends: %s", got.Format("2006-01-02 15:04 MST"))
	}
	if cr, _ := Parse("0 0 30 2 *"); !cr.Next(base, la).IsZero() {
		t.Error("30 February fired")
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "5-1 * * * *", "*/0 * * * *", "0 9 * * funday"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted", expr)
		}
	}
}

func TestMinGap(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for expr, want := range map[string]time.Duration{
		"* * * * *":   time.Minute,
		"*/5 * * * *": 5 * time.Minute,
		"0,1 9 * * *": time.Minute,
		"0 9 * * 1-5": 24 * time.Hour,
		"0 */2 * * *": 2 * time.Hour,
	} {
		cr, _ := Parse(expr)
		if got := cr.MinGap(now, time.UTC); got != want {
			t.Errorf("MinGap(%q) = %s, want %s", expr, got, want)
		}
	}
}
