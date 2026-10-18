// Package schedule parses five-field cron expressions and finds when they
// next fire.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed cron expression: minute, hour, day of month, month and
// day of week, each a set of allowed values.
type Cron struct {
	expr                     string
	min, hour, dom, mon, dow uint64
	domStar, dowStar         bool
}

var macros = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// Parse reads a cron expression such as "0 9 * * 1-5" or "@daily".
func Parse(expr string) (*Cron, error) {
	expr = strings.TrimSpace(expr)
	spec := expr
	if m, ok := macros[strings.ToLower(spec)]; ok {
		spec = m
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want five fields (minute hour day month weekday)", expr)
	}
	c := &Cron{expr: expr, domStar: f[2] == "*", dowStar: f[4] == "*"}
	var err error
	if c.min, err = field(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q minute: %w", expr, err)
	}
	if c.hour, err = field(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q hour: %w", expr, err)
	}
	if c.dom, err = field(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q day: %w", expr, err)
	}
	if c.mon, err = field(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q month: %w", expr, err)
	}
	if c.dow, err = field(f[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron %q weekday: %w", expr, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	return c, nil
}

func (c *Cron) String() string { return c.expr }

func field(s string, lo, hi int, names map[string]int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
		}
		from, to := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if from, err = value(a, lo, hi, names); err != nil {
				return 0, err
			}
			to = from
			if isRange {
				if to, err = value(b, lo, hi, names); err != nil {
					return 0, err
				}
			} else if hasStep {
				to = hi
			}
			if to < from {
				return 0, fmt.Errorf("range %q runs backwards", rng)
			}
		}
		for v := from; v <= to; v += step {
			set |= 1 << v
		}
	}
	if set == 0 {
		return 0, errors.New("matches nothing")
	}
	return set, nil
}

func value(s string, lo, hi int, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%q is not between %d and %d", s, lo, hi)
	}
	return v, nil
}

// Next is the first time after t, in loc, that c fires. It returns the
// zero time if there is none within five years (such as 30 February).
func (c *Cron) Next(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case c.mon&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case c.hour&(1<<uint(t.Hour())) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case c.min&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// dayMatches follows cron: when both day of month and day of week are
// restricted, either may match.
func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}

// MinGap is the shortest time between consecutive runs over the next
// few, to refuse schedules that would keep an agent awake.
func (c *Cron) MinGap(from time.Time, loc *time.Location) time.Duration {
	gap := time.Duration(1<<63 - 1)
	prev := c.Next(from, loc)
	for range 48 {
		next := c.Next(prev, loc)
		if next.IsZero() || prev.IsZero() {
			break
		}
		gap = min(gap, next.Sub(prev))
		prev = next
	}
	return gap
}
