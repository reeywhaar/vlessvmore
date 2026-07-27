package cli

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2026-12-31", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), true},
		{"2026-12-31T23:59:00Z", time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), true},
		{"2026-12-31 18:30", time.Date(2026, 12, 31, 18, 30, 0, 0, time.UTC), true},
		{"30d", now.Add(30 * 24 * time.Hour), true},
		{"2w", now.Add(14 * 24 * time.Hour), true},
		{"1y", now.Add(365 * 24 * time.Hour), true},
		{"48h", now.Add(48 * time.Hour), true},
		{"", time.Time{}, false},
		{"tomorrow", time.Time{}, false},
		{"31/12/2026", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseTime(tt.in, now)
			if !tt.ok {
				if err == nil {
					t.Fatalf("ParseTime(%q) = %s, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTime(%q): %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("ParseTime(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"30s", 30 * time.Second, true},
		{"5m", 5 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"1w", 7 * 24 * time.Hour, true},
		{"0.5d", 12 * time.Hour, true},
		{"nope", 0, false},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if tt.ok {
			if err != nil {
				t.Errorf("ParseDuration(%q): %v", tt.in, err)
			} else if got != tt.want {
				t.Errorf("ParseDuration(%q) = %s, want %s", tt.in, got, tt.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseDuration(%q) = %s, want an error", tt.in, got)
		}
	}
}
