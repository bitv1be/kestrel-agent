// Package sched implements a small, dependency-free cron scheduler.
package sched

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression.
type Schedule struct {
	expr                          string
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}
var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// Parse parses "minute hour day-of-month month day-of-week". Supported syntax:
// *, ranges a-b, steps */n and a-b/n, lists, and month/day names.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields, got %d", len(fields))
	}
	s := &Schedule{expr: strings.Join(fields, " ")}
	var err error
	if s.minute, _, err = parseField(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if s.hour, _, err = parseField(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if s.dom, s.domStar, err = parseField(fields[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if s.month, _, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if s.dow, s.dowStar, err = parseField(fields[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if s.dow&(1<<7) != 0 {
		s.dow |= 1 // Sunday can be 0 or 7
		s.dow &^= 1 << 7
	}
	return s, nil
}

func parseField(field string, min, max int, names map[string]int) (uint64, bool, error) {
	var bits uint64
	if field == "*" || field == "?" {
		for i := min; i <= max; i++ {
			bits |= 1 << uint(i)
		}
		return bits, true, nil
	}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, false, fmt.Errorf("empty list item")
		}
		step := 1
		rangePart := part
		if i := strings.IndexByte(part, '/'); i >= 0 {
			rangePart = part[:i]
			var err error
			step, err = strconv.Atoi(part[i+1:])
			if err != nil || step <= 0 {
				return 0, false, fmt.Errorf("invalid step %q", part[i+1:])
			}
		}
		lo, hi := min, max
		switch {
		case rangePart == "*" || rangePart == "?":
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			var err error
			lo, err = parseValue(a, names)
			if err != nil {
				return 0, false, err
			}
			hi, err = parseValue(b, names)
			if err != nil {
				return 0, false, err
			}
			if lo > hi {
				return 0, false, fmt.Errorf("range %q is descending", rangePart)
			}
		default:
			v, err := parseValue(rangePart, names)
			if err != nil {
				return 0, false, err
			}
			lo, hi = v, v
		}
		if lo < min || hi > max {
			return 0, false, fmt.Errorf("value out of range %d-%d: %q", min, max, part)
		}
		for i := lo; i <= hi; i += step {
			bits |= 1 << uint(i)
		}
	}
	if bits == 0 {
		return 0, false, fmt.Errorf("no values")
	}
	return bits, false, nil
}

func parseValue(s string, names map[string]int) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if names != nil {
		if v, ok := names[s]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return v, nil
}

// Next returns the first matching time strictly after from, in from's
// location.
func (s *Schedule) Next(from time.Time) time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domStar && s.dowStar {
		return true
	}
	if s.domStar {
		return dowOK
	}
	if s.dowStar {
		return domOK
	}
	return domOK || dowOK
}

func (s *Schedule) String() string { return s.expr }
