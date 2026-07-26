package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DumpVersion is the format version of an export.
const DumpVersion = 1

// Dump is a whole deployment's state in one document: users, tokens and usage
// history.
//
// This exists so moving or backing up a deployment is a single command that works
// while the service is running. Copying the data directory by hand is not equivalent:
// stats.db is SQLite in WAL mode, so a live file copy can capture a torn database
// unless the -wal and -shm files come with it, in exactly the right order. Going
// through this struct sidesteps that entirely.
//
// A dump contains user UUIDs and, unless excluded, the server's Reality private key.
// Treat the file as a secret.
type Dump struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exported_at"`

	// Identity is the Reality keypair. Included by default, because a restore without
	// it produces a working server that none of the restored users' clients can reach —
	// a silent, baffling failure. Its presence is why a dump is a secret.
	Identity *Identity `json:"identity,omitempty"`

	Users  []User  `json:"users"`
	Tokens []Token `json:"tokens"`

	// Usage is omitted by an export that only needs to move accounts.
	Usage []Row `json:"usage,omitempty"`
}

// ExportOptions controls what an export includes.
type ExportOptions struct {
	// ExcludeIdentity leaves the Reality private key out. Only sensible when the
	// destination deliberately wants a different identity, since the restored users'
	// existing clients will then all fail.
	ExcludeIdentity bool

	// IncludeUsage carries traffic history across. Off by default because history is
	// usually the bulky, least interesting part of a move.
	IncludeUsage bool
	// IncludeTokens carries API credentials across. Off by default: the destination
	// is a different host, and minting fresh tokens there is both easy and safer.
	IncludeTokens bool
}

// Export gathers the current state.
func (s *Store) Export(ctx context.Context, now time.Time, opts ExportOptions) (*Dump, error) {
	d := &Dump{
		Version:    DumpVersion,
		ExportedAt: now.UTC().Truncate(time.Second),
		Users:      s.Users.List(),
	}
	if !opts.ExcludeIdentity {
		id := s.Identity.Get()
		d.Identity = &id
	}
	if opts.IncludeTokens {
		d.Tokens = s.Tokens.List()
	}
	if opts.IncludeUsage {
		rows, err := s.Usage.Export(ctx)
		if err != nil {
			return nil, fmt.Errorf("export usage: %w", err)
		}
		d.Usage = rows
	}
	return d, nil
}

// Encode writes the dump as indented JSON. Deliberately not named WriteTo: that
// name belongs to io.WriterTo, whose signature returns a byte count.
func (d *Dump) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// ReadDump decodes and sanity-checks a dump.
func ReadDump(r io.Reader) (*Dump, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var d Dump
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("parse dump: %w", err)
	}
	if d.Version != DumpVersion {
		return nil, fmt.Errorf("dump version %d, this build understands %d", d.Version, DumpVersion)
	}

	// Usage rows reference users by id. A row whose user is absent would be dead
	// weight that never gets pruned by a user deletion, so refuse the import rather
	// than silently carrying orphans forward.
	known := make(map[string]bool, len(d.Users))
	for _, u := range d.Users {
		known[u.ID] = true
	}
	for _, r := range d.Usage {
		if !known[r.UserID] {
			return nil, fmt.Errorf("dump has usage for unknown user %q", r.UserID)
		}
	}
	return &d, nil
}

// ErrNotEmpty is returned by Import when the target already holds data.
var ErrNotEmpty = errors.New("data directory is not empty")

// Import replaces the current state with a dump.
//
// It refuses a non-empty target unless force is set: importing over a live
// deployment silently discards every user on it, which is not something to do by
// accident. Sections absent from the dump are left untouched — importing a dump with no
// identity keeps the destination's own key.
func (s *Store) Import(ctx context.Context, d *Dump, force bool) error {
	if !force {
		if n := len(s.Users.List()); n > 0 {
			return fmt.Errorf("%w: %d existing users would be discarded; pass --force to overwrite", ErrNotEmpty, n)
		}
	}

	// Identity first: if it fails, nothing else has been touched yet.
	if d.Identity != nil {
		if err := s.Identity.Replace(*d.Identity); err != nil {
			return fmt.Errorf("import identity: %w", err)
		}
	}
	if err := s.Users.Replace(d.Users); err != nil {
		return fmt.Errorf("import users: %w", err)
	}
	if d.Tokens != nil {
		if err := s.Tokens.Replace(d.Tokens); err != nil {
			return fmt.Errorf("import tokens: %w", err)
		}
	}
	if d.Usage != nil {
		if err := s.Usage.Import(ctx, d.Usage); err != nil {
			return fmt.Errorf("import usage: %w", err)
		}
	}
	return nil
}
