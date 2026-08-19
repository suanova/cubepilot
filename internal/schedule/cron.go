// Package schedule is a minimal 5-field cron parser used by the task
// scheduler (FR-M4). Supported syntax per field: "*", "*/n", numbers, and
// comma-separated lists of those. Standard minute/hour/dom/month/dow order.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed 5-field cron expression.
type Cron struct {
	min, hour, dom, mon, dow fieldSet
}

type fieldSet map[int]bool

func parseField(spec string, min, max int) (fieldSet, error) {
	set := fieldSet{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "*":
			for i := min; i <= max; i++ {
				set[i] = true
			}
		case strings.HasPrefix(part, "*/"):
			n, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad step %q", part)
			}
			for i := min; i <= max; i += n {
				set[i] = true
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil || n < min || n > max {
				return nil, fmt.Errorf("bad value %q (range %d-%d)", part, min, max)
			}
			set[n] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("empty field %q", spec)
	}
	return set, nil
}

// Parse parses a 5-field cron expression.
func Parse(expr string) (Cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(fields))
	}
	var c Cron
	var err error
	if c.min, err = parseField(fields[0], 0, 59); err != nil {
		return Cron{}, err
	}
	if c.hour, err = parseField(fields[1], 0, 23); err != nil {
		return Cron{}, err
	}
	if c.dom, err = parseField(fields[2], 1, 31); err != nil {
		return Cron{}, err
	}
	if c.mon, err = parseField(fields[3], 1, 12); err != nil {
		return Cron{}, err
	}
	if c.dow, err = parseField(fields[4], 0, 7); err != nil {
		return Cron{}, err
	}
	if c.dow[7] { // allow both 0 and 7 for Sunday
		c.dow[0] = true
		delete(c.dow, 7)
	}
	return c, nil
}

// matches reports whether t (minute granularity) satisfies the expression.
// Day-of-month and day-of-week are OR-ed when both are restricted, following
// standard cron semantics.
func (c Cron) matches(t time.Time) bool {
	if !c.min[t.Minute()] || !c.hour[t.Hour()] || !c.mon[int(t.Month())] {
		return false
	}
	domAll := len(c.dom) == 31
	dowAll := len(c.dow) == 7
	domOK := c.dom[t.Day()]
	dowOK := c.dow[int(t.Weekday())]
	switch {
	case domAll && dowAll:
		return true
	case domAll:
		return dowOK
	case dowAll:
		return domOK
	default:
		return domOK || dowOK
	}
}

// NextAfter returns the next fire time strictly after t (minute precision).
// It walks minute-by-minute with a ~1-year cap — fine at these task counts.
func (c Cron) NextAfter(t time.Time) time.Time {
	next := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if c.matches(next) {
			return next
		}
		next = next.Add(time.Minute)
	}
	return time.Time{}
}

// NextRun computes the next fire time for a cron expression after now, or the
// zero time when the expression is empty/invalid.
func NextRun(expr string, now time.Time) time.Time {
	if strings.TrimSpace(expr) == "" {
		return time.Time{}
	}
	c, err := Parse(expr)
	if err != nil {
		return time.Time{}
	}
	return c.NextAfter(now)
}
