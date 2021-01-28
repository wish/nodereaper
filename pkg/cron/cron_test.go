package cron

import (
	"testing"
	"time"
)

type test struct {
	t   time.Time
	res bool
}

func TestHourDay(t *testing.T) {
	s, err := ParseStandard("* 2-4 5-10 * *")
	if err != nil {
		t.Error(err)
	}

	tests := []test{
		// Test day of month
		{time.Date(2021, time.March, 4, 3, 0, 0, 0, time.UTC), false},
		{time.Date(2021, time.March, 5, 3, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 6, 3, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 9, 3, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 10, 3, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 11, 3, 0, 0, 0, time.UTC), false},

		// Test hour
		{time.Date(2021, time.March, 5, 1, 0, 0, 0, time.UTC), false},
		{time.Date(2021, time.March, 5, 2, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 5, 3, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 5, 4, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 5, 5, 0, 0, 0, time.UTC), false},

		// Test minute/second
		{time.Date(2021, time.March, 5, 1, 59, 59, 0, time.UTC), false},
		{time.Date(2021, time.March, 5, 2, 0, 0, 0, time.UTC), true},
	}

	for _, test := range tests {
		if s.Matches(test.t) != test.res {
			t.Errorf("Failed testing date %s, got result %v, wanted %v", test.t, !test.res, test.res)
		}
	}
}

func TestWeekendNights(t *testing.T) {
	// Weekends from 6 to 8 pm
	s, err := ParseStandard("* 18-20 * * 0,6")
	if err != nil {
		t.Error(err)
	}

	tests := []test{
		// Test day
		{time.Date(2021, time.March, 5, 18, 0, 0, 0, time.UTC), false},
		{time.Date(2021, time.March, 6, 18, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 7, 18, 0, 0, 0, time.UTC), true},
		{time.Date(2021, time.March, 8, 18, 0, 0, 0, time.UTC), false},
	}

	for _, test := range tests {
		if s.Matches(test.t) != test.res {
			t.Errorf("Failed testing date %s, got result %v, wanted %v", test.t, !test.res, test.res)
		}
	}
}

func TestMinues(t *testing.T) {
	// Weekends from 6 to 8 pm
	s, err := ParseStandard("25-30 * * * *")
	if err != nil {
		t.Error(err)
	}

	tests := []test{
		{time.Date(2021, time.January, 27, 20, 26, 19, 0, time.UTC), true},
	}

	for _, test := range tests {
		if s.Matches(test.t) != test.res {
			t.Errorf("Failed testing date %s, got result %v, wanted %v", test.t, !test.res, test.res)
		}
	}
}
