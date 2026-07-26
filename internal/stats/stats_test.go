package stats

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"vlessvmore/internal/store"
)

func TestParseName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Stat
		ok   bool
	}{
		{
			"user uplink",
			"user>>>u_01ABCDEF>>>traffic>>>uplink",
			Stat{Kind: KindUser, ID: "u_01ABCDEF", Direction: DirectionUp},
			true,
		},
		{
			"user downlink",
			"user>>>u_01ABCDEF>>>traffic>>>downlink",
			Stat{Kind: KindUser, ID: "u_01ABCDEF", Direction: DirectionDown},
			true,
		},
		{
			"inbound",
			"inbound>>>vless-in>>>traffic>>>uplink",
			Stat{Kind: KindInbound, ID: "vless-in", Direction: DirectionUp},
			true,
		},
		{
			"outbound",
			"outbound>>>direct>>>traffic>>>downlink",
			Stat{Kind: KindOutbound, ID: "direct", Direction: DirectionDown},
			true,
		},
		{"empty", "", Stat{}, false},
		{"too few parts", "user>>>alice>>>traffic", Stat{}, false},
		{"too many parts", "user>>>alice>>>traffic>>>uplink>>>extra", Stat{}, false},
		{"not traffic", "user>>>alice>>>other>>>uplink", Stat{}, false},
		{"unknown kind", "session>>>alice>>>traffic>>>uplink", Stat{}, false},
		{"unknown direction", "user>>>alice>>>traffic>>>sideways", Stat{}, false},
		{"empty id", "user>>>>>>traffic>>>uplink", Stat{}, false},
		{"wrong separator", "user>>alice>>traffic>>uplink", Stat{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseName(tt.in)
			if ok != tt.ok {
				t.Fatalf("ParseName(%q) ok = %v, want %v", tt.in, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("ParseName(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func testCollector(t *testing.T) (*Collector, *store.Store, *fakeReloader) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := &fakeReloader{}
	c := &Collector{
		store:     st,
		reload:    r,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		interval:  time.Second,
		retention: DefaultRetention,
	}
	return c, st, r
}

type fakeReloader struct{ calls int }

func (f *fakeReloader) Reload(context.Context) error { f.calls++; return nil }

func TestEnforceDisablesOverQuota(t *testing.T) {
	ctx := context.Background()
	c, st, r := testCollector(t)
	now := time.Now()

	u, err := st.Users.Create(store.CreateParams{Name: "alice", QuotaBytes: 1000}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Usage.Add(ctx, now, []store.Delta{{UserID: u.ID, Up: 700, Down: 400}}); err != nil {
		t.Fatal(err)
	}

	changed, err := c.Enforce(ctx)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	got, err := st.Users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Error("user over quota is still enabled")
	}
	if got.DisabledReason != ReasonQuota {
		t.Errorf("disabled_reason = %q, want %q", got.DisabledReason, ReasonQuota)
	}

	// Idempotent: a second pass must not report changes or reload again.
	changed, err = c.Enforce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("second Enforce changed = %d, want 0", changed)
	}
	_ = r
}

func TestEnforceDisablesExpired(t *testing.T) {
	ctx := context.Background()
	c, st, _ := testCollector(t)
	now := time.Now()

	past := now.Add(-time.Hour)
	u, err := st.Users.Create(store.CreateParams{Name: "alice", ExpiresAt: &past}, now)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := c.Enforce(ctx)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	got, err := st.Users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.DisabledReason != ReasonExpired {
		t.Errorf("expected expired+disabled, got %+v", got)
	}
}

func TestEnforceLeavesHealthyUsersAlone(t *testing.T) {
	ctx := context.Background()
	c, st, _ := testCollector(t)
	now := time.Now()

	future := now.Add(time.Hour)
	if _, err := st.Users.Create(store.CreateParams{Name: "unlimited"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Users.Create(store.CreateParams{Name: "not-expired", ExpiresAt: &future}, now); err != nil {
		t.Fatal(err)
	}
	under, err := st.Users.Create(store.CreateParams{Name: "under-quota", QuotaBytes: 1000}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Usage.Add(ctx, now, []store.Delta{{UserID: under.ID, Up: 1, Down: 1}}); err != nil {
		t.Fatal(err)
	}

	changed, err := c.Enforce(ctx)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0", changed)
	}
	for _, u := range st.Users.List() {
		if !u.Enabled {
			t.Errorf("%q was disabled unnecessarily", u.Name)
		}
	}
}

// Expiry is checked before quota so a user who is both gets the more meaningful
// reason: their time ran out, and resetting usage would not bring them back.
func TestEnforcePrefersExpiredOverQuota(t *testing.T) {
	ctx := context.Background()
	c, st, _ := testCollector(t)
	now := time.Now()

	past := now.Add(-time.Hour)
	u, err := st.Users.Create(store.CreateParams{Name: "both", QuotaBytes: 10, ExpiresAt: &past}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Usage.Add(ctx, now, []store.Delta{{UserID: u.ID, Up: 100, Down: 100}}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Enforce(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.Users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisabledReason != ReasonExpired {
		t.Errorf("disabled_reason = %q, want %q", got.DisabledReason, ReasonExpired)
	}
}

func TestEnforceSkipsAlreadyDisabled(t *testing.T) {
	ctx := context.Background()
	c, st, _ := testCollector(t)
	now := time.Now()

	past := now.Add(-time.Hour)
	if _, err := st.Users.Create(store.CreateParams{Name: "alice", ExpiresAt: &past, Disabled: true}, now); err != nil {
		t.Fatal(err)
	}
	changed, err := c.Enforce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Already off: reporting a change would trigger a pointless reload, and each
	// reload drops live connections.
	if changed != 0 {
		t.Errorf("changed = %d, want 0", changed)
	}
}

func TestEnforceAndReloadOnlyReloadsOnChange(t *testing.T) {
	ctx := context.Background()
	c, st, r := testCollector(t)
	now := time.Now()

	if _, err := st.Users.Create(store.CreateParams{Name: "healthy"}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.enforceAndReload(ctx); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Errorf("reloaded %d times with nothing to change, want 0", r.calls)
	}

	past := now.Add(-time.Hour)
	if _, err := st.Users.Create(store.CreateParams{Name: "expired", ExpiresAt: &past}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.enforceAndReload(ctx); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Errorf("reloaded %d times after a change, want 1", r.calls)
	}
}
