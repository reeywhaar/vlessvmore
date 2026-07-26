package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// RFC 7748 Alice — a published test vector, never anyone's real key.
const (
	testPrivateKey = "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo"
	testPublicKey  = "hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo"
	testShortID    = "0102030405060708"
)

func TestIdentityGeneratedOnFirstOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if !s.Identity.Generated() {
		t.Error("Generated() = false on a fresh directory")
	}
	id := s.Identity.Get()
	if id.PrivateKey == "" || id.ShortID == "" {
		t.Fatalf("identity is incomplete: %+v", id)
	}
	if _, err := s.Identity.PublicKey(); err != nil {
		t.Errorf("PublicKey: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IdentityFile)); err != nil {
		t.Errorf("identity.json was not written: %v", err)
	}
}

// The whole reason the keypair lives on disk: a restart must not change it, or every
// client stops connecting.
func TestIdentityStableAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := first.Identity.Get()
	wantPub, err := first.Identity.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	for range 3 {
		again, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := again.Identity.Get()
		gotPub, err := again.Identity.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		if again.Identity.Generated() {
			t.Error("Generated() = true when reopening an existing directory")
		}
		if got.PrivateKey != want.PrivateKey || got.ShortID != want.ShortID {
			t.Fatalf("identity changed across reopen:\n got %+v\nwant %+v", got, want)
		}
		if gotPub != wantPub {
			t.Fatalf("public key changed across reopen: %s vs %s", gotPub, wantPub)
		}
		again.Close()
	}
}

func TestIdentityFilePermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	info, err := os.Stat(filepath.Join(dir, IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	// It is the server's private key.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("identity.json mode = %o, want 600", perm)
	}
}

func TestIdentityReplace(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Identity.Replace(Identity{PrivateKey: testPrivateKey, ShortID: testShortID}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	pub, err := s.Identity.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub != testPublicKey {
		t.Errorf("PublicKey() = %q, want %q", pub, testPublicKey)
	}
	if s.Identity.Generated() {
		t.Error("Generated() should be false after an explicit Replace")
	}
	s.Close()

	// And it persists.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.Identity.Get().PrivateKey; got != testPrivateKey {
		t.Errorf("replaced key did not persist: %q", got)
	}
}

func TestIdentityReplaceRejectsBadKeys(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := s.Identity.Get()

	tests := map[string]Identity{
		"empty key":      {PrivateKey: "", ShortID: testShortID},
		"short key":      {PrivateKey: "AAAA", ShortID: testShortID},
		"padded key":     {PrivateKey: testPrivateKey + "=", ShortID: testShortID},
		"empty short id": {PrivateKey: testPrivateKey, ShortID: ""},
		"non-hex":        {PrivateKey: testPrivateKey, ShortID: "zz"},
		"long short id":  {PrivateKey: testPrivateKey, ShortID: "000102030405060708"},
	}
	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			err := s.Identity.Replace(id)
			if err == nil {
				t.Fatal("Replace accepted invalid key material")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid so the API answers 400", err)
			}
			// A rejected Replace must not have disturbed the live identity.
			if s.Identity.Get() != before {
				t.Error("a failed Replace changed the stored identity")
			}
		})
	}
}

