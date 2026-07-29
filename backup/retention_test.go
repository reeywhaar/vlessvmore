package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func name(t time.Time) string {
	return "vlessvmore-" + t.UTC().Format("20060102_150405") + ".tgz"
}

// hourly returns n names at one-hour intervals ending at testNow, newest first.
func hourly(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, name(testNow.Add(-time.Duration(i)*time.Hour)))
	}
	return out
}

func TestSelectBackupsToKeep(t *testing.T) {
	// A spread with something in every slot: today, yesterday, two days back, and one
	// each at 10 and 40 days.
	spread := []string{
		name(testNow),
		name(testNow.Add(-2 * time.Hour)),
		name(testNow.AddDate(0, 0, -1)),
		name(testNow.AddDate(0, 0, -2)),
		name(testNow.AddDate(0, 0, -10)),
		name(testNow.AddDate(0, 0, -40)),
	}

	tests := []struct {
		name  string
		names []string
		want  []string
	}{
		{
			"nothing",
			nil,
			nil,
		},
		{
			// The only archive there is fills every slot, so it survives. Deleting the
			// single backup a deployment owns is the one outcome that must be impossible.
			"one archive",
			[]string{name(testNow)},
			[]string{name(testNow)},
		},
		{
			// 72 hourly archives ending at noon span four calendar days. One keeper per
			// day for the newest three — the last hour of each, so 23:00 for the days
			// that are over — plus the weekly and monthly slots falling back to the
			// oldest, which is the fourth day's only survivor.
			"hourly for three days",
			hourly(72),
			[]string{
				name(testNow),
				name(testNow.AddDate(0, 0, -1).Add(11 * time.Hour)),
				name(testNow.AddDate(0, 0, -2).Add(11 * time.Hour)),
				name(testNow.Add(-71 * time.Hour)),
			},
		},
		{
			"a full spread",
			spread,
			[]string{
				name(testNow),
				name(testNow.AddDate(0, 0, -1)),
				name(testNow.AddDate(0, 0, -2)),
				name(testNow.AddDate(0, 0, -10)),
				name(testNow.AddDate(0, 0, -40)),
			},
		},
		{
			// Whichever of the two the caller wrote, retention must see both, or the
			// other format accumulates unbounded.
			"tgz and zip together",
			[]string{
				"vlessvmore-20260729_120000.zip",
				"vlessvmore-20260728_120000.tgz",
				"vlessvmore-20260727_120000.zip",
				"vlessvmore-20260726_120000.tgz",
			},
			[]string{
				"vlessvmore-20260729_120000.zip",
				"vlessvmore-20260728_120000.tgz",
				"vlessvmore-20260727_120000.zip",
				// Oldest, standing in for both the weekly and monthly slots.
				"vlessvmore-20260726_120000.tgz",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectBackupsToKeep(parseEntries(tt.names), testNow)
			want := map[string]bool{}
			for _, n := range tt.want {
				want[n] = true
			}
			if joinKeys(got) != joinKeys(want) {
				t.Errorf("keep = %s\nwant  = %s", joinKeys(got), joinKeys(want))
			}
		})
	}
}

// The policy caps the archive count regardless of how long the loop has been running,
// which is what stops an hourly backup from filling a bucket.
func TestSelectBackupsToKeepIsBounded(t *testing.T) {
	// A year of hourly archives.
	var names []string
	for i := range 24 * 365 {
		names = append(names, name(testNow.Add(-time.Duration(i)*time.Hour)))
	}
	keep := selectBackupsToKeep(parseEntries(names), testNow)
	if len(keep) > dailySlots+2 {
		t.Errorf("kept %d archives, want at most %d", len(keep), dailySlots+2)
	}
	if !keep[name(testNow)] {
		t.Error("the newest archive was not kept")
	}
}

func TestToRemoveLeavesForeignNamesAlone(t *testing.T) {
	names := append(hourly(48),
		"notes.txt",
		"vaultwarden-20260729_120000.zip",
		"vlessvmore-latest.tgz",
		"vlessvmore-20260729_120000.tar",
	)
	for _, got := range toRemove(names, testNow) {
		if !strings.HasPrefix(got, "vlessvmore-") || !strings.Contains(got, "_") {
			t.Errorf("toRemove wants to delete %q, which is not one of ours", got)
		}
	}
	// 48 hourly archives ending at noon span three calendar days: three daily keepers,
	// plus the oldest standing in for both the weekly and monthly slots. 48 - 4 = 44.
	if n := len(toRemove(names, testNow)); n != 44 {
		t.Errorf("toRemove returned %d names, want 44", n)
	}
}

func TestParseEntriesSortsNewestFirst(t *testing.T) {
	got := parseEntries([]string{
		"vlessvmore-20260701_000000.tgz",
		"vlessvmore-20260729_120000.tgz",
		"vlessvmore-20260715_060000.zip",
		"garbage",
	})
	if len(got) != 3 {
		t.Fatalf("parsed %d entries, want 3 (garbage dropped)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].date.After(got[i].date) {
			t.Errorf("entry %d (%s) is not newer than %d (%s)",
				i-1, got[i-1].name, i, got[i].name)
		}
	}
}

func TestParseDateFromName(t *testing.T) {
	got, ok := parseDateFromName("vlessvmore-20260729_031500.tgz")
	if !ok {
		t.Fatal("a well-formed name did not parse")
	}
	if want := time.Date(2026, 7, 29, 3, 15, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("parsed %s, want %s", got, want)
	}
	for _, bad := range []string{
		"vlessvmore-2026729_031500.tgz",
		"vlessvmore-20260729-031500.tgz",
		"vlessvmore-20260729_031500.tar.gz",
		"prefix-vlessvmore-20260729_031500.tgz",
		fmt.Sprintf("vlessvmore-%s.tgz", "20260729_0315"),
	} {
		if _, ok := parseDateFromName(bad); ok {
			t.Errorf("%q parsed but should not have", bad)
		}
	}
}
