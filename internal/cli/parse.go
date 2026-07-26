package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// byteUnits maps a size suffix to its multiplier.
//
// KB/MB/GB are 1024-based here, not 1000-based. That is technically the wrong name
// for the value, but it is what every VPN panel and every operator writing
// "--quota 100GB" means, and quietly giving them 7% less than they asked for would be
// the worse surprise. The IEC spellings are accepted as synonyms.
var byteUnits = map[string]int64{
	"":    1,
	"B":   1,
	"K":   1 << 10,
	"KB":  1 << 10,
	"KIB": 1 << 10,
	"M":   1 << 20,
	"MB":  1 << 20,
	"MIB": 1 << 20,
	"G":   1 << 30,
	"GB":  1 << 30,
	"GIB": 1 << 30,
	"T":   1 << 40,
	"TB":  1 << 40,
	"TIB": 1 << 40,
	"P":   1 << 50,
	"PB":  1 << 50,
	"PIB": 1 << 50,
}

// ParseBytes reads a size like "100GB", "1.5T", "500M" or a plain byte count.
// Zero means unlimited.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Split the numeric head from the unit tail.
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '-' || s[i] == '+') {
		i++
	}
	num, unit := s[:i], strings.ToUpper(strings.TrimSpace(s[i:]))
	if num == "" {
		return 0, fmt.Errorf("size %q has no number", s)
	}
	mult, ok := byteUnits[unit]
	if !ok {
		return 0, fmt.Errorf("size %q has an unknown unit %q; use B, KB, MB, GB, TB", s, unit)
	}

	// Parsed as a float so "1.5G" works, then rounded.
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("size %q must not be negative", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatBytes renders a byte count for human output. 0 reads as "unlimited" where
// that is what it means, which the caller decides.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	suffixes := []string{"KB", "MB", "GB", "TB", "PB"}
	var suffix string
	for _, s := range suffixes {
		value /= unit
		suffix = s
		if value < unit {
			break
		}
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

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
