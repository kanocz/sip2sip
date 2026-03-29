package main

import (
	"strconv"
	"strings"
	"time"
)

// IsWithinBusinessHours checks if current time is within configured business hours.
// Returns true if business hours checking is disabled (all calls accepted).
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

	// Check time of day
	startH, startM := parseTimeStr(cfg.StartTime)
	endH, endM := parseTimeStr(cfg.EndTime)

	nowMinutes := now.Hour()*60 + now.Minute()
	startMinutes := startH*60 + startM
	endMinutes := endH*60 + endM

	return nowMinutes >= startMinutes && nowMinutes < endMinutes
}

func parseTimeStr(t string) (int, int) {
	parts := strings.Split(t, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h, m
}
