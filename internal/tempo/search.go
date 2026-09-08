package tempo

import (
	"fmt"
	"strconv"
	"time"
)

// ParseTime parses a time string. Accepts:
//   - "" or "now" → current time
//   - relative durations: "1h", "30m", "2d", "1h30m", "1d12h"
//   - RFC3339: "2026-09-08T10:00:00Z"
//
// Relative durations resolve to time.Now().Add(-d).
func ParseTime(s string) (time.Time, error) {
	if s == "" || s == "now" {
		return time.Now(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := parseRelativeDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q: expected RFC3339 or relative duration (e.g. 1h, 30m, 2d, 1d12h)", s)
	}
	return time.Now().Add(-d), nil
}

// parseRelativeDuration extends time.ParseDuration with day ("d") support.
// Accepts sequences of <number><unit> with units: d, h, m, s.
func parseRelativeDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	var total time.Duration
	rem := s

	for rem != "" {
		// Consume digits
		i := 0
		for i < len(rem) && rem[i] >= '0' && rem[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("expected digit in %q at position %d", s, len(s)-len(rem))
		}
		n, err := strconv.Atoi(rem[:i])
		if err != nil {
			return 0, err
		}
		rem = rem[i:]

		// Consume unit (non-digit chars)
		j := 0
		for j < len(rem) && (rem[j] < '0' || rem[j] > '9') {
			j++
		}
		if j == 0 {
			return 0, fmt.Errorf("expected unit after %d in %q", n, s)
		}
		unit := rem[:j]
		rem = rem[j:]

		switch unit {
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		case "h":
			total += time.Duration(n) * time.Hour
		case "m":
			total += time.Duration(n) * time.Minute
		case "s":
			total += time.Duration(n) * time.Second
		default:
			return 0, fmt.Errorf("unknown unit %q in %q", unit, s)
		}
	}
	return total, nil
}
