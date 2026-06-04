package config

import (
	"testing"
	"time"
)

func TestIsTimeAllowed_BoundariesInclusive(t *testing.T) {
	ranges := []TimeRange{{Start: "08:00", End: "10:00"}}

	cases := []struct {
		name string
		now  string
		want bool
	}{
		{name: "before start", now: "07:59", want: false},
		{name: "at start", now: "08:00", want: true},
		{name: "middle", now: "09:15", want: true},
		{name: "at end", now: "10:00", want: true},
		{name: "after end", now: "10:01", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, _, err := IsTimeAllowed(ranges, mustClock(tc.now))
			if err != nil {
				t.Fatalf("IsTimeAllowed returned error: %v", err)
			}
			if allowed != tc.want {
				t.Fatalf("allowed=%v want=%v", allowed, tc.want)
			}
		})
	}
}

func TestIsTimeAllowed_CrossMidnight(t *testing.T) {
	ranges := []TimeRange{{Start: "22:00", End: "06:00"}}

	cases := []struct {
		name string
		now  string
		want bool
	}{
		{name: "before evening start", now: "21:59", want: false},
		{name: "at evening start", now: "22:00", want: true},
		{name: "before midnight", now: "23:30", want: true},
		{name: "after midnight", now: "02:30", want: true},
		{name: "at morning end", now: "06:00", want: true},
		{name: "after morning end", now: "06:01", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, _, err := IsTimeAllowed(ranges, mustClock(tc.now))
			if err != nil {
				t.Fatalf("IsTimeAllowed returned error: %v", err)
			}
			if allowed != tc.want {
				t.Fatalf("allowed=%v want=%v", allowed, tc.want)
			}
		})
	}
}

func TestIsTimeAllowed_EmptyRangesAllowAll(t *testing.T) {
	allowed, matched, err := IsTimeAllowed(nil, mustClock("13:45"))
	if err != nil {
		t.Fatalf("IsTimeAllowed returned error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected empty ranges to allow all")
	}
	if matched != "all-day" {
		t.Fatalf("matched=%q want all-day", matched)
	}
}

func TestIsTimeAllowed_MultipleRanges(t *testing.T) {
	ranges := []TimeRange{
		{Start: "08:00", End: "09:00"},
		{Start: "13:00", End: "14:00"},
		{Start: "22:00", End: "23:00"},
	}

	cases := []struct {
		now  string
		want bool
	}{
		{now: "08:30", want: true},
		{now: "12:59", want: false},
		{now: "13:15", want: true},
		{now: "21:30", want: false},
		{now: "22:30", want: true},
	}

	for _, tc := range cases {
		allowed, _, err := IsTimeAllowed(ranges, mustClock(tc.now))
		if err != nil {
			t.Fatalf("IsTimeAllowed returned error: %v", err)
		}
		if allowed != tc.want {
			t.Fatalf("now=%s allowed=%v want=%v", tc.now, allowed, tc.want)
		}
	}
}

func TestPrepareTasks_AllowsOverlappingRanges(t *testing.T) {
	tasks := []TaskConfig{{
		ID:   "task-1",
		Name: "Task 1",
		TimeRanges: []TimeRange{
			{Start: "08:00", End: "10:00"},
			{Start: "09:00", End: "11:00"},
		},
	}}

	if err := PrepareTasks(tasks); err != nil {
		t.Fatalf("expected overlapping ranges to be allowed, got %v", err)
	}
}

func TestPrepareTasks_AllowsCrossMidnight(t *testing.T) {
	tasks := []TaskConfig{{
		ID:   "task-1",
		Name: "Task 1",
		TimeRanges: []TimeRange{
			{Start: "22:00", End: "06:00"},
			{Start: "12:00", End: "13:00"},
		},
	}}

	if err := PrepareTasks(tasks); err != nil {
		t.Fatalf("expected cross-midnight ranges to be allowed, got %v", err)
	}
}

func TestPrepareTasks_RejectsNonHourlyRanges(t *testing.T) {
	tasks := []TaskConfig{{
		ID:   "task-1",
		Name: "Task 1",
		TimeRanges: []TimeRange{
			{Start: "08:30", End: "10:00"},
		},
	}}

	if err := PrepareTasks(tasks); err == nil {
		t.Fatalf("expected non-hourly ranges to be rejected")
	}
}

func mustClock(value string) time.Time {
	t, err := time.Parse("15:04", value)
	if err != nil {
		panic(err)
	}
	return t
}
