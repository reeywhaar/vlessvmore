package link

import (
	"strings"
	"testing"
)

func TestEncodeShape(t *testing.T) {
	code, err := Encode(Build(base()))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if code.Size <= 0 {
		t.Fatalf("Size = %d", code.Size)
	}
	// A QR symbol is always square, and version N is 17+4N modules, so the size is
	// always 21, 25, 29, … A size outside that family means the matrix is malformed.
	if (code.Size-21)%4 != 0 {
		t.Errorf("Size = %d is not a valid QR version size", code.Size)
	}
	if len(code.Rows) != code.Size {
		t.Errorf("rows = %d, want %d", len(code.Rows), code.Size)
	}
	for i, row := range code.Rows {
		if len(row) != code.Size {
			t.Fatalf("row %d has length %d, want %d", i, len(row), code.Size)
		}
		if strings.Trim(row, "01") != "" {
			t.Fatalf("row %d contains something other than 0 and 1: %q", i, row)
		}
	}
	if code.QuietZone != RecommendedQuietZone {
		t.Errorf("QuietZone = %d, want %d", code.QuietZone, RecommendedQuietZone)
	}
}

// The three finder patterns are 7x7 solid squares in the top-left, top-right and
// bottom-left corners. If those are wrong the matrix is not a QR code at all, whatever
// else it looks like.
func TestEncodeHasFinderPatterns(t *testing.T) {
	code, err := Encode("vless://test")
	if err != nil {
		t.Fatal(err)
	}
	n := code.Size

	corners := map[string][2]int{
		"top-left":    {0, 0},
		"top-right":   {n - 7, 0},
		"bottom-left": {0, n - 7},
	}
	for name, origin := range corners {
		ox, oy := origin[0], origin[1]
		// Outer ring dark.
		for i := range 7 {
			if code.Rows[oy][ox+i] != '1' || code.Rows[oy+6][ox+i] != '1' {
				t.Errorf("%s finder: outer ring not dark at column %d", name, i)
				break
			}
		}
		// Inner 3x3 dark, and the ring around it light.
		if code.Rows[oy+1][ox+1] != '0' {
			t.Errorf("%s finder: separator ring not light", name)
		}
		if code.Rows[oy+3][ox+3] != '1' {
			t.Errorf("%s finder: centre not dark", name)
		}
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	uri := Build(base())
	first, err := Encode(uri)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := Encode(uri)
		if err != nil {
			t.Fatal(err)
		}
		if again.Size != first.Size {
			t.Fatalf("size changed between encodings: %d vs %d", again.Size, first.Size)
		}
		for i := range first.Rows {
			if again.Rows[i] != first.Rows[i] {
				t.Fatalf("row %d differs between encodings of identical input", i)
			}
		}
	}
}

func TestEncodeLongLinkStillFits(t *testing.T) {
	// A realistic worst case: long hostname and a long display name.
	p := base()
	p.Host = "very-long-subdomain.of-some-longer-domain.example.test"
	p.SNI = p.Host
	p.Name = strings.Repeat("long-display-name-", 5)

	code, err := Encode(Build(p))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Anything past version 25 (117 modules) will not fit in an ordinary terminal or
	// scan reliably off a screen; flag it rather than emitting something unusable.
	if code.Size > 117 {
		t.Errorf("Size = %d, too large to be scannable in practice", code.Size)
	}
}
