package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Archive names this tool produces and recognises: vlessvmore-20260729_031500.tgz, or
// .zip when BACKUP_PASSWORD turned it into an encrypted archive.
//
// Both extensions match on purpose. Toggling the password would otherwise make every
// archive taken under the old setting invisible to retention, and they would pile up
// forever with nothing to remove them.
var nameRe = regexp.MustCompile(`^vlessvmore-\d{8}_\d{6}\.(tgz|zip)$`)

var dateRe = regexp.MustCompile(`^vlessvmore-(\d{4})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})\.(?:tgz|zip)$`)

type backupEntry struct {
	name string
	date time.Time
}

// parseEntries drops names it cannot date and sorts the rest newest first.
func parseEntries(names []string) []backupEntry {
	var out []backupEntry
	for _, name := range names {
		if date, ok := parseDateFromName(name); ok {
			out = append(out, backupEntry{name: name, date: date})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].date.After(out[j].date) })
	return out
}

func parseDateFromName(name string) (time.Time, bool) {
	m := dateRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	n := make([]int, 6)
	for i := range n {
		for _, c := range m[i+1] {
			n[i] = n[i]*10 + int(c-'0')
		}
	}
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.UTC), true
}

// Retention slots. Every slot is defined against the archives that exist rather than
// against the wall clock, because each run prunes what the last run left behind: a
// bucket's keeper is the newest archive in it, and no later archive can land in an
// earlier bucket, so the keeper is settled the moment its bucket ends.
//
// Age windows cannot do that. "The newest archive at least a week old" deletes every
// archive that is four days old — too new for the window, too old for the day slots —
// so nothing survives to reach a week and the slot only ever holds whatever the first
// run happened to pin.
const (
	// currentDaySlots is how many of the newest archives from the newest day are kept.
	// Everything else that day is pruned, however short the interval.
	currentDaySlots = 3
	// daySlots is how many calendar days keep their newest archive.
	daySlots = 3
	// weekSlots and monthSlots count calendar buckets, and the newest bucket's keeper is
	// the newest archive overall, already held by the day slots. Two buckets each is
	// therefore one week-old copy and one month-old copy.
	weekSlots  = 2
	monthSlots = 2
)

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func weekKey(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func monthKey(t time.Time) string { return t.Format("2006-01") }

// selectBackupsToKeep applies the retention policy: the three newest archives from the
// newest day, the newest archive of each of the three newest days, and the newest archive
// of the second-newest week and month. Seven archives at most, whatever the interval.
//
// Expects backups sorted newest first.
func selectBackupsToKeep(backups []backupEntry) map[string]bool {
	keep := map[string]bool{}
	if len(backups) == 0 {
		return keep
	}

	// The newest archive's day rather than today's date, so a loop that stopped a week ago
	// keeps the last day it managed instead of leaving the slot empty.
	day := dayKey(backups[0].date)
	for i, b := range backups {
		if i == currentDaySlots || dayKey(b.date) != day {
			break
		}
		keep[b.name] = true
	}

	for _, slot := range []struct {
		n   int
		key func(time.Time) string
	}{
		{daySlots, dayKey},
		{weekSlots, weekKey},
		{monthSlots, monthKey},
	} {
		for _, name := range bucketKeepers(backups, slot.n, slot.key) {
			keep[name] = true
		}
	}
	return keep
}

// bucketKeepers returns the newest archive from each of the n newest buckets, where key
// puts an archive in a bucket. Expects backups sorted newest first, so the first archive
// seen in a bucket is that bucket's keeper and a bucket's archives are contiguous.
func bucketKeepers(backups []backupEntry, n int, key func(time.Time) string) []string {
	var out []string
	seen := ""
	for _, b := range backups {
		bucket := key(b.date)
		if bucket == seen {
			continue
		}
		seen = bucket
		out = append(out, b.name)
		if len(out) == n {
			break
		}
	}
	return out
}

// toRemove is the set difference the callers actually want: everything recognisable that
// no slot claimed. Unrecognisable names are left alone — they are not ours to delete.
func toRemove(names []string) []string {
	keep := selectBackupsToKeep(parseEntries(names))
	var out []string
	for _, name := range names {
		if nameRe.MatchString(name) && !keep[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
