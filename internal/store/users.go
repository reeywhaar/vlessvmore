package store

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"vlessvmore/internal/ids"
)

// ErrNotFound is returned when a user or token reference matches nothing.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a name or uuid is already taken.
var ErrConflict = errors.New("already exists")

// ErrInvalid marks a caller's input as bad, as opposed to something failing on our
// side. The API needs the distinction to answer 400 rather than 500.
var ErrInvalid = errors.New("invalid")

// User is a client credential plus its limits.
//
// ID is what sing-box sees as the user's `name`: a stable internal id, never the
// display name. Because the UUID is what actually authenticates, the id is
// server-side only — which is why renaming a user keeps their usage history intact.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	UUID string `json:"uuid"`

	Enabled bool `json:"enabled"`

	// QuotaBytes is the traffic ceiling since UsageResetAt; 0 means unlimited.
	QuotaBytes int64      `json:"quota_bytes"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`

	// UsageResetAt is the start of the current quota window.
	UsageResetAt time.Time `json:"usage_reset_at"`

	// DisabledReason records why enforcement turned this user off ("quota",
	// "expired"), so the operator is not left guessing. Empty when disabled by hand.
	DisabledReason string `json:"disabled_reason,omitempty"`

	// SubToken is the unguessable path segment of this user's subscription URL. It is
	// a capability: whoever holds it can fetch the user's credential, because clients
	// polling a subscription cannot send an Authorization header.
	//
	// Separate from UUID on purpose, so it can be rotated to cut off a leaked
	// subscription URL without invalidating the credential itself — and vice versa.
	SubToken string `json:"sub_token"`

	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SubTokenBytes is the entropy in a subscription token. The URL is unauthenticated, so
// this is the only thing standing between an attacker and a working credential; 20
// bytes is 160 bits, which is not guessable.
const SubTokenBytes = 20

// IsExpired reports whether the user's expiry has passed.
func (u *User) IsExpired(now time.Time) bool {
	return u.ExpiresAt != nil && !now.Before(*u.ExpiresAt)
}

type usersDoc struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

// Users is the JSON-backed user list. Every mutation rewrites the whole file
// atomically; at the scale this serves (tens to hundreds of users) the file is a few
// KB and the simplicity is worth far more than incremental writes.
type Users struct {
	path string
	mu   sync.RWMutex
	list []User
}

// OpenUsers loads users.json, treating a missing file as an empty list.
func OpenUsers(path string) (*Users, error) {
	var doc usersDoc
	found, err := readJSON(path, &doc)
	if err != nil {
		return nil, err
	}
	if found && doc.Version != jsonVersion {
		return nil, fmt.Errorf("%s: unsupported version %d, this build understands %d", path, doc.Version, jsonVersion)
	}
	u := &Users{path: path, list: doc.Users}
	if err := u.validateLoaded(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := u.backfillSubTokens(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return u, nil
}

// validateLoaded catches damage from hand-editing before it becomes confusing
// behaviour later — a duplicate uuid, for instance, would silently give two people
// the same credential.
func (s *Users) validateLoaded() error {
	names := make(map[string]bool, len(s.list))
	uuids := make(map[string]bool, len(s.list))
	seen := make(map[string]bool, len(s.list))
	subTokens := make(map[string]bool, len(s.list))
	for i, u := range s.list {
		switch {
		case u.ID == "":
			return fmt.Errorf("users[%d]: id is empty", i)
		case u.Name == "":
			return fmt.Errorf("users[%d] (%s): name is empty", i, u.ID)
		case u.UUID == "":
			return fmt.Errorf("users[%d] (%s): uuid is empty", i, u.Name)
		case seen[u.ID]:
			return fmt.Errorf("users[%d]: duplicate id %q", i, u.ID)
		case names[strings.ToLower(u.Name)]:
			return fmt.Errorf("users[%d]: duplicate name %q", i, u.Name)
		case uuids[u.UUID]:
			return fmt.Errorf("users[%d]: duplicate uuid %q; two users would share one credential", i, u.UUID)
		}
		if _, err := uuid.Parse(u.UUID); err != nil {
			return fmt.Errorf("users[%d] (%s): uuid %q is not a UUID: %w", i, u.Name, u.UUID, err)
		}
		if u.SubToken != "" {
			if subTokens[u.SubToken] {
				return fmt.Errorf("users[%d]: duplicate sub_token; two users would share a subscription URL", i)
			}
			subTokens[u.SubToken] = true
		}
		seen[u.ID] = true
		names[strings.ToLower(u.Name)] = true
		uuids[u.UUID] = true
	}
	return nil
}

// backfillSubTokens gives a token to any user loaded without one, so a file written by
// an older build — or edited by hand — gets a working subscription URL rather than a
// silently broken one. Must hold the write lock.
func (s *Users) backfillSubTokens() (int, error) {
	var added int
	for i := range s.list {
		if s.list[i].SubToken != "" {
			continue
		}
		tok, err := ids.Secret(SubTokenBytes)
		if err != nil {
			return added, err
		}
		s.list[i].SubToken = tok
		added++
	}
	if added > 0 {
		if err := s.save(); err != nil {
			return added, err
		}
	}
	return added, nil
}

// Path returns the backing file.
func (s *Users) Path() string { return s.path }

// List returns a copy of the users, oldest first.
func (s *Users) List() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Not slices.Clone: it returns nil for a nil input, and this is marshalled straight
	// into a JSON response where a nil slice becomes `null` rather than `[]`. A fresh
	// node has an empty list, so that is the first thing a caller ever sees.
	return append(make([]User, 0, len(s.list)), s.list...)
}

// Get resolves a reference that may be an internal id or a display name. Names are
// matched case-insensitively so `user show Alice` works.
func (s *Users) Get(ref string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := s.indexOf(ref)
	if i < 0 {
		return nil, fmt.Errorf("user %q: %w", ref, ErrNotFound)
	}
	u := s.list[i]
	return &u, nil
}

// indexOf must be called with the lock held.
func (s *Users) indexOf(ref string) int {
	for i := range s.list {
		if s.list[i].ID == ref {
			return i
		}
	}
	for i := range s.list {
		if strings.EqualFold(s.list[i].Name, ref) {
			return i
		}
	}
	return -1
}

// GetBySubToken resolves a subscription token.
//
// Compared in constant time: this is an unauthenticated lookup on a secret, and a
// timing-dependent comparison would leak the token a character at a time.
func (s *Users) GetBySubToken(token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.list {
		if s.list[i].SubToken == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(s.list[i].SubToken), []byte(token)) == 1 {
			u := s.list[i]
			return &u, nil
		}
	}
	return nil, ErrNotFound
}

