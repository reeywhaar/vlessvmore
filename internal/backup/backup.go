// Package backup takes a copy of the deployment and hands it to something that keeps copies.
//
// # Why this program does it rather than something beside it
//
// It used to serve the archive instead: a listener of its own on :3000, answering GET /backup
// with no token at all, and a sidecar container of ours fetching it on a loop. That worked and
// cost an image to build, publish and patch — and the loop could only ever be a timer, because
// a process outside this one cannot know whether anything has been written since the last copy.
//
// Pushing inverts both. The image is somebody else's now — backio-agent takes an archive at
// POST /backup and handles naming, encryption, upload and retention — and the decision of
// *when* moves in here, where the answer is knowable. Nothing is sent while nothing has
// changed. It also retires the argument for the extra listener: nothing fetches any more, so
// there is no unauthenticated route left to keep off the reverse proxy's port.
//
// # What "changed" means
//
// Everything in the archive except stats.db. That is config.json, the Reality keypair, the
// users and the API token hashes — what somebody set, and the part that cannot be rebuilt
// from anywhere. stats.db is what the collector measured: it grows every polling interval on
// a server nobody is administering, so letting it decide would make every deployment a
// deployment that uploads on a timer, which is the thing being replaced. It travels *in* the
// archive when the mode says so; it does not decide whether one is made.
//
// sing-box.json is in the digest too, and is the one entry that is neither. It is rendered
// from the others, so it changes exactly when they do and never on its own — redundant rather
// than wrong, and cheaper to leave in than to explain an exception for.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"vlessvmore/internal/store"
)

// The archive's two top-level directories, named after the two mounts a deployment has, so
// restoring is extracting it over them:
//
//	docker compose down
//	tar xzf vlessvmore-20260729_031500.tgz -C /srv/vlessvmore
//	docker compose up -d
const (
	ArchiveConfigDir = "config"
	ArchiveDataDir   = "data"
)

// Filename is the archive's name, timestamped so a backup store can keep a series.
func Filename(t time.Time) string {
	return "vlessvmore-" + t.UTC().Format("20060102_150405") + ".tgz"
}

// Archive is one copy of the deployment, and the digest that says which copy it is.
type Archive struct {
	Body []byte
	// Digest identifies everything in the archive except stats.db.
	//
	// Taken from the files rather than from the tarball, because a tarball carries a
	// timestamp in its gzip header and in every entry — two archives of one unchanged
	// deployment differ, which is exactly the question this is asked to answer.
	Digest []byte
}

// Source is what an archive is built from.
type Source struct {
	Store *store.Store
	// ConfigPath is where config.json was loaded from, so the archive can carry the file
	// itself rather than a re-marshalled copy of the parsed struct. Empty when unknown.
	ConfigPath string
	Log        *slog.Logger
}

type entry struct {
	name string
	data []byte
}

// files gathers what goes in the archive, in the order it goes in.
//
// config.json first, then the data directory. Reading it is best effort: it is
// operator-authored and `vlessvmore init` regenerates it, so an unreadable one is worth a log
// line but not a failed backup — the keypair underneath it is the part that cannot be retyped.
func (s Source) files(ctx context.Context, stats bool) ([]entry, error) {
	var out []entry
	if s.ConfigPath != "" {
		raw, err := os.ReadFile(s.ConfigPath)
		if err != nil {
			s.Log.Warn("backup is missing the config file", "path", s.ConfigPath, "error", err)
		} else {
			out = append(out, entry{path.Join(ArchiveConfigDir, filepath.Base(s.ConfigPath)), raw})
		}
	}

	snapshot, err := s.Store.Snapshot(ctx, stats)
	if err != nil {
		return nil, fmt.Errorf("snapshot the data directory: %w", err)
	}
	for _, f := range snapshot {
		out = append(out, entry{path.Join(ArchiveDataDir, f.Name), f.Data})
	}
	return out, nil
}

// digest hashes everything but stats.db. Names go in beside the bytes, so moving content
// between two files cannot leave the digest where it was.
func digest(files []entry) []byte {
	statsEntry := path.Join(ArchiveDataDir, store.StatsFile)
	h := sha256.New()
	for _, f := range files {
		if f.name == statsEntry {
			continue
		}
		fmt.Fprintf(h, "%s %d\n", f.name, len(f.data))
		h.Write(f.data)
	}
	return h.Sum(nil)
}

// Probe is the digest alone, without building an archive.
//
// The cheap half of the question, and the reason Snapshot takes a flag: this reads a few
// kilobytes of JSON straight off disk, where building an archive would run `VACUUM INTO` over
// the whole traffic history to find out whether anybody had touched a user.
func (s Source) Probe(ctx context.Context) ([]byte, error) {
	files, err := s.files(ctx, false)
	if err != nil {
		return nil, err
	}
	return digest(files), nil
}

// Build renders the deployment as a gzipped tar, in memory.
//
// A tar header needs its entry's size up front, so the files have to be in hand anyway. See
// [store.Store.Snapshot] for why stats.db is `VACUUM INTO` rather than a copy.
func (s Source) Build(ctx context.Context, stats bool, now time.Time) (*Archive, error) {
	files, err := s.files(ctx, stats)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		// 0600 throughout: the archive carries the Reality private key and every user
		// UUID, so an extracted copy should not be readable by anyone else. No directory
		// entries, so extracting does not re-chmod a data directory that already exists.
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.name,
			Mode:     0o600,
			Size:     int64(len(f.data)),
			ModTime:  now.UTC().Truncate(time.Second),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write %s header: %w", f.name, err)
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return &Archive{Body: out.Bytes(), Digest: digest(files)}, nil
}

// maxReply is how much of a rejection is read back.
//
// Enough to carry a sentence from the other end and not enough for a server answering with a
// page of HTML to put a page of HTML in the log.
const maxReply = 2 << 10

// Push posts one archive to a backup agent.
//
// Multipart, with the file under `backup` and the name beside it, which is what backio-agent
// documents. It is a POST rather than a PUT because the agent keeps a series: each one is a
// new archive rather than a replacement for the last. No credential travels with it — the
// token for the remote is the agent's, which is the point of the agent.
func Push(ctx context.Context, client *http.Client, url, name string, body []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("backup", name)
	if err != nil {
		return err
	}
	if _, err := part.Write(body); err != nil {
		return err
	}
	if err := w.WriteField("name", name); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// What it said, not just that it said no. "The agent answered 500" is a fact
		// nobody can act on; the body underneath is where the rejected token and the
		// unreachable remote live.
		said, _ := io.ReadAll(io.LimitReader(res.Body, maxReply))
		return fmt.Errorf("the backup agent answered %s: %s", res.Status, bytes.TrimSpace(said))
	}
	return nil
}
