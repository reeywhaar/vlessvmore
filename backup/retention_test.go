package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// A Wednesday, so the three newest days sit inside one ISO week and the day before them
// falls into the previous one.
var testNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func name(t time.Time) string {
	return "vlessvmore-" + t.UTC().Format("20060102_150405") + ".tgz"
}

// maxKept is the ceiling the slots imply. The newest bucket of each kind holds the newest
// archive, which the day slots already keep, so every slot past the first of its kind adds
// one archive.
const maxKept = currentDaySlots + (daySlots - 1) + (weekSlots - 1) + (monthSlots - 1)

// hourly returns n names at one-hour intervals ending at testNow, newest first.
func hourly(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, name(testNow.Add(-time.Duration(i)*time.Hour)))
	}
	return out
}

func TestSelectBackupsToKeep(t *testing.T) {
	// A spread with something in every slot: two archives today, one each on the two days
	// before, one last week, one last month.
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
			// 72 hourly archives ending at noon span four calendar days. The three newest
			// from today, the last hour of the two days before it, and 07-26 for the
			// previous ISO week. The month slot adds nothing — it is all one month.
			"hourly for three days",
			hourly(72),
			[]string{
				name(testNow),
				name(testNow.Add(-1 * time.Hour)),
				name(testNow.Add(-2 * time.Hour)),
				name(testNow.AddDate(0, 0, -1).Add(11 * time.Hour)),
				name(testNow.AddDate(0, 0, -2).Add(11 * time.Hour)),
				name(testNow.AddDate(0, 0, -3).Add(11 * time.Hour)),
			},
		},
		{
			// Every archive is the only occupant of its slot, so nothing is dropped.
			"a full spread",
			spread,
			spread,
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
				// Previous ISO week: 07-27 is a Monday.
				"vlessvmore-20260726_120000.tgz",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectBackupsToKeep(parseEntries(tt.names))
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
	keep := selectBackupsToKeep(parseEntries(names))
	if len(keep) > maxKept {
		t.Errorf("kept %d archives, want at most %d: %s", len(keep), maxKept, joinKeys(keep))
	}
	if !keep[name(testNow)] {
		t.Error("the newest archive was not kept")
	}
}

// Every run prunes what the last run left behind, so a slot no surviving archive can reach
// is a slot that never fills. Only running the loop shows that.
func TestRetentionSurvivesRepeatedPruning(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const hours = 24 * 60

	pool := map[string]bool{}
	for h := range hours {
		pool[name(start.Add(time.Duration(h)*time.Hour))] = true
		for _, gone := range toRemove(sortedKeys(pool)) {
			delete(pool, gone)
		}
		if len(pool) > maxKept {
			t.Fatalf("hour %d: pool grew to %d archives", h, len(pool))
		}
	}

	// Two months of hourly backups later: three archives from the last day, the last hour
	// of the two days before it, the last hour of the previous ISO week (07-26, a Sunday),
	// and the last hour of June for the month slot.
	last := start.Add((hours - 1) * time.Hour)
	want := map[string]bool{}
	for _, n := range []string{
		name(last),
		name(last.Add(-1 * time.Hour)),
		name(last.Add(-2 * time.Hour)),
		name(last.AddDate(0, 0, -1)),
		name(last.AddDate(0, 0, -2)),
		name(time.Date(2026, 7, 26, 23, 0, 0, 0, time.UTC)),
		name(time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC)),
	} {
		want[n] = true
	}
	if joinKeys(pool) != joinKeys(want) {
		t.Errorf("pool = %s\nwant  = %s", joinKeys(pool), joinKeys(want))
	}

	// The same pool pruned twice must lose nothing, or the slots are chasing the clock
	// instead of the archives.
	if again := toRemove(sortedKeys(pool)); len(again) != 0 {
		t.Errorf("a second prune of the same pool removed %v", again)
	}
}

func TestToRemoveLeavesForeignNamesAlone(t *testing.T) {
	names := append(hourly(48),
		"notes.txt",
		"vaultwarden-20260729_120000.zip",
		"vlessvmore-latest.tgz",
		"vlessvmore-20260729_120000.tar",
	)
	for _, got := range toRemove(names) {
		if !strings.HasPrefix(got, "vlessvmore-") || !strings.Contains(got, "_") {
			t.Errorf("toRemove wants to delete %q, which is not one of ours", got)
		}
	}
	// 48 hourly archives ending at noon span three calendar days, all in one ISO week and
	// one month: three keepers from today plus the last hour of the two days before it.
	// 48 - 5 = 43.
	if n := len(toRemove(names)); n != 43 {
		t.Errorf("toRemove returned %d names, want 43", n)
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
