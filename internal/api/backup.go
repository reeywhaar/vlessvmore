package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

// BackupPath is the only route the backup listener serves.
const BackupPath = "/backup"

// The archive's two top-level directories, named after the two mounts a deployment has,
// so restoring is extracting it over them:
//
//	docker compose down
//	tar xzf vlessvmore-20260729_031500.tgz -C /srv/vlessvmore
//	docker compose up -d
const (
	ArchiveConfigDir = "config"
	ArchiveDataDir   = "data"
)

// BackupHandler serves the backup listener.
//
// No bearer token, no CORS and no antibunsteal padding, unlike Handler. Those defend
// api_listen, which a reverse proxy exposes to the internet; this listener is private to
// the container network and its one client is the backup sidecar.
func (s *Server) BackupHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.notFound)
	mux.HandleFunc("GET "+BackupPath, s.backup)
	return s.logRequests(mux)
}

// BackupFilename is the archive's name, timestamped so a backup store can keep a series.
func BackupFilename(t time.Time) string {
	return "vlessvmore-" + t.UTC().Format("20060102_150405") + ".tgz"
}

// backup writes the deployment's config and data directories as a gzipped tar.
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	// One reading, so the entry timestamps and the filename cannot straddle a second.
	now := time.Now()

	archive, err := s.buildArchive(r.Context(), now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backup: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.Itoa(archive.Len()))
	w.Header().Set("Content-Disposition", `attachment; filename="`+BackupFilename(now)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(archive.Bytes())
}

type archiveEntry struct {
	name string
	data []byte
}

// buildArchive renders the tgz in memory rather than streaming it: a tar header needs its
// entry's size up front, and a failure partway through a streamed 200 would look like a
// success while producing a truncated archive.
func (s *Server) buildArchive(ctx context.Context, now time.Time) (*bytes.Buffer, error) {
	files, err := s.store.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot the data directory: %w", err)
	}

	entries := make([]archiveEntry, 0, len(files)+1)
	for _, f := range files {
		entries = append(entries, archiveEntry{path.Join(ArchiveDataDir, f.Name), f.Data})
	}

	// Best effort. config.json is operator-authored and `vlessvmore init` regenerates it,
	// so an unreadable one is worth a log line but not a failed backup.
	if s.configPath != "" {
		raw, err := os.ReadFile(s.configPath)
		if err != nil {
			s.log.Warn("backup is missing the config file", "path", s.configPath, "error", err)
		} else {
			name := path.Join(ArchiveConfigDir, filepath.Base(s.configPath))
			entries = append(entries, archiveEntry{name, raw})
		}
	}

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		// 0600 throughout: the archive carries the Reality private key and every user
		// UUID, so an extracted copy should not be readable by anyone else. No directory
		// entries, so extracting does not re-chmod a data directory that already exists.
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Mode:     0o600,
			Size:     int64(len(e.data)),
			ModTime:  now.UTC().Truncate(time.Second),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write %s header: %w", e.name, err)
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, fmt.Errorf("write %s: %w", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return &out, nil
}
