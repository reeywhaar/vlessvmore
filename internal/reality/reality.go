// Package reality generates and validates the Reality key material sing-box needs.
//
// Encodings follow sing-box's common/tls/reality_server.go exactly: private_key is
// base64.RawURLEncoding of a 32-byte X25519 scalar, short_id is hex decoding to at
// most 8 bytes. Getting either wrong produces "REALITY: processed invalid
// connection" on the server and no usable diagnostic on the client, so everything
// here is validated on the way in and on the way out.
package reality

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// KeyLen is the length of a raw X25519 scalar, in bytes.
const KeyLen = 32

// MaxShortIDLen is the maximum decoded length of a Reality short_id, in bytes.
// sing-box accepts anything up to this; we always generate the full 8.
const MaxShortIDLen = 8

// GeneratePrivateKey returns a new Reality private key, base64url-encoded.
func GeneratePrivateKey() (string, error) {
	var b [KeyLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	// Clamp to a valid X25519 scalar, as curve25519.GenerateKey would.
	b[0] &= 248
	b[31] &= 127
	b[31] |= 64
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// PublicKey derives the client-facing public key (the `pbk` URI parameter) from a
// base64url private key. The public key is never stored anywhere: it is derived on
// demand, which makes it impossible for the two halves to drift out of sync.
func PublicKey(privateKey string) (string, error) {
	priv, err := DecodePrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive public key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(pub), nil
}

// DecodePrivateKey validates a base64url private key and returns its raw bytes.
func DecodePrivateKey(privateKey string) ([]byte, error) {
	if privateKey == "" {
		return nil, fmt.Errorf("private key is empty")
	}
	b, err := base64.RawURLEncoding.DecodeString(privateKey)
	if err != nil {
		return nil, fmt.Errorf("private key is not base64url (no padding): %w", err)
	}
	if len(b) != KeyLen {
		return nil, fmt.Errorf("private key decodes to %d bytes, want %d", len(b), KeyLen)
	}
	return b, nil
}

// GenerateShortID returns a new Reality short_id: MaxShortIDLen random bytes, hex-encoded.
func GenerateShortID() (string, error) {
	var b [MaxShortIDLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidateShortID checks that a short_id is hex of an even length decoding to at
// most MaxShortIDLen bytes, which is what sing-box will accept.
func ValidateShortID(shortID string) error {
	if shortID == "" {
		return fmt.Errorf("short id is empty")
	}
	if len(shortID)%2 != 0 {
		return fmt.Errorf("short id %q has odd length %d; must be an even number of hex chars", shortID, len(shortID))
	}
	b, err := hex.DecodeString(shortID)
	if err != nil {
		return fmt.Errorf("short id %q is not hex: %w", shortID, err)
	}
	if len(b) > MaxShortIDLen {
		return fmt.Errorf("short id %q decodes to %d bytes, at most %d allowed", shortID, len(b), MaxShortIDLen)
	}
	return nil
}
