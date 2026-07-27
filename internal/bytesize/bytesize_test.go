package bytesize

import "testing"

func TestParse(t *testing.T) {
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
			got, err := Parse(tt.in)
			if tt.ok && err != nil {
				t.Fatalf("Parse(%q): %v", tt.in, err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("Parse(%q) = %d, want an error", tt.in, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// The 1024-based reading of "GB" is a deliberate choice, so it gets a test that will
// fail loudly if someone "corrects" it to 1000.
func TestParseUses1024(t *testing.T) {
	got, err := Parse("100GB")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(100) * 1024 * 1024 * 1024; got != want {
		t.Errorf("Parse(100GB) = %d, want %d (1024-based)", got, want)
	}
}

func TestFormat(t *testing.T) {
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
		if got := Format(tt.in); got != tt.want {
			t.Errorf("Format(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatRoundTrips(t *testing.T) {
	// Anything ParseBytes accepts should format back to something ParseBytes accepts.
	for _, in := range []string{"1K", "100M", "1G", "2T", "1.5G"} {
		n, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(Format(n)); err != nil {
			t.Errorf("Format(%d) = %q, which ParseBytes rejects: %v", n, Format(n), err)
		}
	}
}
