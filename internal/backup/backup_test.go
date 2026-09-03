package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"vlessvmore/internal/config"
	"vlessvmore/internal/store"
)

func dataEntry(name string) string   { return path.Join(ArchiveDataDir, name) }
func configEntry(name string) string { return path.Join(ArchiveConfigDir, name) }

// RFC 7748 Alice, so nothing here depends on a generated key.
const testPrivateKey = "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo"

const testConfig = `{
  "host": "vpn.example.test"
}`

// testSource seeds a deployment with something in every file the archive should carry.
func testSource(t *testing.T) (Source, *store.Store) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.Identity.Replace(store.Identity{
		PrivateKey: testPrivateKey,
		ShortID:    "0102030405060708",
	}); err != nil {
		t.Fatalf("install identity: %v", err)
	}
	u, err := st.Users.Create(store.CreateParams{Name: "alice"}, time.Now())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, _, err := st.Tokens.Create("web", time.Now()); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := st.Usage.Add(ctx, time.Now(), []store.Delta{{UserID: u.ID, Up: 11, Down: 22}}); err != nil {
		t.Fatalf("add usage: %v", err)
	}
	// sing-box.json is a render artifact, absent until something writes it.
	if err := os.WriteFile(st.RenderedConfigPath(), []byte(`{"log":{}}`), 0o600); err != nil {
		t.Fatalf("write rendered config: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(testConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return Source{
		Store:      st,
		ConfigPath: configPath,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st
}

// members unpacks an archive, checking the properties every entry has to have.
func members(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	defer gz.Close()

	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		// The archive carries the Reality private key, so an extracted copy must not be
		// readable by anyone but its owner.
		if hdr.Mode != 0o600 {
			t.Errorf("%s has mode %#o, want 0600", hdr.Name, hdr.Mode)
		}
		// A directory entry would re-chmod an existing data directory on extract.
		if hdr.Typeflag != tar.TypeReg {
			t.Errorf("%s has type %q, want a regular file", hdr.Name, hdr.Typeflag)
		}
		out[hdr.Name] = data
	}
	return out
}

