package cli

import (
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"", 0, true},
		{"0", 0, true},
		{"1024", 1024, true},
		{"1K", 1 << 10, true},
		{"1KB", 1 << 10, true},
		{"1KiB", 1 << 10, true},
		{"100M", 100 << 20, true},
		{"100MB", 100 << 20, true},
		{"1G", 1 << 30, true},
		{"100GB", 100 << 30, true},
		{"1.5G", 1610612736, true},
		{"2T", 2 << 40, true},
		{" 5 GB ", 5 << 30, true},
		{"5gb", 5 << 30, true},
		{"-1", 0, false},
		{"-1G", 0, false},
		{"GB", 0, false},
		{"1XB", 0, false},
		{"abc", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseBytes(tt.in)
			if tt.ok && err != nil {
				t.Fatalf("ParseBytes(%q): %v", tt.in, err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("ParseBytes(%q) = %d, want an error", tt.in, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// The 1024-based reading of "GB" is a deliberate choice, so it gets a test that will
// fail loudly if someone "corrects" it to 1000.
func TestParseBytesUses1024(t *testing.T) {
	got, err := ParseBytes("100GB")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(100) * 1024 * 1024 * 1024; got != want {
		t.Errorf("ParseBytes(100GB) = %d, want %d (1024-based)", got, want)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{100 << 20, "100 MB"},
		{1 << 30, "1.0 GB"},
		{1 << 40, "1.0 TB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.in); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatBytesRoundTrips(t *testing.T) {
	// Anything ParseBytes accepts should format back to something ParseBytes accepts.
	for _, in := range []string{"1K", "100M", "1G", "2T", "1.5G"} {
		n, err := ParseBytes(in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseBytes(FormatBytes(n)); err != nil {
			t.Errorf("FormatBytes(%d) = %q, which ParseBytes rejects: %v", n, FormatBytes(n), err)
		}
	}
}

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
