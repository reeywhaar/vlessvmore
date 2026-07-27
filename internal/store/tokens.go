package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"vlessvmore/internal/ids"
)

// Token is an API credential. Only the hash is stored: a leaked tokens.json cannot
// be replayed against the API, and the secret is shown exactly once, at creation.
type Token struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hash  string `json:"hash"`

	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the token may still authenticate.
func (t *Token) Active() bool { return t.RevokedAt == nil }

type tokensDoc struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

// Tokens is the JSON-backed token list.
type Tokens struct {
	path string
	mu   sync.RWMutex
	list []Token

	// lastUsedFlushed throttles persistence of LastUsedAt. Without it every
	// authenticated request would rewrite tokens.json, turning a read-heavy API into
	// a write-heavy one for no benefit.
	lastUsedFlushed map[string]time.Time
}

// LastUsedResolution is how coarsely LastUsedAt is persisted. In-memory tracking is
// exact; only the write to disk is throttled.
const LastUsedResolution = time.Minute

// OpenTokens loads tokens.json, treating a missing file as an empty list.
func OpenTokens(path string) (*Tokens, error) {
	var doc tokensDoc
	found, err := readJSON(path, &doc)
	if err != nil {
		return nil, err
	}
	if found && doc.Version != jsonVersion {
		return nil, fmt.Errorf("%s: unsupported version %d, this build understands %d", path, doc.Version, jsonVersion)
	}
	t := &Tokens{path: path, list: doc.Tokens, lastUsedFlushed: map[string]time.Time{}}
	seen := make(map[string]bool, len(t.list))
	for i, tok := range t.list {
		if tok.ID == "" {
			return nil, fmt.Errorf("%s: tokens[%d]: id is empty", path, i)
		}
		if tok.Hash == "" {
			return nil, fmt.Errorf("%s: tokens[%d] (%s): hash is empty", path, i, tok.ID)
		}
		if seen[tok.Hash] {
			return nil, fmt.Errorf("%s: tokens[%d]: duplicate hash", path, i)
		}
		seen[tok.Hash] = true
		if tok.LastUsedAt != nil {
			t.lastUsedFlushed[tok.ID] = *tok.LastUsedAt
		}
	}
	return t, nil
}

// Path returns the backing file.
func (s *Tokens) Path() string { return s.path }

// HashSecret is the one-way mapping from a bearer secret to what we store.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// List returns a copy of the tokens.
func (s *Tokens) List() []Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Not slices.Clone: it returns nil for a nil input, and this is marshalled straight
	// into a JSON response where a nil slice becomes `null` rather than `[]`. A fresh
	// node has an empty list, so that is the first thing a caller ever sees.
	return append(make([]Token, 0, len(s.list)), s.list...)
}

// Create mints a token and returns it alongside the secret, which is the only time
// the secret exists outside the caller's hands.
func (s *Tokens) Create(label string, now time.Time) (*Token, string, error) {
	if strings.TrimSpace(label) == "" {
		return nil, "", fmt.Errorf("%w: label is required", ErrInvalid)
	}
	id, err := ids.NewToken()
	if err != nil {
		return nil, "", err
	}
	secret, err := generateSecret()
	if err != nil {
		return nil, "", err
	}

	tok := Token{
		ID:        id,
		Label:     label,
		Hash:      HashSecret(secret),
		CreatedAt: now.UTC().Truncate(time.Second),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, tok)
	if err := s.save(); err != nil {
		s.list = s.list[:len(s.list)-1]
		return nil, "", err
	}
	return &tok, secret, nil
}

// Lookup finds an active token by its secret and records the use. It returns
// ErrNotFound for both unknown and revoked secrets, so a caller cannot distinguish
// the two.
func (s *Tokens) Lookup(secret string, now time.Time) (*Token, error) {
	hash := HashSecret(secret)

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.list {
		if s.list[i].Hash != hash || !s.list[i].Active() {
			continue
		}
		at := now.UTC().Truncate(time.Second)
		s.list[i].LastUsedAt = &at
		tok := s.list[i]

		// Persist only when the coarse timestamp actually moved.
		if last, ok := s.lastUsedFlushed[tok.ID]; !ok || at.Sub(last) >= LastUsedResolution {
			s.lastUsedFlushed[tok.ID] = at
			if err := s.save(); err != nil {
				// A failed bookkeeping write must not fail the request: the token is
				// valid regardless of whether we recorded the timestamp.
				return &tok, nil
			}
		}
		return &tok, nil
	}
	return nil, ErrNotFound
}

// Revoke marks a token unusable, keeping the row for audit.
func (s *Tokens) Revoke(ref string, now time.Time) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(ref)
	if i < 0 {
		return nil, fmt.Errorf("token %q: %w", ref, ErrNotFound)
	}
	before := s.list[i]
	if before.RevokedAt != nil {
		return &before, nil
	}
	at := now.UTC().Truncate(time.Second)
	s.list[i].RevokedAt = &at
	tok := s.list[i]
	if err := s.save(); err != nil {
		s.list[i] = before
		return nil, err
	}
	return &tok, nil
}

// Delete removes a token outright.
func (s *Tokens) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(ref)
	if i < 0 {
		return fmt.Errorf("token %q: %w", ref, ErrNotFound)
	}
	removed := s.list[i]
	s.list = slices.Delete(s.list, i, i+1)
	if err := s.save(); err != nil {
		s.list = slices.Insert(s.list, i, removed)
		return err
	}
	return nil
}

// Replace overwrites the whole list, for `import`.
func (s *Tokens) Replace(list []Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.list
	s.list = slices.Clone(list)
	if err := s.save(); err != nil {
		s.list = before
		return err
	}
	return nil
}

// indexOf matches an id or, failing that, a label. Must hold the lock.
func (s *Tokens) indexOf(ref string) int {
	for i := range s.list {
		if s.list[i].ID == ref {
			return i
		}
	}
	for i := range s.list {
		if s.list[i].Label == ref {
			return i
		}
	}
	return -1
}

func (s *Tokens) save() error {
	return writeJSONAtomic(s.path, tokensDoc{Version: jsonVersion, Tokens: s.list})
}

// secretBytes is the entropy in an API secret. 20 bytes is 160 bits, well past
// anything brute-forceable, and encodes to 32 characters.
const secretBytes = 20

func generateSecret() (string, error) { return ids.Secret(secretBytes) }