func names(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestArchiveLayout(t *testing.T) {
	src, st := testSource(t)

	archive, err := src.Build(context.Background(), true, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := members(t, archive.Body)

	want := []string{
		configEntry("config.json"),
		dataEntry(store.IdentityFile),
		dataEntry(store.UsersFile),
		dataEntry(store.TokensFile),
		dataEntry(store.RenderedConfig),
		dataEntry(store.StatsFile),
	}
	sort.Strings(want)
	if have := names(got); !equal(have, want) {
		t.Errorf("members = %v\nwant     = %v", have, want)
	}

	// Every file is the one on disk, byte for byte, so extracting the archive over a
	// stopped deployment reproduces it exactly.
	if have := string(got[configEntry("config.json")]); have != testConfig {
		t.Errorf("config.json = %q, want the file verbatim:\n%q", have, testConfig)
	}
	for _, name := range []string{store.IdentityFile, store.UsersFile, store.TokensFile, store.RenderedConfig} {
		onDisk, err := os.ReadFile(filepath.Join(st.Dir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(got[dataEntry(name)], onDisk) {
			t.Errorf("%s in the archive differs from the file on disk", name)
		}
	}
}

// The archived stats.db must be a whole, self-contained database — the point of going
// through VACUUM INTO instead of copying a file that has half its state in the -wal.
func TestArchivedStatsDatabaseOpens(t *testing.T) {
	src, _ := testSource(t)

	archive, err := src.Build(context.Background(), true, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	path := filepath.Join(t.TempDir(), store.StatsFile)
	if err := os.WriteFile(path, members(t, archive.Body)[dataEntry(store.StatsFile)], 0o600); err != nil {
		t.Fatalf("write extracted stats.db: %v", err)
	}
	usage, err := store.OpenUsage(path)
	if err != nil {
		t.Fatalf("the archived stats.db does not open: %v", err)
	}
	defer usage.Close()

	totals, err := usage.Totals(context.Background())
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("archived stats.db holds %d users, want 1", len(totals))
	}
	for _, p := range totals {
		if p.Up != 11 || p.Down != 22 {
			t.Errorf("archived usage = up %d down %d, want 11/22", p.Up, p.Down)
		}
	}
}

// Files a young deployment has not written yet must not fail the backup.
func TestArchiveWithMissingFiles(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	src := Source{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	archive, err := src.Build(context.Background(), true, time.Now())
	if err != nil {
		t.Fatalf("Build on a fresh deployment: %v", err)
	}
	got := members(t, archive.Body)
	if _, ok := got[dataEntry(store.StatsFile)]; !ok {
		t.Error("a fresh deployment's archive has no stats.db")
	}
	if _, ok := got[dataEntry(store.TokensFile)]; ok {
		t.Error("tokens.json is in the archive before any token was minted")
	}
}

// config.json is regenerable with `vlessvmore init`; the keypair beside it is not. Losing
// it must cost a log line, not the backup.
func TestArchiveWithoutConfigFile(t *testing.T) {
	src, _ := testSource(t)
	if err := os.Remove(src.ConfigPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}

	archive, err := src.Build(context.Background(), true, time.Now())
	if err != nil {
		t.Fatalf("Build without config.json: %v", err)
	}
	got := members(t, archive.Body)
	if _, ok := got[configEntry("config.json")]; ok {
		t.Error("config.json is in the archive although it does not exist")
	}
	if _, ok := got[dataEntry(store.IdentityFile)]; !ok {
		t.Error("the keypair is missing from an archive taken without config.json")
	}
}

// The pusher builds on a timer, so back-to-back builds must both come back whole. VACUUM
// INTO refuses an existing target, which makes a leaked temp file a real risk.
func TestArchiveIsRepeatable(t *testing.T) {
	src, _ := testSource(t)
	ctx := context.Background()

	first, err := src.Build(ctx, true, time.Now())
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second, err := src.Build(ctx, true, time.Now())
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if !equal(names(members(t, first.Body)), names(members(t, second.Body))) {
		t.Error("two consecutive archives carry different members")
	}
	// The digest is what decides whether anything is sent, so an unchanged deployment
	// producing two of them is the whole mechanism failing quietly.
	if !bytes.Equal(first.Digest, second.Digest) {
		t.Error("an unchanged deployment produced two different digests")
	}
}

func TestArchiveNamedForWhenItWasTaken(t *testing.T) {
	at := time.Date(2026, 7, 29, 3, 15, 0, 0, time.UTC)
	if got, want := Filename(at), "vlessvmore-20260729_031500.tgz"; got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
	// UTC regardless of where the server thinks it is, or a series sorts by nothing.
	east := time.FixedZone("east", 4*60*60)
	if got := Filename(at.In(east)); got != "vlessvmore-20260729_031500.tgz" {
		t.Errorf("Filename in a non-UTC zone = %q", got)
	}
}

// received collects what a stand-in backup agent was sent.
type received struct {
	mu     sync.Mutex
	bodies [][]byte
	names  []string
	status int
}

func (r *received) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.status != 0 && r.status != http.StatusOK {
			http.Error(w, "the remote is unreachable", r.status)
			return
		}
		f, hdr, err := req.FormFile("backup")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.bodies = append(r.bodies, body)
		r.names = append(r.names, hdr.Filename)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *received) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *received) last(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatal("nothing was sent")
	}
	return r.bodies[len(r.bodies)-1]
}

func testPusher(t *testing.T, mode config.BackupMode) (*Pusher, *store.Store, *received) {
	t.Helper()
	src, st := testSource(t)
	r := &received{}
	return &Pusher{Source: src, URL: r.server(t).URL, Mode: mode, Client: http.DefaultClient}, st, r
}

func TestWhatEachModeCarries(t *testing.T) {
	tests := []struct {
		mode  config.BackupMode
		stats bool
	}{
		{config.BackupState, false},
		{config.BackupRelaxed, true},
		{config.BackupAll, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			p, _, r := testPusher(t, tt.mode)
			if err := p.Once(context.Background()); err != nil {
				t.Fatalf("Once: %v", err)
			}
			got := members(t, r.last(t))
			if _, ok := got[dataEntry(store.StatsFile)]; ok != tt.stats {
				t.Errorf("stats.db present = %v, want %v", ok, tt.stats)
			}
			// The keypair is in every mode. There is no mode that leaves it out, because
			// an archive without it restores a server none of its clients can reach.
			if _, ok := got[dataEntry(store.IdentityFile)]; !ok {
				t.Error("the keypair is missing from the archive")
			}
		})
	}
}

// Every mode sends when the deployment changes, and none of them sends when it has not.
func TestSendsOnAChangeAndNotBefore(t *testing.T) {
	for _, mode := range []config.BackupMode{config.BackupState, config.BackupRelaxed, config.BackupAll} {
		t.Run(string(mode), func(t *testing.T) {
			p, st, r := testPusher(t, mode)
			ctx := context.Background()

			// The first pass always sends: a process that has just started may be one
			// restarted onto a volume nobody has a copy of.
			if err := p.Once(ctx); err != nil {
				t.Fatalf("first Once: %v", err)
			}
			if r.count() != 1 {
				t.Fatalf("the first pass sent %d archives, want 1", r.count())
			}

			if err := p.Once(ctx); err != nil {
				t.Fatalf("second Once: %v", err)
			}
			if r.count() != 1 {
				t.Errorf("an unchanged deployment sent %d archives, want 1", r.count())
			}

			if _, err := st.Users.Create(store.CreateParams{Name: "bob"}, time.Now()); err != nil {
				t.Fatalf("create user: %v", err)
			}
			if err := p.Once(ctx); err != nil {
				t.Fatalf("Once after a change: %v", err)
			}
			if r.count() != 2 {
				t.Errorf("a changed deployment sent %d archives, want 2", r.count())
			}
		})
	}
}

// Traffic is measurement, not intent. It arrives every polling interval on a server nobody
// is administering, so letting it decide would turn every deployment back into one that
// uploads on a timer — which is the thing pushing replaced.
func TestTrafficAloneDoesNotSend(t *testing.T) {
	p, st, r := testPusher(t, config.BackupRelaxed)
	ctx := context.Background()

	if err := p.Once(ctx); err != nil {
		t.Fatalf("first Once: %v", err)
	}
	users := st.Users.List()
	if len(users) == 0 {
		t.Fatal("no seeded user to attribute traffic to")
	}
	if err := st.Usage.Add(ctx, time.Now(), []store.Delta{{UserID: users[0].ID, Up: 9000, Down: 9000}}); err != nil {
		t.Fatalf("add usage: %v", err)
	}
	if err := p.Once(ctx); err != nil {
		t.Fatalf("Once after traffic: %v", err)
	}
	if r.count() != 1 {
		t.Errorf("traffic alone sent %d archives, want 1", r.count())
	}
}

// The floor is what catches the traffic the check above deliberately ignores.
func TestAllSendsOnItsFloor(t *testing.T) {
	p, st, r := testPusher(t, config.BackupAll)
	ctx := context.Background()

	if err := p.Once(ctx); err != nil {
		t.Fatalf("first Once: %v", err)
	}
	digest, _, err := st.Usage.LastBackup(ctx)
	if err != nil {
		t.Fatalf("LastBackup: %v", err)
	}
	// Backdate the record rather than wait half an hour for it.
	stale := time.Now().Add(-config.BackupAllPeriod - time.Minute)
	if err := st.Usage.RecordBackup(ctx, digest, stale); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	if err := p.Once(ctx); err != nil {
		t.Fatalf("Once past the floor: %v", err)
	}
	if r.count() != 2 {
		t.Errorf("the floor sent %d archives, want 2", r.count())
	}

	// And the push resets it, so the floor is a floor rather than a repeating alarm.
	if err := p.Once(ctx); err != nil {
		t.Fatalf("Once after the floor fired: %v", err)
	}
	if r.count() != 2 {
		t.Errorf("the floor fired twice in a row, sending %d archives", r.count())
	}
}

// Only `all` promises a floor, so the other two must sit still past it.
func TestOnlyAllHasAFloor(t *testing.T) {
	for _, mode := range []config.BackupMode{config.BackupState, config.BackupRelaxed} {
		t.Run(string(mode), func(t *testing.T) {
			p, st, r := testPusher(t, mode)
			ctx := context.Background()

			if err := p.Once(ctx); err != nil {
				t.Fatalf("first Once: %v", err)
			}
			digest, _, err := st.Usage.LastBackup(ctx)
			if err != nil {
				t.Fatalf("LastBackup: %v", err)
			}
			stale := time.Now().Add(-24 * time.Hour)
			if err := st.Usage.RecordBackup(ctx, digest, stale); err != nil {
				t.Fatalf("RecordBackup: %v", err)
			}

			if err := p.Once(ctx); err != nil {
				t.Fatalf("Once a day later: %v", err)
			}
			if r.count() != 1 {
				t.Errorf("%s sent %d archives with nothing changed, want 1", mode, r.count())
			}
		})
	}
}

// A rejected upload recorded as a success would leave this process believing a copy exists
// that does not, and on a deployment nobody is editing nothing would ever try again.
func TestARejectedUploadIsNotRemembered(t *testing.T) {
	p, st, r := testPusher(t, config.BackupRelaxed)
	ctx := context.Background()

	r.mu.Lock()
	r.status = http.StatusInternalServerError
	r.mu.Unlock()

	if err := p.Once(ctx); err == nil {
		t.Fatal("a rejected upload was reported as a success")
	}
	digest, at, err := st.Usage.LastBackup(ctx)
	if err != nil {
		t.Fatalf("LastBackup: %v", err)
	}
	if digest != nil || !at.IsZero() {
		t.Errorf("a rejected upload was recorded: digest %x at %s", digest, at)
	}

	// And the next pass tries again rather than waiting for somebody to add a user.
	r.mu.Lock()
	r.status = http.StatusOK
	r.mu.Unlock()
	if err := p.Once(ctx); err != nil {
		t.Fatalf("Once after the agent recovered: %v", err)
	}
	if r.count() != 1 {
		t.Errorf("the retry sent %d archives, want 1", r.count())
	}
}

// The rejection has to carry what the agent said. "The agent answered 500" is a fact nobody
// can act on; the body underneath is where the rejected token lives.
func TestARejectionCarriesWhatTheAgentSaid(t *testing.T) {
	p, _, r := testPusher(t, config.BackupRelaxed)
	r.mu.Lock()
	r.status = http.StatusInternalServerError
	r.mu.Unlock()

	err := p.Once(context.Background())
	if err == nil {
		t.Fatal("no error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("the remote is unreachable")) {
		t.Errorf("error = %q, want the agent's own message in it", err)
	}
}

// No address is the off switch, and Run has to return rather than log about somewhere
// nobody meant to send anything.
func TestNoAddressMeansNoBackups(t *testing.T) {
	src, st := testSource(t)
	p := &Pusher{Source: src, Mode: config.BackupRelaxed}

	done := make(chan struct{})
	go func() {
		p.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return with no address configured")
	}

	digest, _, err := st.Usage.LastBackup(context.Background())
	if err != nil {
		t.Fatalf("LastBackup: %v", err)
	}
	if digest != nil {
		t.Error("something was recorded as backed up with no address configured")
	}
}
