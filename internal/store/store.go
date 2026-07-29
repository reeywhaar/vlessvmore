// Package store persists everything the manager owns.
//
// The split is deliberate:
//
//   - users.json, tokens.json and identity.json are plain, pretty-printed JSON files.
//     They are small, they change rarely, and an operator can read or repair them with a
//     text editor — which matters when something has gone wrong at 3am.
//   - stats.db is SQLite, because usage is the only data here that is high-volume,
//     append-heavy and needs real aggregation (sum over a range, roll up by day).
//
// config.json remains the source of truth for how the server is reachable; nothing
// in this package touches it.
//
// The JSON files are read once at startup and owned by the process from then on.
// Hand-editing them while the service runs is not detected: edit, then restart.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultDir is the data directory inside the container. It should be a volume.
const DefaultDir = "/var/lib/vlessvmore"

// DirEnv overrides DefaultDir.
const DirEnv = "VLESSVMORE_DATA_DIR"

// File names within the data directory.
const (
	UsersFile    = "users.json"
	TokensFile   = "tokens.json"
	IdentityFile = "identity.json"
	StatsFile    = "stats.db"
	// RenderedConfig is where the generated sing-box config is written. It lives in
	// the data dir because it is a build artifact of (config.json + users), not
	// state, and is rewritten on every change.
	RenderedConfig = "sing-box.json"
)

// Store aggregates the three backing stores.
type Store struct {
	dir      string
	Users    *Users
	Tokens   *Tokens
	Identity *IdentityStore
	Usage    *Usage
}

// Open prepares dir and opens all three stores. Missing JSON files are treated as
// empty, so a fresh volume needs no seeding.
func Open(dir string) (*Store, error) {
	users, err := OpenUsers(filepath.Join(dir, UsersFile))
	if err != nil {
		return nil, err
	}
	tokens, err := OpenTokens(filepath.Join(dir, TokensFile))
	if err != nil {
		return nil, err
	}
	identity, err := OpenIdentity(filepath.Join(dir, IdentityFile))
	if err != nil {
		return nil, err
	}
	usage, err := OpenUsage(filepath.Join(dir, StatsFile))
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, Users: users, Tokens: tokens, Identity: identity, Usage: usage}, nil
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// RenderedConfigPath is where the generated sing-box config belongs.
func (s *Store) RenderedConfigPath() string { return filepath.Join(s.dir, RenderedConfig) }

// Close releases the SQLite handle. The JSON stores hold no resources.
func (s *Store) Close() error { return s.Usage.Close() }

// SnapshotFile is one file from a data directory snapshot. Name is relative to the data
// directory.
type SnapshotFile struct {
	Name string
	Data []byte
}

// Snapshot reads the data directory as a set of files that can be written back over a
// stopped deployment's data directory.
//
// The JSON files are read from disk rather than re-marshalled from memory, so a restored
// file is byte-identical to the one that was backed up. Each is written atomically, so a
// concurrent change yields either the old or the new file and never half of one; no
// invariant spans two of them, so reading them one at a time is enough.
//
// stats.db goes through Usage.Snapshot, being the one file a plain copy can tear.
func (s *Store) Snapshot(ctx context.Context) ([]SnapshotFile, error) {
	tmp, err := os.MkdirTemp("", "vlessvmore-snapshot")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	statsPath := filepath.Join(tmp, StatsFile)
	if err := s.Usage.Snapshot(ctx, statsPath); err != nil {
		return nil, err
	}
	stats, err := os.ReadFile(statsPath)
	if err != nil {
		return nil, err
	}

	// Identity first: it is the file that must survive, so it should be the first thing
	// out and the first thing back in.
	var out []SnapshotFile
	for _, name := range []string{IdentityFile, UsersFile, TokensFile, RenderedConfig} {
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if errors.Is(err, os.ErrNotExist) {
			// tokens.json before the first token is minted, sing-box.json before the
			// first render: absent is a normal state here, not a failure.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, SnapshotFile{Name: name, Data: data})
	}
	return append(out, SnapshotFile{Name: StatsFile, Data: stats}), nil
}

// DeleteUser removes a user and their usage history together. Usage lives in a
// different backing store than users, so there are no foreign keys to do this for
// us; forgetting it would leak rows that no longer belong to anyone.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	u, err := s.Users.Get(id)
	if err != nil {
		return err
	}
	if err := s.Usage.DeleteUser(ctx, u.ID); err != nil {
		return fmt.Errorf("delete usage for %s: %w", u.ID, err)
	}
	return s.Users.Delete(u.ID)
}

// UsedBytes is a user's traffic since their last usage reset.
func (s *Store) UsedBytes(ctx context.Context, u *User) (int64, error) {
	up, down, err := s.Usage.TotalSince(ctx, u.ID, u.UsageResetAt)
	return up + down, err
}

// OverQuota reports whether a user has reached their quota. A zero quota is
// unlimited.
func (s *Store) OverQuota(ctx context.Context, u *User) (bool, error) {
	if u.QuotaBytes <= 0 {
		return false, nil
	}
	used, err := s.UsedBytes(ctx, u)
	if err != nil {
		return false, err
	}
	return used >= u.QuotaBytes, nil
}

// ActiveUsers returns the users that belong in the rendered sing-box config:
// enabled, not expired, and not over quota. Everyone else simply is not offered a
// credential, which is how a disabled user stops being able to connect.
func (s *Store) ActiveUsers(ctx context.Context, now time.Time) ([]User, error) {
	all := s.Users.List()
	active := make([]User, 0, len(all))
	for _, u := range all {
		if !u.Enabled || u.IsExpired(now) {
			continue
		}
		over, err := s.OverQuota(ctx, &u)
		if err != nil {
			return nil, err
		}
		if over {
			continue
		}
		active = append(active, u)
	}
	return active, nil
}
