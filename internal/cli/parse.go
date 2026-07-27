package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseTime reads a timestamp from a flag: RFC3339, a plain date, or a relative
// duration like "30d" for "thirty days from now".
func ParseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// A bare date means the start of that day, so "--expires 2026-12-31" ends access
	// as that day begins. Operators reading it as "valid through the 31st" would be
	// off by one, so the CLI help says "at 00:00 UTC".
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02 15:04", s); err == nil {
		return t.UTC(), nil
	}
	if d, err := ParseDuration(s); err == nil {
		return now.Add(d).UTC().Truncate(time.Second), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2026-12-31), an RFC3339 timestamp, or a duration (30d)", s)
}

// ParseDuration extends time.ParseDuration with day and week units, which are the
// ones that matter for access windows and which the standard library omits.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	for suffix, unit := range map[string]time.Duration{
		"d": 24 * time.Hour,
		"w": 7 * 24 * time.Hour,
		"y": 365 * 24 * time.Hour,
	} {
		if rest, ok := strings.CutSuffix(s, suffix); ok {
			f, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				continue
			}
			return time.Duration(f * float64(unit)), nil
		}
	}
	return time.ParseDuration(s)
}
