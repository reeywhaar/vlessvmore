// Package bytesize parses and renders human-readable byte counts.
//
// Its own package because both the CLI and the HTTP handlers need it, and the CLI
// already imports the API.
package bytesize

import (
	"fmt"
	"strconv"
	"strings"
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

// Parse reads a size like "100GB", "1.5T", "500M" or a plain byte count.
// Zero means unlimited.
func Parse(s string) (int64, error) {
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

// Format renders a byte count for human output. 0 reads as "unlimited" where
// that is what it means, which the caller decides.
func Format(n int64) string {
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