func TestOpenRejectsCorruptIdentityFile(t *testing.T) {
	tests := map[string]string{
		"bad json":      `{`,
		"wrong version": `{"version": 99, "private_key": "` + testPrivateKey + `", "short_id": "` + testShortID + `"}`,
		"bad key":       `{"version": 1, "private_key": "nope", "short_id": "` + testShortID + `"}`,
		"bad short id":  `{"version": 1, "private_key": "` + testPrivateKey + `", "short_id": "zz"}`,
		"unknown field": `{"version": 1, "private_key": "` + testPrivateKey + `", "short_id": "` + testShortID + `", "public_key": "x"}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, IdentityFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			// Refusing to start beats silently generating a replacement key, which would
			// break every client while looking like a successful boot.
			if _, err := Open(dir); err == nil {
				t.Error("Open accepted a corrupt identity.json")
			}
		})
	}
}

// The dangerous case: identity.json lost while users.json survived. Every client would
// break, so it must be reported loudly rather than passed over.
func TestWarnOnUnexpectedIdentity(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Users.Create(CreateParams{Name: "alice"}, now); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Simulate the loss.
	if err := os.Remove(filepath.Join(dir, IdentityFile)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	var buf bytes.Buffer
	reopened.WarnOnUnexpectedIdentity(newTestLogger(&buf))

	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("level=ERROR")) {
		t.Errorf("expected an ERROR-level log, got: %s", out)
	}
	for _, want := range []string{"NEW Reality identity", "users already exist"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("log should mention %q, got: %s", want, out)
		}
	}
}

func TestWarnOnUnexpectedIdentityQuietOnFreshInstall(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var buf bytes.Buffer
	s.WarnOnUnexpectedIdentity(newTestLogger(&buf))

	// A brand new deployment generating a key is unremarkable.
	if bytes.Contains(buf.Bytes(), []byte("level=ERROR")) {
		t.Errorf("a fresh install should not log an error: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("generated a new Reality identity")) {
		t.Errorf("expected an info line, got: %s", buf.String())
	}
}

func TestWarnOnUnexpectedIdentitySilentWhenLoaded(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var buf bytes.Buffer
	s.WarnOnUnexpectedIdentity(newTestLogger(&buf))
	if buf.Len() != 0 {
		t.Errorf("nothing should be logged when the identity was loaded: %s", buf.String())
	}
}

func TestExportImportCarriesIdentity(t *testing.T) {
	ctx := context.Background()

	src, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.Identity.Replace(Identity{PrivateKey: testPrivateKey, ShortID: testShortID}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Users.Create(CreateParams{Name: "alice"}, now); err != nil {
		t.Fatal(err)
	}

	dump, err := src.Export(ctx, now, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Included by default: a restore without it produces a server none of the restored
	// users can reach.
	if dump.Identity == nil {
		t.Fatal("export omitted the identity")
	}

	var buf bytes.Buffer
	if err := dump.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDump(&buf)
	if err != nil {
		t.Fatal(err)
	}

	dst, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.Import(ctx, read, false); err != nil {
		t.Fatal(err)
	}

	pub, err := dst.Identity.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub != testPublicKey {
		t.Errorf("imported public key = %q, want %q — clients would break", pub, testPublicKey)
	}
}

func TestExportCanExcludeIdentity(t *testing.T) {
	ctx := context.Background()
	src, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dump, err := src.Export(ctx, now, ExportOptions{ExcludeIdentity: true})
	if err != nil {
		t.Fatal(err)
	}
	if dump.Identity != nil {
		t.Error("ExcludeIdentity did not omit the identity")
	}

	// Importing a dump with no identity must leave the destination's own key alone
	// rather than blanking it.
	dst, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	before := dst.Identity.Get()
	if err := dst.Import(ctx, dump, false); err != nil {
		t.Fatal(err)
	}
	if dst.Identity.Get() != before {
		t.Error("importing a dump without an identity changed the existing one")
	}
}

// A separate process (`vlessvmore identity set`) writes identity.json directly, so a
// long-running daemon has to notice. Without this, rotating and reloading looks like it
// worked while the old key stays live until the next restart.
func TestIdentityReloadPicksUpExternalChange(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original := s.Identity.Get()

	// Another process replaces the file.
	writer, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Identity.Replace(Identity{PrivateKey: testPrivateKey, ShortID: testShortID}); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	// The first handle still has the old key in memory.
	if s.Identity.Get().PrivateKey != original.PrivateKey {
		t.Fatal("in-memory identity changed without a reload")
	}

	if err := s.Identity.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.Identity.Get().PrivateKey; got != testPrivateKey {
		t.Errorf("after Reload private key = %q, want the externally written one", got)
	}
	pub, err := s.Identity.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub != testPublicKey {
		t.Errorf("after Reload public key = %q, want %q", pub, testPublicKey)
	}
}

// If identity.json vanishes under a running server, keeping the key we already have is
// far better than inventing a new one and breaking every client.
func TestIdentityReloadKeepsKeyWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := s.Identity.Get()

	if err := os.Remove(filepath.Join(dir, IdentityFile)); err != nil {
		t.Fatal(err)
	}
	if err := s.Identity.Reload(); err != nil {
		t.Errorf("Reload on a missing file should not error, got: %v", err)
	}
	if s.Identity.Get() != before {
		t.Error("a missing file changed the in-memory identity")
	}
}

func TestIdentityReloadRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := s.Identity.Get()

	if err := os.WriteFile(filepath.Join(dir, IdentityFile), []byte(`{"version":1,"private_key":"bad","short_id":"ab"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An error here is what makes the caller decline to render, leaving the working
	// config in place rather than guessing.
	if err := s.Identity.Reload(); err == nil {
		t.Error("Reload accepted a corrupt identity.json")
	}
	if s.Identity.Get() != before {
		t.Error("a corrupt file changed the in-memory identity")
	}
}
