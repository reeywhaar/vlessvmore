package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"vlessvmore/internal/store"
)

func dataEntry(name string) string   { return path.Join(ArchiveDataDir, name) }
func configEntry(name string) string { return path.Join(ArchiveConfigDir, name) }

// getBackup fetches /backup from the backup handler and returns the archive's members.
func getBackup(t *testing.T, s *Server) (*httptest.ResponseRecorder, map[string][]byte) {
	t.Helper()

	rec := httptest.NewRecorder()
	s.BackupHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BackupPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200\n%s", BackupPath, rec.Code, rec.Body.String())
	}

	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	defer gz.Close()

	members := map[string][]byte{}
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
		members[hdr.Name] = data
	}
	return rec, members
}

// seed gives the store something in every file the archive should carry.
func seed(t *testing.T, s *Server, st *store.Store) {
	t.Helper()
	ctx := context.Background()

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
}

func TestBackupArchiveLayout(t *testing.T) {
	s, st := testServer(t)
	seed(t, s, st)

	rec, members := getBackup(t, s)

	if got := rec.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", got)
	}
	// A wrong Content-Length truncates the client's read, so it has to match the body
	// actually written rather than merely being present.
	if got, want := rec.Header().Get("Content-Length"), rec.Body.Len(); got != strconv.Itoa(want) {
		t.Errorf("Content-Length = %q, body is %d bytes", got, want)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "vlessvmore-") {
		t.Errorf("Content-Disposition = %q, want a vlessvmore-<ts>.tgz filename", got)
	}

	want := []string{
		configEntry("config.json"),
		dataEntry(store.IdentityFile),
		dataEntry(store.UsersFile),
		dataEntry(store.TokensFile),
		dataEntry(store.RenderedConfig),
		dataEntry(store.StatsFile),
	}
	sort.Strings(want)
	if got := keysOf(members); !equalStrings(got, want) {
		t.Errorf("members = %v\nwant     = %v", got, want)
	}

	// Every file is the one on disk, byte for byte, so extracting the archive over a
	// stopped deployment reproduces it exactly.
	if got := string(members[configEntry("config.json")]); got != testConfig {
		t.Errorf("config.json = %q, want the file verbatim:\n%q", got, testConfig)
	}
	for _, name := range []string{store.IdentityFile, store.UsersFile, store.TokensFile, store.RenderedConfig} {
		onDisk, err := os.ReadFile(filepath.Join(st.Dir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(members[dataEntry(name)], onDisk) {
			t.Errorf("%s in the archive differs from the file on disk", name)
		}
	}
}

// The archived stats.db must be a whole, self-contained database — the point of going
// through VACUUM INTO instead of copying a file that has half its state in the -wal.
func TestBackupStatsDatabaseIsUsable(t *testing.T) {
	s, st := testServer(t)
	seed(t, s, st)

	_, members := getBackup(t, s)

	restored := filepath.Join(t.TempDir(), store.StatsFile)
	if err := os.WriteFile(restored, members[dataEntry(store.StatsFile)], 0o600); err != nil {
		t.Fatalf("write restored db: %v", err)
	}
	// No -wal or -shm alongside it, exactly as it comes out of the archive.
	usage, err := store.OpenUsage(restored)
	if err != nil {
		t.Fatalf("the archived stats.db does not open: %v", err)
	}
	defer usage.Close()

	rows, err := usage.Export(context.Background())
	if err != nil {
		t.Fatalf("export from the archived stats.db: %v", err)
	}
	if len(rows) != 1 || rows[0].Up != 11 || rows[0].Down != 22 {
		t.Errorf("archived usage = %+v, want one bucket of 11/22", rows)
	}
}

// Files that a young deployment has not written yet must not fail the backup.
func TestBackupWithMissingFiles(t *testing.T) {
	s, st := testServer(t)

	// No users, no tokens, no render — only identity.json, which Open always creates.
	_, members := getBackup(t, s)

	if _, ok := members[dataEntry(store.IdentityFile)]; !ok {
		t.Errorf("%s is missing", store.IdentityFile)
	}
	if _, ok := members[dataEntry(store.StatsFile)]; !ok {
		t.Errorf("%s is missing", store.StatsFile)
	}
	if _, ok := members[dataEntry(store.RenderedConfig)]; ok {
		t.Errorf("%s is present but was never rendered", store.RenderedConfig)
	}
	_ = st
}

// config.json is regenerable with `vlessvmore init`; the data directory is not. Losing it
// must cost a log line, not the backup.
func TestBackupWithoutConfigFile(t *testing.T) {
	s, _ := testServer(t)
	if err := os.Remove(s.configPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}

	_, members := getBackup(t, s)
	if _, ok := members[configEntry("config.json")]; ok {
		t.Errorf("config.json is present after deleting the file")
	}
	if _, ok := members[dataEntry(store.IdentityFile)]; !ok {
		t.Errorf("%s is missing", store.IdentityFile)
	}
}

// The sidecar calls this on a timer, so back-to-back requests must both come back whole.
// VACUUM INTO refuses an existing target, which makes a leaked temp file a real risk.
func TestBackupIsRepeatable(t *testing.T) {
	s, st := testServer(t)
	seed(t, s, st)

	_, first := getBackup(t, s)
	_, second := getBackup(t, s)

	if !equalStrings(keysOf(first), keysOf(second)) {
		t.Errorf("members changed between backups: %v then %v", keysOf(first), keysOf(second))
	}
	if !bytes.Equal(first[dataEntry(store.IdentityFile)], second[dataEntry(store.IdentityFile)]) {
		t.Error("identity.json differs between two consecutive backups")
	}
}

// The two listeners must not answer each other's routes. /backup is unauthenticated, so
// reaching it from the internet-facing port would hand out the Reality key to anyone.
func TestBackupAndApiRoutesAreSeparate(t *testing.T) {
	s, _ := testServer(t)

	tests := []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{"backup on the trusted api handler", s.Handler(false), BackupPath},
		{"backup on the public api handler", s.Handler(true), BackupPath},
		{"api on the backup handler", s.BackupHandler(), "/api/users"},
		{"unknown path on the backup handler", s.BackupHandler(), "/nope"},
		{"root on the backup handler", s.BackupHandler(), "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", tt.path, rec.Code)
			}
		})
	}
}

func TestBackupRejectsOtherMethods(t *testing.T) {
	s, _ := testServer(t)
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		s.BackupHandler().ServeHTTP(rec, httptest.NewRequest(method, BackupPath, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", method, BackupPath, rec.Code)
		}
	}
}

func TestBackupFilename(t *testing.T) {
	got := BackupFilename(time.Date(2026, 7, 29, 3, 15, 0, 0, time.UTC))
	if want := "vlessvmore-20260729_031500.tgz"; got != want {
		t.Errorf("BackupFilename() = %q, want %q", got, want)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
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
