package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// jsonVersion is the format version stamped into users.json and tokens.json, so a
// future release can recognise an old layout instead of misreading it.
const jsonVersion = 1

// readJSON decodes path into dst. A missing file is not an error: it leaves dst
// untouched and reports false, which is what a fresh volume looks like.
//
// Unknown fields are rejected. These files are meant to be hand-edited, and silently
// dropping a misspelled "quota_byte" would be worse than refusing to start.
func readJSON(path string, dst any) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

// writeJSONAtomic writes src to path as pretty-printed JSON via a temp file and a
// rename, so a crash or a full disk can never leave a half-written user list behind.
// Readers see either the old file or the new one.
func writeJSONAtomic(path string, src any) error {
	b, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// Same directory as the target, so the rename is atomic rather than a
	// cross-device copy.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	// These files hold user UUIDs and token hashes: readable by owner only.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// fsync before the rename: without it a power loss can leave the renamed file
	// present but empty.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}

	// Persist the directory entry too, so the rename itself survives a crash.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
