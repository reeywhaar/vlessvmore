package ids

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewShape(t *testing.T) {
	id, err := NewUser()
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	if !strings.HasPrefix(id, UserPrefix) {
		t.Errorf("id %q missing prefix %q", id, UserPrefix)
	}
	if got := len(id) - len(UserPrefix); got != EncodedLen {
		t.Errorf("id %q body length = %d, want %d", id, got, EncodedLen)
	}
	if !Valid(UserPrefix, id) {
		t.Errorf("Valid(%q) = false", id)
	}
	// The alphabet excludes I, L, O and U so ids cannot be misread.
	if strings.ContainsAny(id[len(UserPrefix):], "ILOU") {
		t.Errorf("id %q contains an ambiguous character", id)
	}
}

func TestPrefixesDiffer(t *testing.T) {
	u, err := NewUser()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if Valid(UserPrefix, tok) || Valid(TokenPrefix, u) {
		t.Error("user and token ids must not validate against each other's prefix")
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 500)
	for range 500 {
		id, err := NewUser()
		if err != nil {
			t.Fatalf("NewUser: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// Time-sortability is the reason for the timestamp prefix: `ORDER BY id` must be
// chronological, and eyeballing two ids should tell you which is older.
func TestIDsSortChronologically(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var got []string
	for i := range 20 {
		id, err := newAt(UserPrefix, base.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if !slices.IsSorted(got) {
		t.Errorf("ids generated in time order are not lexicographically sorted:\n%s", strings.Join(got, "\n"))
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	want := time.Date(2026, 7, 26, 12, 34, 56, 0, time.UTC)
	id, err := newAt(UserPrefix, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Timestamp(UserPrefix, id)
	if err != nil {
		t.Fatalf("Timestamp(%q): %v", id, err)
	}
	if !got.Equal(want) {
		t.Errorf("Timestamp = %s, want %s", got, want)
	}
}

func TestValidRejectsJunk(t *testing.T) {
	good, err := NewUser()
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{
		"",
		"u_",
		"u_short",
		good + "X",                             // too long
		good[:len(good)-1],                     // too short
		"x_" + good[2:],                        // wrong prefix
		"u_" + strings.Repeat("I", EncodedLen), // outside the alphabet
	}
	for _, s := range tests {
		if Valid(UserPrefix, s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestSecret(t *testing.T) {
	seen := make(map[string]bool, 50)
	for range 50 {
		s, err := Secret(20)
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		if seen[s] {
			t.Fatalf("duplicate secret %q", s)
		}
		seen[s] = true
		// 20 bytes is 160 bits, which is 32 base32 characters.
		if len(s) != 32 {
			t.Errorf("Secret(20) length = %d, want 32", len(s))
		}
	}
}
