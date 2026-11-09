package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var weekdays = map[string]bool{"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}

// validate rejects a schedule that could never match or cannot be parsed.
func (c *BusinessHoursConfig) validate() error {
	if len(c.Days) == 0 {
		return errors.New("days is empty")
	}
	for _, d := range c.Days {
		if !weekdays[strings.ToLower(d)] {
			return fmt.Errorf("unknown day %q (use mon..sun)", d)
		}
	}
	start, err := parseHHMM(c.StartTime)
	if err != nil {
		return fmt.Errorf("start_time: %w", err)
	}
	end, err := parseHHMM(c.EndTime)
	if err != nil {
		return fmt.Errorf("end_time: %w", err)
	}
	if end <= start {
		return fmt.Errorf("end_time %s is not after start_time %s", c.EndTime, c.StartTime)
	}
	return nil
}

// IsWithinBusinessHours checks if current time is within configured business hours.
// Returns true if business hours checking is disabled (all calls accepted).
// The schedule is validated in LoadConfig, so parse errors cannot happen here.
func IsWithinBusinessHours(cfg *BusinessHoursConfig) bool {
	if !cfg.Enabled {
		return true
	}

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}

	now := time.Now().In(loc)

	// Check day of week
	dayName := strings.ToLower(now.Weekday().String()[:3])
	dayAllowed := false
	for _, d := range cfg.Days {
		if strings.EqualFold(d, dayName) {
			dayAllowed = true
			break
		}
	}
	if !dayAllowed {
		return false
	}

	start, _ := parseHHMM(cfg.StartTime)
	end, _ := parseHHMM(cfg.EndTime)
	nowMinutes := now.Hour()*60 + now.Minute()
	return nowMinutes >= start && nowMinutes < end
}

// parseHHMM returns minutes since midnight for a strict "HH:MM" string.
func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

const dateLayout = "2006-01-02"

// CurrentVacation returns the vacation period that includes today
// in the business hours timezone, or nil if there is none.
func CurrentVacation(cfg *VacationConfig, timezone string) *VacationPeriod {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	return findVacation(cfg.Periods, time.Now().In(loc).Format(dateLayout))
}

// findVacation compares YYYY-MM-DD strings directly: their lexical order is date order.
// Both ends of a period are inclusive. If periods overlap, the first one listed wins.
func findVacation(periods []VacationPeriod, today string) *VacationPeriod {
	for i := range periods {
		if p := &periods[i]; p.From <= today && today <= p.To {
			return p
		}
	}
	return nil
}