// RotateSubToken issues a new subscription token, invalidating the old URL. The user's
// UUID is untouched, so an already-configured client keeps working.
func (s *Users) RotateSubToken(ref string, now time.Time) (*User, error) {
	tok, err := ids.Secret(SubTokenBytes)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(ref)
	if i < 0 {
		return nil, fmt.Errorf("user %q: %w", ref, ErrNotFound)
	}
	before := s.list[i]
	s.list[i].SubToken = tok
	s.list[i].UpdatedAt = now.UTC().Truncate(time.Second)
	u := s.list[i]
	if err := s.save(); err != nil {
		s.list[i] = before
		return nil, err
	}
	return &u, nil
}

// CreateParams describes a new user. Zero fields take their defaults.
type CreateParams struct {
	Name       string
	UUID       string // generated when empty
	QuotaBytes int64
	ExpiresAt  *time.Time
	Disabled   bool
	Note       string
}

// Create adds a user.
func (s *Users) Create(p CreateParams, now time.Time) (*User, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if p.QuotaBytes < 0 {
		return nil, fmt.Errorf("%w: quota must not be negative, got %d", ErrInvalid, p.QuotaBytes)
	}

	id, err := ids.NewUser()
	if err != nil {
		return nil, err
	}
	subToken, err := ids.Secret(SubTokenBytes)
	if err != nil {
		return nil, err
	}
	if p.UUID == "" {
		p.UUID = uuid.NewString()
	} else if _, err := uuid.Parse(p.UUID); err != nil {
		return nil, fmt.Errorf("%w: uuid %q is not a UUID: %s", ErrInvalid, p.UUID, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if i := s.indexOf(p.Name); i >= 0 {
		return nil, fmt.Errorf("user %q: %w", p.Name, ErrConflict)
	}
	for _, u := range s.list {
		if u.UUID == p.UUID {
			return nil, fmt.Errorf("uuid %s is already used by %q: %w", p.UUID, u.Name, ErrConflict)
		}
	}

	now = now.UTC().Truncate(time.Second)
	u := User{
		ID:           id,
		Name:         p.Name,
		UUID:         p.UUID,
		Enabled:      !p.Disabled,
		QuotaBytes:   p.QuotaBytes,
		ExpiresAt:    normalizeTime(p.ExpiresAt),
		UsageResetAt: now,
		SubToken:     subToken,
		Note:         p.Note,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	s.list = append(s.list, u)
	if err := s.save(); err != nil {
		s.list = s.list[:len(s.list)-1] // keep memory consistent with disk
		return nil, err
	}
	return &u, nil
}

// Patch is a set of changes; nil fields are left alone. A pointer-per-field patch is
// what lets `user set alice --note ""` clear a note, which a plain struct could not
// express.
type Patch struct {
	Name       *string
	UUID       *string
	Enabled    *bool
	QuotaBytes *int64
	ExpiresAt  **time.Time // pointer-to-pointer: set to a nil *time.Time to clear
	Note       *string
	// DisabledReason is set by enforcement, not by operators.
	DisabledReason *string
}

// Update applies a patch and persists it.
func (s *Users) Update(ref string, p Patch, now time.Time) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(ref)
	if i < 0 {
		return nil, fmt.Errorf("user %q: %w", ref, ErrNotFound)
	}
	before := s.list[i]
	u := before

	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name must not be empty", ErrInvalid)
		}
		if j := s.indexOf(name); j >= 0 && j != i {
			return nil, fmt.Errorf("user %q: %w", name, ErrConflict)
		}
		u.Name = name
	}
	if p.UUID != nil {
		if _, err := uuid.Parse(*p.UUID); err != nil {
			return nil, fmt.Errorf("%w: uuid %q is not a UUID: %s", ErrInvalid, *p.UUID, err)
		}
		for j, other := range s.list {
			if j != i && other.UUID == *p.UUID {
				return nil, fmt.Errorf("uuid %s is already used by %q: %w", *p.UUID, other.Name, ErrConflict)
			}
		}
		u.UUID = *p.UUID
	}
	if p.QuotaBytes != nil {
		if *p.QuotaBytes < 0 {
			return nil, fmt.Errorf("%w: quota must not be negative, got %d", ErrInvalid, *p.QuotaBytes)
		}
		u.QuotaBytes = *p.QuotaBytes
	}
	if p.ExpiresAt != nil {
		u.ExpiresAt = normalizeTime(*p.ExpiresAt)
	}
	if p.Note != nil {
		u.Note = *p.Note
	}
	if p.DisabledReason != nil {
		u.DisabledReason = *p.DisabledReason
	}
	if p.Enabled != nil {
		u.Enabled = *p.Enabled
		// Re-enabling by hand clears the enforcement reason, otherwise a user
		// re-enabled after a quota trip would still read as "disabled: quota".
		if *p.Enabled && p.DisabledReason == nil {
			u.DisabledReason = ""
		}
	}

	if u == before {
		return &u, nil // nothing changed; skip the write
	}
	u.UpdatedAt = now.UTC().Truncate(time.Second)

	s.list[i] = u
	if err := s.save(); err != nil {
		s.list[i] = before
		return nil, err
	}
	return &u, nil
}

