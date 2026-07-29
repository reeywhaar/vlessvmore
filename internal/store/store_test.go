package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, name string) *User {
	t.Helper()
	u, err := s.Users.Create(CreateParams{Name: name}, now)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return u
}

func TestOpenFreshDirectory(t *testing.T) {
	s := open(t)
	// A fresh directory generates an identity, which is what makes a deployment
	// zero-configuration.
	if !s.Identity.Generated() {
		t.Error("a fresh store should have generated an identity")
	}
	if _, err := s.Identity.PublicKey(); err != nil {
		t.Errorf("generated identity has no derivable public key: %v", err)
	}
	if got := len(s.Users.List()); got != 0 {
		t.Errorf("fresh store has %d users, want 0", got)
	}
	if got := len(s.Tokens.List()); got != 0 {
		t.Errorf("fresh store has %d tokens, want 0", got)
	}
}

func TestCreateUserDefaults(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	if !u.Enabled {
		t.Error("new user should be enabled")
	}
	if u.UUID == "" {
		t.Error("uuid should be generated when not supplied")
	}
	if u.QuotaBytes != 0 {
		t.Errorf("quota = %d, want 0 (unlimited)", u.QuotaBytes)
	}
	if !u.UsageResetAt.Equal(now) {
		t.Errorf("usage_reset_at = %s, want %s", u.UsageResetAt, now)
	}
	if u.ID == "" {
		t.Error("id should be generated")
	}
}

func TestCreateRejectsDuplicates(t *testing.T) {
	s := open(t)
	a := mustCreate(t, s, "alice")

	if _, err := s.Users.Create(CreateParams{Name: "alice"}, now); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name error = %v, want ErrConflict", err)
	}
	// Case-insensitive: "Alice" and "alice" would be indistinguishable to an
	// operator and Get resolves either.
	if _, err := s.Users.Create(CreateParams{Name: "ALICE"}, now); !errors.Is(err, ErrConflict) {
		t.Errorf("case-different duplicate name error = %v, want ErrConflict", err)
	}
	// Two users sharing a UUID would share one credential.
	if _, err := s.Users.Create(CreateParams{Name: "bob", UUID: a.UUID}, now); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate uuid error = %v, want ErrConflict", err)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	s := open(t)
	if _, err := s.Users.Create(CreateParams{Name: "  "}, now); err == nil {
		t.Error("blank name accepted")
	}
	if _, err := s.Users.Create(CreateParams{Name: "x", UUID: "not-a-uuid"}, now); err == nil {
		t.Error("malformed uuid accepted")
	}
	if _, err := s.Users.Create(CreateParams{Name: "y", QuotaBytes: -1}, now); err == nil {
		t.Error("negative quota accepted")
	}
}

func TestGetByIDAndName(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	for _, ref := range []string{u.ID, "alice", "ALICE"} {
		got, err := s.Users.Get(ref)
		if err != nil {
			t.Fatalf("Get(%q): %v", ref, err)
		}
		if got.ID != u.ID {
			t.Errorf("Get(%q) returned %s, want %s", ref, got.ID, u.ID)
		}
	}
	if _, err := s.Users.Get("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(nobody) = %v, want ErrNotFound", err)
	}
}

func TestUpdate(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")
	later := now.Add(time.Hour)

	name := "alice2"
	quota := int64(1 << 20)
	got, err := s.Users.Update(u.ID, Patch{Name: &name, QuotaBytes: &quota}, later)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "alice2" || got.QuotaBytes != 1<<20 {
		t.Errorf("update not applied: %+v", got)
	}
	// The id is stable across a rename, which is what keeps usage history attached.
	if got.ID != u.ID {
		t.Errorf("id changed on rename: %s -> %s", u.ID, got.ID)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %s, want %s", got.UpdatedAt, later)
	}
}

func TestUpdateExpiresAtSetAndClear(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	exp := now.Add(24 * time.Hour)
	set := &exp
	got, err := s.Users.Update(u.ID, Patch{ExpiresAt: &set}, now)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at = %v, want %s", got.ExpiresAt, exp)
	}

	// A pointer-to-pointer patch is what lets a caller distinguish "leave it alone"
	// from "clear it".
	var clear *time.Time
	got, err = s.Users.Update(u.ID, Patch{ExpiresAt: &clear}, now)
	if err != nil {
		t.Fatalf("Update (clear): %v", err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", got.ExpiresAt)
	}
}

