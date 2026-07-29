package main

import (
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

// dailySlots is how many calendar days keep an archive.
const dailySlots = 3

// selectBackupsToKeep applies the retention policy: one archive for each of the last
// three days that has one, plus one at least a week old and one at least a month old.
//
// Expects backups sorted newest first. Days rather than "the three newest files", because
// at an hourly cadence the three newest files cover three hours.
//
// Each slot falls back to the oldest archive when nothing qualifies, so a young
// deployment converges on keeping everything rather than deleting its only history.
func selectBackupsToKeep(backups []backupEntry, now time.Time) map[string]bool {
	keep := map[string]bool{}
	if len(backups) == 0 {
		return keep
	}

	// Newest first, so the first archive seen on a day is that day's keeper.
	days := 0
	seenDay := ""
	for _, b := range backups {
		day := b.date.Format("2006-01-02")
		if day == seenDay {
			continue
		}
		seenDay = day
		keep[b.name] = true
		if days++; days == dailySlots {
			break
		}
	}

	oldest := backups[len(backups)-1].name
	keep[firstOlderThan(backups, now, 7*24*time.Hour, oldest)] = true
	keep[firstOlderThan(backups, now, 30*24*time.Hour, oldest)] = true
	return keep
}

// firstOlderThan returns the newest archive at least age old, or fallback if there is none.
func firstOlderThan(backups []backupEntry, now time.Time, age time.Duration, fallback string) string {
	for _, b := range backups {
		if now.Sub(b.date) >= age {
			return b.name
		}
	}
	return fallback
}

// toRemove is the set difference the callers actually want: everything recognisable that
// no slot claimed. Unrecognisable names are left alone — they are not ours to delete.
func toRemove(names []string, now time.Time) []string {
	keep := selectBackupsToKeep(parseEntries(names), now)
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