// Delete removes a user. Callers should use Store.DeleteUser so usage goes too.
func (s *Users) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(ref)
	if i < 0 {
		return fmt.Errorf("user %q: %w", ref, ErrNotFound)
	}
	removed := s.list[i]
	s.list = slices.Delete(s.list, i, i+1)
	if err := s.save(); err != nil {
		s.list = slices.Insert(s.list, i, removed)
		return err
	}
	return nil
}

// ResetUsageWindow moves a user's quota window to now, which is what makes
// `user reset-usage` re-enable someone who tripped their quota.
func (s *Users) ResetUsageWindow(ref string, now time.Time) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(ref)
	if i < 0 {
		return nil, fmt.Errorf("user %q: %w", ref, ErrNotFound)
	}
	before := s.list[i]
	u := before
	u.UsageResetAt = now.UTC().Truncate(time.Second)
	u.UpdatedAt = u.UsageResetAt
	if u.DisabledReason == "quota" {
		u.Enabled = true
		u.DisabledReason = ""
	}
	s.list[i] = u
	if err := s.save(); err != nil {
		s.list[i] = before
		return nil, err
	}
	return &u, nil
}

// Replace overwrites the whole list, for `import`.
func (s *Users) Replace(list []User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.list
	s.list = slices.Clone(list)
	if err := s.validateLoaded(); err != nil {
		s.list = before
		return err
	}
	if err := s.save(); err != nil {
		s.list = before
		return err
	}
	return nil
}

// save must be called with the write lock held.
func (s *Users) save() error {
	return writeJSONAtomic(s.path, usersDoc{Version: jsonVersion, Users: s.list})
}

// normalizeTime drops sub-second precision and forces UTC so JSON round-trips are
// stable and comparisons are not defeated by monotonic clock readings.
func normalizeTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	n := t.UTC().Truncate(time.Second)
	return &n
}
