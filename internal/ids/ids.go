// Package ids generates the opaque, time-sortable identifiers used for users and
// API tokens.
//
// An id is a prefix plus 26 characters of Crockford base32 over 16 bytes: a 6-byte
// big-endian millisecond timestamp followed by 10 random bytes. That layout is
// ULID's, and because the alphabet is in ASCII order and the length is fixed, the
// encoded strings sort chronologically — handy for `ORDER BY id` and for eyeballing
// which of two ids is older.
//
// The encoding is Go's standard base32 over those bytes rather than ULID's
// canonical 128-bit-integer encoding, so these are not interchangeable with other
// ULID implementations. They are internal opaque ids and never parsed back, so that
// costs nothing and saves a dependency.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// UserPrefix and TokenPrefix make an id self-describing in logs and API responses.
const (
	UserPrefix  = "u_"
	TokenPrefix = "t_"
)

// crockford omits I, L, O and U so ids cannot accidentally spell things or be
// misread between similar glyphs.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// EncodedLen is the number of base32 characters in the random part of an id.
const EncodedLen = 26

// New returns a new id with the given prefix, timestamped now.
func New(prefix string) (string, error) {
	return newAt(prefix, time.Now())
}

// NewUser returns a new user id.
func NewUser() (string, error) { return New(UserPrefix) }

// NewToken returns a new API token id.
func NewToken() (string, error) { return New(TokenPrefix) }

func newAt(prefix string, t time.Time) (string, error) {
	var b [16]byte
	// 48 bits of milliseconds covers dates to the year 10889; encode the low 6
	// bytes of the millisecond timestamp big-endian so byte order matches time
	// order.
	ms := uint64(t.UTC().UnixMilli())
	binary.BigEndian.PutUint64(b[:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return prefix + crockford.EncodeToString(b[:]), nil
}

// Valid reports whether s looks like an id produced by New with the given prefix.
// It is a cheap shape check for API input, not an existence check.
func Valid(prefix, s string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || len(rest) != EncodedLen {
		return false
	}
	_, err := crockford.DecodeString(rest)
	return err == nil
}

// Secret returns n bytes of randomness in the same base32 alphabet, for use as an
// API bearer secret. Unlike New it carries no timestamp: a credential should leak
// nothing about itself.
func Secret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return crockford.EncodeToString(b), nil
}

// Timestamp recovers the creation time encoded in an id.
func Timestamp(prefix, s string) (time.Time, error) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return time.Time{}, fmt.Errorf("id %q does not start with %q", s, prefix)
	}
	b, err := crockford.DecodeString(rest)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode id %q: %w", s, err)
	}
	if len(b) != 16 {
		return time.Time{}, fmt.Errorf("id %q decodes to %d bytes, want 16", s, len(b))
	}
	var padded [8]byte
	copy(padded[:6], b[:6])
	ms := binary.BigEndian.Uint64(padded[:]) >> 16
	return time.UnixMilli(int64(ms)).UTC(), nil
}
