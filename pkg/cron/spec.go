package cron

import (
	"time"
)

// Schedule specifies a duty cycle (to the second granularity), based on a
// traditional crontab specification. It is computed initially and stored as bit sets.
type Schedule struct {
	Second, Minute, Hour, Dom, Month, Dow uint64
	source                                string
}

// bounds provides a range of acceptable values (plus a map of name to value).
type bounds struct {
	min, max uint
	names    map[string]uint
}

// Source returns the string from which the schedule was constructed
func (s *Schedule) Source() string {
	return s.source
}

// The bounds for each field.
var (
	seconds = bounds{0, 59, nil}
	minutes = bounds{0, 59, nil}
	hours   = bounds{0, 23, nil}
	dom     = bounds{1, 31, nil}
	months  = bounds{1, 12, map[string]uint{
		"jan": 1,
		"feb": 2,
		"mar": 3,
		"apr": 4,
		"may": 5,
		"jun": 6,
		"jul": 7,
		"aug": 8,
		"sep": 9,
		"oct": 10,
		"nov": 11,
		"dec": 12,
	}}
	dow = bounds{0, 6, map[string]uint{
		"sun": 0,
		"mon": 1,
		"tue": 2,
		"wed": 3,
		"thu": 4,
		"fri": 5,
		"sat": 6,
	}}
)

const (
	// Set the top bit if a star was included in the expression.
	starBit = 1 << 63
)

// Matches describes whether the given time matches the cron spec
func (s *Schedule) Matches(t time.Time) bool {
	monthMatches := 1<<uint(t.Month())&s.Month != 0
	dayMatches := dayMatches(s, t)
	hourMatches := 1<<uint(t.Hour())&s.Hour != 0
	minuteMatches := 1<<uint(t.Minute())&s.Minute != 0
	secondMatches := 1<<uint(t.Second())&s.Second != 0

	return monthMatches && dayMatches && hourMatches && minuteMatches && secondMatches
}

// dayMatches returns true if the schedule's day-of-week and day-of-month
// restrictions are satisfied by the given time.
func dayMatches(s *Schedule, t time.Time) bool {
	var (
		domMatch bool = 1<<uint(t.Day())&s.Dom > 0
		dowMatch bool = 1<<uint(t.Weekday())&s.Dow > 0
	)
	if s.Dom&starBit > 0 || s.Dow&starBit > 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}
