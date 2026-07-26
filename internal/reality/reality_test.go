package reality

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// Published X25519 test vectors from RFC 7748 section 6.1, re-encoded from the
// RFC's hex into the base64url form sing-box uses. These are documentation, not
// anyone's key material — never put a real private key in this repo, see
// private/README.md.
//
// Agreeing with them proves our encoding and derivation match the standard, which
// is what real sing-box clients implement. If this fails, clients see
// "REALITY: processed invalid connection" and nothing else here is worth debugging.
const (
	rfc7748AlicePrivate = "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo"
	rfc7748AlicePublic  = "hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo"
	rfc7748BobPrivate   = "XasIfmJKikt54X-Lg4AO5m87sSkmGLb9HC-LJ_-I4Os"
	rfc7748BobPublic    = "3p7bfXt9wbTTW2HC7OQ1Nz-DQ8hbeGdNrfx-FG-IK08"
)

func TestPublicKeyMatchesRFC7748(t *testing.T) {
	tests := []struct{ name, priv, pub string }{
		{"alice", rfc7748AlicePrivate, rfc7748AlicePublic},
		{"bob", rfc7748BobPrivate, rfc7748BobPublic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PublicKey(tt.priv)
			if err != nil {
				t.Fatalf("PublicKey: %v", err)
			}
			if got != tt.pub {
				t.Fatalf("derived public key does not match the RFC 7748 vector.\n got: %s\nwant: %s", got, tt.pub)
			}
		})
	}
}

// TestPublicKeyMatchesDeployment is the pre-migration safety check: it proves this
// build derives the same public key an existing deployment already hands to its
// clients. Opt in by exporting the pair from outside the repo, e.g.
//
//	VLESSVMORE_TEST_PRIVATE_KEY=… VLESSVMORE_TEST_PUBLIC_KEY=… go test ./internal/reality/
//
// Env vars rather than constants, deliberately: a live private key must never be
// committed. Keep it in private/ if you need it on disk.
func TestPublicKeyMatchesDeployment(t *testing.T) {
	priv := os.Getenv("VLESSVMORE_TEST_PRIVATE_KEY")
	want := os.Getenv("VLESSVMORE_TEST_PUBLIC_KEY")
	if priv == "" || want == "" {
		t.Skip("set VLESSVMORE_TEST_PRIVATE_KEY and VLESSVMORE_TEST_PUBLIC_KEY to check against a real deployment")
	}
	got, err := PublicKey(priv)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if got != want {
		t.Fatalf("derived public key does not match the deployment's; every client would break.\n got: %s\nwant: %s", got, want)
	}
}

func TestGeneratePrivateKeyRoundTrips(t *testing.T) {
	priv, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	// Must be unpadded base64url: padding or +/ would be rejected by sing-box.
	if strings.ContainsAny(priv, "=+/") {
		t.Errorf("private key %q contains padding or non-url-safe base64 chars", priv)
	}
	raw, err := DecodePrivateKey(priv)
	if err != nil {
		t.Fatalf("DecodePrivateKey(%q): %v", priv, err)
	}
	if len(raw) != KeyLen {
		t.Errorf("decoded length = %d, want %d", len(raw), KeyLen)
	}
	// Clamped as X25519 requires.
	if raw[0]&7 != 0 {
		t.Errorf("low 3 bits of byte 0 not cleared: %#x", raw[0])
	}
	if raw[31]&128 != 0 {
		t.Errorf("high bit of byte 31 not cleared: %#x", raw[31])
	}
	if raw[31]&64 == 0 {
		t.Errorf("bit 6 of byte 31 not set: %#x", raw[31])
	}

	pub, err := PublicKey(priv)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(pub)
	if err != nil {
		t.Fatalf("public key %q is not base64url: %v", pub, err)
	}
	if len(pubRaw) != KeyLen {
		t.Errorf("public key decodes to %d bytes, want %d", len(pubRaw), KeyLen)
	}
}

func TestGeneratePrivateKeyIsRandom(t *testing.T) {
	seen := make(map[string]bool, 32)
	for range 32 {
		k, err := GeneratePrivateKey()
		if err != nil {
			t.Fatalf("GeneratePrivateKey: %v", err)
		}
		if seen[k] {
			t.Fatalf("GeneratePrivateKey returned a duplicate: %q", k)
		}
		seen[k] = true
	}
}

func TestDecodePrivateKeyRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"standard base64 padding", rfc7748AlicePrivate + "="},
		{"not base64", "!!!!"},
		{"too short", base64.RawURLEncoding.EncodeToString(make([]byte, 31))},
		{"too long", base64.RawURLEncoding.EncodeToString(make([]byte, 33))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodePrivateKey(tt.key); err == nil {
				t.Errorf("DecodePrivateKey(%q) = nil error, want failure", tt.key)
			}
		})
	}
}

func TestGenerateShortID(t *testing.T) {
	id, err := GenerateShortID()
	if err != nil {
		t.Fatalf("GenerateShortID: %v", err)
	}
	if len(id) != MaxShortIDLen*2 {
		t.Errorf("short id %q has length %d, want %d hex chars", id, len(id), MaxShortIDLen*2)
	}
	if err := ValidateShortID(id); err != nil {
		t.Errorf("ValidateShortID(%q): %v", id, err)
	}
}

func TestValidateShortID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{"eight bytes", "0102030405060708", true},
		{"single byte", "ab", true},
		{"max length", hex.EncodeToString(make([]byte, MaxShortIDLen)), true},
		{"empty", "", false},
		{"odd length", "abc", false},
		{"not hex", "zzzz", false},
		{"too long", hex.EncodeToString(make([]byte, MaxShortIDLen+1)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateShortID(tt.id)
			if tt.ok && err != nil {
				t.Errorf("ValidateShortID(%q) = %v, want nil", tt.id, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("ValidateShortID(%q) = nil, want error", tt.id)
			}
		})
	}
}
