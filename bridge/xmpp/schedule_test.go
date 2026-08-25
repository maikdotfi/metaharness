package xmpp

import (
	"strings"
	"testing"
	"time"
)

func TestNextDue(t *testing.T) {
	helsinki, err := time.LoadLocation("Europe/Helsinki")
	if err != nil {
		t.Skip("Europe/Helsinki is unavailable:", err)
	}
	day := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, helsinki)
	}

	tests := []struct {
		name string
		now  time.Time
		at   []string
		want time.Time
	}{
		{
			name: "later today",
			now:  day(2026, time.August, 24, 6, 0),
			at:   []string{"07:30", "18:30"},
			want: day(2026, time.August, 24, 7, 30),
		},
		{
			name: "between two slots",
			now:  day(2026, time.August, 24, 9, 15),
			at:   []string{"07:30", "18:30"},
			want: day(2026, time.August, 24, 18, 30),
		},
		{
			name: "every slot passed rolls to tomorrow",
			now:  day(2026, time.August, 24, 23, 0),
			at:   []string{"07:30", "18:30"},
			want: day(2026, time.August, 25, 7, 30),
		},
		{
			name: "the only slot is exactly now",
			now:  day(2026, time.August, 24, 7, 30),
			at:   []string{"07:30"},
			want: day(2026, time.August, 25, 7, 30),
		},
		{
			name: "unsorted and duplicated",
			now:  day(2026, time.August, 24, 6, 0),
			at:   []string{"18:30", "07:30", "07:30", "12:00"},
			want: day(2026, time.August, 24, 7, 30),
		},
		{
			name: "no times is never due",
			now:  day(2026, time.August, 24, 6, 0),
			at:   nil,
			want: time.Time{},
		},
		// Helsinki jumps 03:00 to 04:00 on 2026-03-29, so 03:30 is a wall clock
		// that does not exist. It still comes due, once, two and a half hours later.
		{
			name: "a slot inside the spring forward gap",
			now:  day(2026, time.March, 29, 1, 0),
			at:   []string{"03:30"},
			want: day(2026, time.March, 29, 1, 0).Add(150 * time.Minute),
		},
		// The autumn repeat is the same wall clock twice; one firing is enough.
		{
			name: "a slot inside the autumn repeat",
			now:  day(2026, time.October, 25, 2, 0),
			at:   []string{"03:30"},
			want: day(2026, time.October, 25, 3, 30),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextDue(tc.now, tc.at)
			if !got.Equal(tc.want) {
				t.Errorf("nextDue(%s, %v) = %s, want %s", tc.now, tc.at, got, tc.want)
			}
		})
	}
}

func TestScheduleValidation(t *testing.T) {
	tests := []struct {
		name string
		s    Schedule
		want string
	}{
		{"the zero value schedules nothing", Schedule{}, ""},
		{"a daily schedule", Daily("digest", "07:30", "18:30"), ""},
		{"a continuing schedule", Daily("digest", "07:30").Continuing(), ""},
		{"a bad hour", Daily("digest", "07:30", "25:00"), "25:00"},
		{"nonsense", Daily("digest", "morning"), "morning"},
		{"no times", Daily("digest"), "time"},
		{"no prompt", Daily("", "07:30"), "prompt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("validate() error = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("validate() error = nil, want one naming %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("validate() error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestContinuingLeavesTheOriginalAlone(t *testing.T) {
	daily := Daily("digest", "07:30")
	if daily.Continuing(); daily.continuing {
		t.Error("Continuing() mutated the schedule it was called on")
	}
}
