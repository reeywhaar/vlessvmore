package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBackupDir(t *testing.T) {
	// A volume mounted at the default path shows up as the directory existing, and the
	// image creates no such directory, so nothing to mount over means nothing to keep.
	t.Run("mounted volume keeps local copies", func(t *testing.T) {
		mounted := t.TempDir()
		t.Setenv("BACKUP_DIR", "")

		dir, keepLocal, err := resolveBackupDir(mounted, "token")
		if err != nil {
			t.Fatal(err)
		}
		if dir != mounted || !keepLocal {
			t.Errorf("resolved %q keepLocal=%v, want %q keepLocal=true", dir, keepLocal, mounted)
		}
	})

	t.Run("no directory uploads without keeping a copy", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "backups")
		t.Setenv("BACKUP_DIR", "")

		dir, keepLocal, err := resolveBackupDir(missing, "token")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)

		if keepLocal {
			t.Error("keepLocal is true with no volume mounted")
		}
		if dir == missing {
			t.Errorf("resolved %q, want a temp directory", dir)
		}
		if !isDir(dir) {
			t.Errorf("temp directory %q was not created", dir)
		}
		if isDir(missing) {
			t.Errorf("%q was created after all", missing)
		}
	})

	// Nothing mounted and nothing to upload to means the run has nowhere to put the
	// archive it is about to fetch, which is a misconfiguration rather than a mode.
	t.Run("no directory and no token is an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "backups")
		t.Setenv("BACKUP_DIR", "")

		if _, _, err := resolveBackupDir(missing, ""); err == nil {
			t.Error("resolved a directory with nowhere to keep or send the archive")
		}
	})

	// An explicit path is honoured whether or not it exists yet, so a deployment that
	// wants local copies somewhere else does not have to pre-create the directory.
	t.Run("explicit BACKUP_DIR is created and kept", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "archives")
		t.Setenv("BACKUP_DIR", want)

		dir, keepLocal, err := resolveBackupDir("/nonexistent", "token")
		if err != nil {
			t.Fatal(err)
		}
		if dir != want || !keepLocal {
			t.Errorf("resolved %q keepLocal=%v, want %q keepLocal=true", dir, keepLocal, want)
		}
		if !isDir(want) {
			t.Errorf("%q was not created", want)
		}
	})
}
