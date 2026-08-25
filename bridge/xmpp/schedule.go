package xmpp

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// timeOfDayLayout is what a schedule's times look like: "07:30".
const timeOfDayLayout = "15:04"

// Schedule is a prompt the bridge starts on its own. The zero value schedules
// nothing.
type Schedule struct {
	prompt     string
	at         []string
	continuing bool
}

// Daily runs prompt at each given time of day, in the local time zone. A time is
// "HH:MM"; an invalid one is reported when Run validates its configuration,
// alongside a bad JID.
//
// Each run starts a fresh session, so the conversation that follows a digest is
// about that digest. What the agent knows across those sessions is its memory,
// not its transcript.
func Daily(prompt string, at ...string) Schedule {
	return Schedule{prompt: prompt, at: at}
}

// Continuing returns a schedule whose runs continue the current session instead
// of replacing it. The conversation then never resets on its own, and grows
// until the user sends /new.
func (s Schedule) Continuing() Schedule {
	s.continuing = true
	s.at = append([]string(nil), s.at...)
	return s
}

func (s Schedule) zero() bool { return s.prompt == "" && len(s.at) == 0 }

func (s Schedule) validate() error {
	if s.zero() {
		return nil
	}
	if strings.TrimSpace(s.prompt) == "" {
		return errors.New("xmpp: Schedule with no prompt")
	}
	if len(s.at) == 0 {
		return errors.New("xmpp: Schedule with no time to run at")
	}
	for _, at := range s.at {
		if _, err := time.Parse(timeOfDayLayout, strings.TrimSpace(at)); err != nil {
			return fmt.Errorf("xmpp: invalid Schedule time %q: want HH:MM", at)
		}
	}
	return nil
}

// nextDue is the first slot strictly after now, or the zero time when there is
// none. Times are wall clock in now's zone, so they survive a DST change, and a
// slot that has passed today comes due tomorrow.
func nextDue(now time.Time, at []string) time.Time {
	var next time.Time
	for _, raw := range at {
		t, err := time.Parse(timeOfDayLayout, strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		due := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if !due.After(now) {
			due = due.AddDate(0, 0, 1)
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}
