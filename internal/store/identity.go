package store

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"vlessvmore/internal/reality"
)

// Identity is the server's Reality key material.
//
// It lives in the data directory rather than in config.json because it is generated,
// not authored: an operator never chooses these values, they are produced once and then
// must never change. Keeping them out of config.json means config.json holds no secrets
// at all and can be committed, templated or handed around freely.
//
// The public key is deliberately absent. It is derived from the private key on demand,
// so the two halves cannot drift apart.
type Identity struct {
	Version    int       `json:"version"`
	PrivateKey string    `json:"private_key"`
	ShortID    string    `json:"short_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// IdentityStore owns identity.json.
type IdentityStore struct {
	path string
	mu   sync.RWMutex
	id   Identity
	// generated records whether this process created the identity, so callers can warn
	// about it.
	generated bool
}

// OpenIdentity loads identity.json, generating a fresh identity if there is none.
//
// Generating on first run is what makes a deployment zero-configuration. It is also the
// one genuinely dangerous moment in this package: if the data directory is lost or a
// fresh volume is mounted over it, a new identity appears and **every existing client
// stops connecting**, with a failure that looks like a misconfiguration rather than a
// missing file. Callers should surface Generated() prominently, and
// Store.WarnOnUnexpectedIdentity does exactly that.
func OpenIdentity(path string) (*IdentityStore, error) {
	var id Identity
	found, err := readJSON(path, &id)
	if err != nil {
		return nil, err
	}

	s := &IdentityStore{path: path, id: id}
	if found {
		if id.Version != jsonVersion {
			return nil, fmt.Errorf("%s: unsupported version %d, this build understands %d", path, id.Version, jsonVersion)
		}
		if err := validateIdentity(&id); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return s, nil
	}

	fresh, err := NewIdentity(time.Now())
	if err != nil {
		return nil, err
	}
	s.id = *fresh
	s.generated = true
	if err := s.save(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewIdentity generates a fresh Reality identity.
func NewIdentity(now time.Time) (*Identity, error) {
	priv, err := reality.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	shortID, err := reality.GenerateShortID()
	if err != nil {
		return nil, err
	}
	return &Identity{
		Version:    jsonVersion,
		PrivateKey: priv,
		ShortID:    shortID,
		CreatedAt:  now.UTC().Truncate(time.Second),
	}, nil
}

func validateIdentity(id *Identity) error {
	if _, err := reality.DecodePrivateKey(id.PrivateKey); err != nil {
		return fmt.Errorf("private_key: %w", err)
	}
	if err := reality.ValidateShortID(id.ShortID); err != nil {
		return fmt.Errorf("short_id: %w", err)
	}
	return nil
}

// Path returns the backing file.
func (s *IdentityStore) Path() string { return s.path }

// Generated reports whether this process created the identity rather than loading it.
func (s *IdentityStore) Generated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generated
}

// Get returns a copy of the identity.
func (s *IdentityStore) Get() Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

// Reload re-reads identity.json from disk.
//
// This exists because `vlessvmore identity set` runs as a separate process and writes the
// file directly, so a long-running daemon would otherwise keep serving the key it loaded
// at startup. Without this, rotating and then reloading appears to succeed while the old
// key stays live — and the change only lands at the next restart, at a moment nobody
// expects.
//
// A missing file is not an error and leaves the in-memory identity alone: if
// identity.json disappears under a running server, continuing with the key we already
// have is far better than inventing a new one. A corrupt file *is* an error, so the
// caller can decline to render rather than guess.
func (s *IdentityStore) Reload() error {
	var id Identity
	found, err := readJSON(s.path, &id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if id.Version != jsonVersion {
		return fmt.Errorf("%s: unsupported version %d, this build understands %d", s.path, id.Version, jsonVersion)
	}
	if err := validateIdentity(&id); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
	return nil
}

// PublicKey derives the client-facing Reality public key. Never stored.
func (s *IdentityStore) PublicKey() (string, error) {
	return reality.PublicKey(s.Get().PrivateKey)
}

// Replace installs a different identity, for adopting an existing deployment's keys or
// restoring a backup. Every client's `pbk=` changes unless the new key matches the old,
// so callers must make the consequences obvious.
func (s *IdentityStore) Replace(id Identity) error {
	if id.Version == 0 {
		id.Version = jsonVersion
	}
	if id.CreatedAt.IsZero() {
		id.CreatedAt = time.Now().UTC().Truncate(time.Second)
	}
	if err := validateIdentity(&id); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.id
	s.id = id
	if err := s.save(); err != nil {
		s.id = before
		return err
	}
	s.generated = false
	return nil
}

// save must be called with the write lock held, or during construction.
func (s *IdentityStore) save() error { return writeJSONAtomic(s.path, s.id) }

// WarnOnUnexpectedIdentity logs about a freshly generated identity.
//
// A brand new deployment generating a key is unremarkable. Generating one when users
// already exist is not: it means the identity file was lost while the user list
// survived, and those users' clients will all fail until they are re-issued links. That
// is worth an error in the log rather than a shrug, because the symptom on the client
// side gives no hint of the cause.
func (s *Store) WarnOnUnexpectedIdentity(log *slog.Logger) {
	if !s.Identity.Generated() {
		return
	}
	users := len(s.Users.List())
	if users == 0 {
		log.Info("generated a new Reality identity", "path", s.Identity.Path())
		return
	}
	log.Error("generated a NEW Reality identity even though users already exist — "+
		"identity.json was probably lost while users.json survived. "+
		"Every existing client will fail to connect until it is given an updated link. "+
		"If you have a backup, restore it with `vlessvmore identity set` or `vlessvmore import` and restart.",
		"path", s.Identity.Path(), "users", users)
}