func TestUpdateRenameConflict(t *testing.T) {
	s := open(t)
	mustCreate(t, s, "alice")
	b := mustCreate(t, s, "bob")

	name := "alice"
	if _, err := s.Users.Update(b.ID, Patch{Name: &name}, now); !errors.Is(err, ErrConflict) {
		t.Errorf("rename onto an existing name = %v, want ErrConflict", err)
	}
	// Renaming to its own current name must be allowed.
	same := "bob"
	if _, err := s.Users.Update(b.ID, Patch{Name: &same}, now); err != nil {
		t.Errorf("no-op rename failed: %v", err)
	}
}

func TestEnableClearsDisabledReason(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	off, reason := false, "quota"
	if _, err := s.Users.Update(u.ID, Patch{Enabled: &off, DisabledReason: &reason}, now); err != nil {
		t.Fatalf("disable: %v", err)
	}
	on := true
	got, err := s.Users.Update(u.ID, Patch{Enabled: &on}, now)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Leaving the reason behind would make a healthy user read as "disabled: quota".
	if got.DisabledReason != "" {
		t.Errorf("disabled_reason = %q after re-enabling, want empty", got.DisabledReason)
	}
}

func TestDeleteUserRemovesUsage(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mustCreate(t, s, "alice")

	if err := s.Usage.Add(ctx, now, []Delta{{UserID: u.ID, Up: 100, Down: 200}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	// Users and usage live in different backing stores, so nothing cascades for us.
	up, down, err := s.Usage.Total(ctx, u.ID)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if up != 0 || down != 0 {
		t.Errorf("usage survived user deletion: up=%d down=%d", up, down)
	}
	if _, err := s.Users.Get(u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	u, err := s1.Users.Create(CreateParams{Name: "alice", Note: "phone"}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := s1.Tokens.Create("web", now); err != nil {
		t.Fatalf("token Create: %v", err)
	}
	if err := s1.Usage.Add(ctx, now, []Delta{{UserID: u.ID, Up: 5, Down: 7}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.Users.Get("alice")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Note != "phone" || got.ID != u.ID {
		t.Errorf("user did not survive reopen: %+v", got)
	}
	if n := len(s2.Tokens.List()); n != 1 {
		t.Errorf("tokens after reopen = %d, want 1", n)
	}
	up, down, err := s2.Usage.Total(ctx, u.ID)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if up != 5 || down != 7 {
		t.Errorf("usage after reopen = %d/%d, want 5/7", up, down)
	}
}

// The files are meant to be readable and repairable by hand, so they must be
// pretty-printed and parseable by an ordinary JSON tool.
func TestUsersFileIsReadableJSON(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	mustCreate(t, s, "alice")

	b, err := os.ReadFile(filepath.Join(dir, UsersFile))
	if err != nil {
		t.Fatalf("read users.json: %v", err)
	}
	if !bytes.Contains(b, []byte("\n  ")) {
		t.Errorf("users.json is not indented:\n%s", b)
	}
	var doc usersDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("users.json is not valid JSON: %v", err)
	}
	if doc.Version != jsonVersion || len(doc.Users) != 1 {
		t.Errorf("unexpected document: %+v", doc)
	}

	info, err := os.Stat(filepath.Join(dir, UsersFile))
	if err != nil {
		t.Fatal(err)
	}
	// The file holds UUIDs, which are credentials.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("users.json mode = %o, want 600", perm)
	}
}

func TestOpenRejectsCorruptUsersFile(t *testing.T) {
	tests := map[string]string{
		"bad json":       `{`,
		"wrong version":  `{"version": 99, "users": []}`,
		"unknown field":  `{"version": 1, "users": [{"id":"u_1","name":"a","uuid":"11111111-2222-3333-4444-555555555555","enabled":true,"quota_byte":5,"usage_reset_at":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`,
		"duplicate uuid": `{"version": 1, "users": [{"id":"u_1","name":"a","uuid":"11111111-2222-3333-4444-555555555555","enabled":true,"usage_reset_at":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},{"id":"u_2","name":"b","uuid":"11111111-2222-3333-4444-555555555555","enabled":true,"usage_reset_at":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`,
		"empty name":     `{"version": 1, "users": [{"id":"u_1","name":"","uuid":"11111111-2222-3333-4444-555555555555","enabled":true,"usage_reset_at":"2026-01-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, UsersFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); err == nil {
				t.Error("Open accepted a corrupt users.json; damage must be reported, not tolerated")
			}
		})
	}
}

func TestActiveUsersFiltersDisabledExpiredAndOverQuota(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	ok := mustCreate(t, s, "ok")

	disabled, err := s.Users.Create(CreateParams{Name: "disabled", Disabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}

	past := now.Add(-time.Hour)
	expired, err := s.Users.Create(CreateParams{Name: "expired", ExpiresAt: &past}, now)
	if err != nil {
		t.Fatal(err)
	}

	future := now.Add(time.Hour)
	notYet, err := s.Users.Create(CreateParams{Name: "not-yet-expired", ExpiresAt: &future}, now)
	if err != nil {
		t.Fatal(err)
	}

	overQuota, err := s.Users.Create(CreateParams{Name: "over", QuotaBytes: 1000}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Usage.Add(ctx, now, []Delta{{UserID: overQuota.ID, Up: 600, Down: 500}}); err != nil {
		t.Fatal(err)
	}

	underQuota, err := s.Users.Create(CreateParams{Name: "under", QuotaBytes: 1000}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Usage.Add(ctx, now, []Delta{{UserID: underQuota.ID, Up: 100, Down: 100}}); err != nil {
		t.Fatal(err)
	}

	active, err := s.ActiveUsers(ctx, now)
	if err != nil {
		t.Fatalf("ActiveUsers: %v", err)
	}
	got := map[string]bool{}
	for _, u := range active {
		got[u.Name] = true
	}
	want := map[string]bool{"ok": true, "not-yet-expired": true, "under": true}
	for name := range want {
		if !got[name] {
			t.Errorf("%q should be active", name)
		}
	}
	for _, u := range []*User{disabled, expired, overQuota} {
		if got[u.Name] {
			t.Errorf("%q should not be active", u.Name)
		}
	}
	if len(active) != len(want) {
		t.Errorf("active = %d users %v, want %d", len(active), got, len(want))
	}
	_ = ok
	_ = notYet
}

func TestQuotaIsRelativeToResetWindow(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	u, err := s.Users.Create(CreateParams{Name: "alice", QuotaBytes: 1000}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Usage.Add(ctx, now, []Delta{{UserID: u.ID, Up: 900, Down: 200}}); err != nil {
		t.Fatal(err)
	}

	over, err := s.OverQuota(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if !over {
		t.Fatal("user should be over quota")
	}

	// Resetting the window must clear the trip without deleting history: the traffic
	// still happened, it is just outside the current quota window.
	later := now.Add(2 * time.Hour)
	reset, err := s.Users.ResetUsageWindow(u.ID, later)
	if err != nil {
		t.Fatalf("ResetUsageWindow: %v", err)
	}
	over, err = s.OverQuota(ctx, reset)
	if err != nil {
		t.Fatal(err)
	}
	if over {
		t.Error("user should be under quota after a window reset")
	}
	up, down, err := s.Usage.Total(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up != 900 || down != 200 {
		t.Errorf("history was destroyed by the reset: %d/%d", up, down)
	}
}

func TestResetUsageWindowReenablesQuotaDisabledUser(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	off, reason := false, "quota"
	if _, err := s.Users.Update(u.ID, Patch{Enabled: &off, DisabledReason: &reason}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Users.ResetUsageWindow(u.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.DisabledReason != "" {
		t.Errorf("reset-usage should re-enable a quota-disabled user, got %+v", got)
	}
}

func TestResetUsageWindowLeavesManuallyDisabledUserOff(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	off := false
	if _, err := s.Users.Update(u.ID, Patch{Enabled: &off}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Users.ResetUsageWindow(u.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Only a quota trip is undone by reset-usage; a deliberate disable stands.
	if got.Enabled {
		t.Error("reset-usage re-enabled a manually disabled user")
	}
}

func TestUsageAccumulatesWithinBucket(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mustCreate(t, s, "alice")

	// Two polls in the same hour must add, not overwrite: the collector drains
	// sing-box's counters each time, so every delta is new traffic.
	for range 3 {
		if err := s.Usage.Add(ctx, now.Add(10*time.Minute), []Delta{{UserID: u.ID, Up: 10, Down: 20}}); err != nil {
			t.Fatal(err)
		}
	}
	up, down, err := s.Usage.Total(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up != 30 || down != 60 {
		t.Errorf("totals = %d/%d, want 30/60", up, down)
	}
}

func TestUsageSeriesRollup(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mustCreate(t, s, "alice")

	day := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	for h := range 26 { // spills into a second day
		if err := s.Usage.Add(ctx, day.Add(time.Duration(h)*time.Hour), []Delta{{UserID: u.ID, Up: 1, Down: 2}}); err != nil {
			t.Fatal(err)
		}
	}

	hourly, err := s.Usage.Series(ctx, u.ID, day, day.Add(48*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("Series hourly: %v", err)
	}
	if len(hourly) != 26 {
		t.Errorf("hourly points = %d, want 26", len(hourly))
	}

	daily, err := s.Usage.Series(ctx, u.ID, day, day.Add(48*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatalf("Series daily: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("daily points = %d, want 2", len(daily))
	}
	if daily[0].Up != 24 || daily[0].Down != 48 {
		t.Errorf("day 1 = %d/%d, want 24/48", daily[0].Up, daily[0].Down)
	}
	if daily[1].Up != 2 || daily[1].Down != 4 {
		t.Errorf("day 2 = %d/%d, want 2/4", daily[1].Up, daily[1].Down)
	}
}

func TestUsagePrune(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mustCreate(t, s, "alice")

	old := now.Add(-100 * 24 * time.Hour)
	if err := s.Usage.Add(ctx, old, []Delta{{UserID: u.ID, Up: 1, Down: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Usage.Add(ctx, now, []Delta{{UserID: u.ID, Up: 2, Down: 2}}); err != nil {
		t.Fatal(err)
	}

	n, err := s.Usage.Prune(ctx, now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	up, _, err := s.Usage.Total(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up != 2 {
		t.Errorf("remaining up = %d, want 2", up)
	}
}

func TestUsageCompactShrinksTheDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	u := mustCreate(t, s, "alice")

	// Enough rows for the page count difference to be unambiguous. Loaded in one
	// transaction; 30k Add calls would dominate the test's runtime.
	rows := make([]Row, 0, 30_000)
	for i := range 30_000 {
		rows = append(rows, Row{
			UserID: u.ID,
			Bucket: Bucket(now.Add(-time.Duration(i+1) * time.Hour)),
			Up:     int64(i),
			Down:   int64(i) * 2,
		})
	}
	// One recent row, so the prune below leaves something behind to query for.
	rows = append(rows, Row{UserID: u.ID, Bucket: Bucket(now), Up: 7, Down: 9})
	if err := s.Usage.Import(ctx, rows); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if err := s.Usage.Compact(ctx); err != nil {
		t.Fatalf("Compact while full: %v", err)
	}
	full := dbSize(t, dir)
	if wal := fileSize(t, filepath.Join(dir, StatsFile+"-wal")); wal != 0 {
		t.Errorf("-wal is %d bytes after a compact, want 0", wal)
	}

	// Everything except the recent row.
	if _, err := s.Usage.Prune(ctx, now); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if err := s.Usage.Compact(ctx); err != nil {
		t.Fatalf("Compact after prune: %v", err)
	}
	pruned := dbSize(t, dir)
	if wal := fileSize(t, filepath.Join(dir, StatsFile+"-wal")); wal != 0 {
		t.Errorf("-wal is %d bytes after a compact, want 0", wal)
	}
	// Pruning alone leaves the file at its old size, so a Compact that does not shrink
	// it is doing nothing.
	if pruned >= full {
		t.Errorf("%s is %d bytes after pruning 30k rows and compacting, was %d before — no space was reclaimed",
			StatsFile, pruned, full)
	}

	// VACUUM rebuilds the database, so check it still answers.
	up, down, err := s.Usage.Total(ctx, u.ID)
	if err != nil {
		t.Fatalf("Total after compact: %v", err)
	}
	if up != 7 || down != 9 {
		t.Errorf("remaining total = %d/%d, want 7/9", up, down)
	}
}

func TestUsageCompactIsRepeatable(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	// Nothing to reclaim, twice over: the caller runs this on a timer and cannot know
	// whether the last run left anything to do.
	for i := range 2 {
		if err := s.Usage.Compact(ctx); err != nil {
			t.Fatalf("Compact %d on an empty database: %v", i+1, err)
		}
	}
}

func dbSize(t *testing.T, dir string) int64 {
	t.Helper()
	return fileSize(t, filepath.Join(dir, StatsFile))
}

// fileSize reports 0 for a missing file, so a caller can ask about the -wal without
// caring whether SQLite truncated it or removed it.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func TestTokenLifecycle(t *testing.T) {
	s := open(t)

	tok, secret, err := s.Tokens.Create("web", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if secret == "" {
		t.Fatal("secret must be returned at creation")
	}
	// Only the hash is stored, so a leaked tokens.json cannot be replayed.
	if tok.Hash == secret {
		t.Error("the secret itself was stored")
	}
	if tok.Hash != HashSecret(secret) {
		t.Error("stored hash does not match the secret")
	}

	got, err := s.Tokens.Lookup(secret, now)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("Lookup returned %s, want %s", got.ID, tok.ID)
	}
	if got.LastUsedAt == nil {
		t.Error("Lookup should record last_used_at")
	}

	if _, err := s.Tokens.Lookup("wrong", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup(wrong) = %v, want ErrNotFound", err)
	}

	if _, err := s.Tokens.Revoke(tok.ID, now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// A revoked token must be indistinguishable from an unknown one.
	if _, err := s.Tokens.Lookup(secret, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token still authenticates: %v", err)
	}
}

func TestTokenSecretsAreUnique(t *testing.T) {
	s := open(t)
	seen := map[string]bool{}
	for i := range 10 {
		_, secret, err := s.Tokens.Create("label", now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if seen[secret] {
			t.Fatal("duplicate secret")
		}
		seen[secret] = true
	}
}

func TestTokenRequiresLabel(t *testing.T) {
	s := open(t)
	if _, _, err := s.Tokens.Create("  ", now); err == nil {
		t.Error("blank label accepted")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := open(t)

	a := mustCreate(t, src, "alice")
	b, err := src.Users.Create(CreateParams{Name: "bob", QuotaBytes: 500, Note: "laptop"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Tokens.Create("web", now); err != nil {
		t.Fatal(err)
	}
	if err := src.Usage.Add(ctx, now, []Delta{{UserID: a.ID, Up: 11, Down: 22}, {UserID: b.ID, Up: 33, Down: 44}}); err != nil {
		t.Fatal(err)
	}

	dump, err := src.Export(ctx, now, ExportOptions{IncludeUsage: true, IncludeTokens: true})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var buf bytes.Buffer
	if err := dump.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	read, err := ReadDump(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadDump: %v", err)
	}

	dst := open(t)
	if err := dst.Import(ctx, read, false); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if got := len(dst.Users.List()); got != 2 {
		t.Errorf("imported %d users, want 2", got)
	}
	gotB, err := dst.Users.Get("bob")
	if err != nil {
		t.Fatalf("Get(bob): %v", err)
	}
	if gotB.ID != b.ID || gotB.QuotaBytes != 500 || gotB.Note != "laptop" {
		t.Errorf("bob did not survive the round trip: %+v", gotB)
	}
	if got := len(dst.Tokens.List()); got != 1 {
		t.Errorf("imported %d tokens, want 1", got)
	}
	up, down, err := dst.Usage.Total(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up != 11 || down != 22 {
		t.Errorf("alice usage = %d/%d, want 11/22", up, down)
	}
}

func TestExportOmitsSectionsByDefault(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u := mustCreate(t, s, "alice")
	if _, _, err := s.Tokens.Create("web", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Usage.Add(ctx, now, []Delta{{UserID: u.ID, Up: 1, Down: 1}}); err != nil {
		t.Fatal(err)
	}

	dump, err := s.Export(ctx, now, ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Tokens belong to the old host and history is bulk; a plain export moves accounts.
	if dump.Tokens != nil {
		t.Errorf("tokens included by default: %+v", dump.Tokens)
	}
	if dump.Usage != nil {
		t.Errorf("usage included by default: %+v", dump.Usage)
	}
	if len(dump.Users) != 1 {
		t.Errorf("users = %d, want 1", len(dump.Users))
	}
}

func TestImportRefusesNonEmptyTargetWithoutForce(t *testing.T) {
	ctx := context.Background()
	src := open(t)
	mustCreate(t, src, "alice")
	dump, err := src.Export(ctx, now, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dst := open(t)
	mustCreate(t, dst, "existing")

	err = dst.Import(ctx, dump, false)
	if !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Import into a populated store = %v, want ErrNotEmpty", err)
	}
	// The refusal must not have partially applied anything.
	if _, err := dst.Users.Get("existing"); err != nil {
		t.Errorf("existing user was lost by a refused import: %v", err)
	}

	if err := dst.Import(ctx, dump, true); err != nil {
		t.Fatalf("forced Import: %v", err)
	}
	if _, err := dst.Users.Get("existing"); !errors.Is(err, ErrNotFound) {
		t.Error("forced import should have replaced the existing users")
	}
}

func TestReadDumpRejectsOrphanUsage(t *testing.T) {
	const in = `{
  "version": 1,
  "exported_at": "2026-07-26T12:00:00Z",
  "users": [],
  "usage": [{"user_id": "u_GONE", "bucket": 0, "up": 1, "down": 2}]
}`
	if _, err := ReadDump(bytes.NewReader([]byte(in))); err == nil {
		t.Error("ReadDump accepted usage referencing an unknown user")
	}
}

func TestReadDumpRejectsWrongVersion(t *testing.T) {
	const in = `{"version": 99, "exported_at": "2026-07-26T12:00:00Z", "users": []}`
	if _, err := ReadDump(bytes.NewReader([]byte(in))); err == nil {
		t.Error("ReadDump accepted an unknown version")
	}
}

func TestBucketTruncatesToHour(t *testing.T) {
	at := time.Date(2026, 7, 26, 12, 59, 59, 999, time.UTC)
	want := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC).Unix()
	if got := Bucket(at); got != want {
		t.Errorf("Bucket = %d, want %d", got, want)
	}
}

// newTestLogger writes text-format logs into buf for assertions.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestCreateGeneratesSubToken(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")

	if u.SubToken == "" {
		t.Fatal("no subscription token generated")
	}
	// The URL is unauthenticated, so the token is the only thing protecting the
	// credential. 20 bytes of base32 is 32 characters.
	if len(u.SubToken) != 32 {
		t.Errorf("sub token length = %d, want 32", len(u.SubToken))
	}
	// It must not be derived from anything guessable.
	if strings.Contains(u.SubToken, u.ID) || strings.Contains(u.SubToken, u.UUID) {
		t.Error("sub token is derived from the id or uuid")
	}
}

func TestSubTokensAreUnique(t *testing.T) {
	s := open(t)
	seen := map[string]bool{}
	for i := range 20 {
		u, err := s.Users.Create(CreateParams{Name: fmt.Sprintf("user%d", i)}, now)
		if err != nil {
			t.Fatal(err)
		}
		if seen[u.SubToken] {
			t.Fatalf("duplicate sub token at user %d", i)
		}
		seen[u.SubToken] = true
	}
}

func TestGetBySubToken(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")
	mustCreate(t, s, "bob")

	got, err := s.Users.GetBySubToken(u.SubToken)
	if err != nil {
		t.Fatalf("GetBySubToken: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("resolved to %s, want %s", got.ID, u.ID)
	}

	for _, bad := range []string{"", "nope", u.SubToken[:len(u.SubToken)-1], u.SubToken + "X"} {
		if _, err := s.Users.GetBySubToken(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetBySubToken(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestRotateSubToken(t *testing.T) {
	s := open(t)
	u := mustCreate(t, s, "alice")
	old := u.SubToken

	rotated, err := s.Users.RotateSubToken(u.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("RotateSubToken: %v", err)
	}
	if rotated.SubToken == old {
		t.Fatal("token did not change")
	}
	// The old URL must stop working...
	if _, err := s.Users.GetBySubToken(old); !errors.Is(err, ErrNotFound) {
		t.Error("the old subscription token still resolves")
	}
	// ...while the credential is untouched, so a configured client keeps connecting.
	if rotated.UUID != u.UUID {
		t.Error("rotating the subscription token changed the uuid")
	}
	if !rotated.Enabled {
		t.Error("rotating the subscription token disabled the user")
	}
}

// A users.json written before subscription tokens existed, or edited by hand, must come
// up with working URLs rather than silently broken ones.
func TestSubTokenBackfilledOnLoad(t *testing.T) {
	dir := t.TempDir()
	const legacy = `{
  "version": 1,
  "users": [
    {
      "id": "u_LEGACY0000000000000000000",
      "name": "alice",
      "uuid": "11111111-2222-3333-4444-555555555555",
      "enabled": true,
      "quota_bytes": 0,
      "usage_reset_at": "2026-01-01T00:00:00Z",
      "sub_token": "",
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z"
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, UsersFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	u, err := s.Users.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.SubToken == "" {
		t.Fatal("sub token was not backfilled")
	}
	// And the backfill is persisted, so it is stable across restarts.
	s.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got, err := again.Users.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.SubToken != u.SubToken {
		t.Errorf("backfilled token changed across reopen: %q vs %q", got.SubToken, u.SubToken)
	}
}

func TestOpenRejectsDuplicateSubTokens(t *testing.T) {
	dir := t.TempDir()
	const dup = `{
  "version": 1,
  "users": [
    {"id":"u_1","name":"a","uuid":"11111111-2222-3333-4444-555555555555","enabled":true,
     "usage_reset_at":"2026-01-01T00:00:00Z","sub_token":"SAME","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},
    {"id":"u_2","name":"b","uuid":"66666666-7777-8888-9999-000000000000","enabled":true,
     "usage_reset_at":"2026-01-01T00:00:00Z","sub_token":"SAME","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, UsersFile), []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two users sharing a subscription URL means one of them serves the other's
	// credential; that has to be an error, not a coin flip.
	if _, err := Open(dir); err == nil {
		t.Error("Open accepted duplicate sub tokens")
	}
}

// Every slice that reaches a JSON response has to be non-nil when empty, or it encodes as
// `null` and every client has to guard for it. The empty case is not exotic: a fresh node
// has no tokens and no users, and every user has no traffic until they send some.
//
// Asserting on the returned slice rather than on marshalled bytes is deliberate — this is
// the invariant at its source, and the API package has its own test on the wire format.
func TestEmptyCollectionsAreNotNil(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if got := s.Users.List(); got == nil {
		t.Error("Users.List() is nil on a fresh store; it marshals as null")
	}
	if got := s.Tokens.List(); got == nil {
		t.Error("Tokens.List() is nil on a fresh store; it marshals as null")
	}

	u, err := s.Users.Create(CreateParams{Name: "alice"}, time.Now())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now()
	series, err := s.Usage.Series(ctx, u.ID, now.Add(-24*time.Hour), now, time.Hour)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if series == nil {
		t.Error("Usage.Series() is nil for a user with no traffic; it marshals as null")
	}

	rows, err := s.Usage.Export(ctx)
	if err != nil {
		t.Fatalf("export usage: %v", err)
	}
	if rows == nil {
		t.Error("Usage.Export() is nil with no traffic recorded; it marshals as null")
	}

	// Non-empty must keep working too — a fix that always returns an empty slice would
	// pass everything above.
	if got := s.Users.List(); len(got) != 1 {
		t.Errorf("Users.List() returned %d users, want 1", len(got))
	}
}
