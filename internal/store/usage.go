package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo, so the image stays static
)

// BucketSeconds is the width of a usage bucket. Hourly is fine-grained enough to
// draw a daily graph and coarse enough that 90 days of history is ~2k rows per user.
const BucketSeconds = 3600

// Usage is the SQLite-backed traffic history. It is the only store that needs a
// database: it is append-heavy and every read is an aggregate.
type Usage struct {
	db *sql.DB
}

// OpenUsage opens (creating if needed) the stats database and migrates it forward.
func OpenUsage(path string) (*Usage, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// WAL so the collector's writes never block an API read; busy_timeout so a
	// concurrent statement waits instead of failing with SQLITE_BUSY.
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": {
			"journal_mode(WAL)",
			"busy_timeout(5000)",
			"synchronous(NORMAL)",
		},
	}.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: writes are serialised anyway, and this removes lock
	// contention as a category of bug.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	u := &Usage{db: db}
	if err := u.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return u, nil
}

// Close releases the database.
func (u *Usage) Close() error { return u.db.Close() }

// migrations are applied in order; PRAGMA user_version records progress.
// Append only — never edit a released entry or deployments will skip it.
var migrations = []string{
	`
CREATE TABLE usage (
  user_id TEXT    NOT NULL,
  bucket  INTEGER NOT NULL,
  up      INTEGER NOT NULL DEFAULT 0,
  down    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, bucket)
) WITHOUT ROWID;

CREATE INDEX usage_bucket ON usage(bucket);
`,
}

func (u *Usage) migrate(ctx context.Context) error {
	var version int
	if err := u.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("stats schema version %d is newer than this build understands (%d); downgrading is not supported",
			version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := u.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA takes no bind parameters, and i+1 is not user input.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: set user_version: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: commit: %w", i+1, err)
		}
	}
	return nil
}

// Bucket returns the bucket t belongs to: the start of its UTC hour.
func Bucket(t time.Time) int64 {
	return t.UTC().Unix() / BucketSeconds * BucketSeconds
}

// Delta is a traffic increment for one user.
type Delta struct {
	UserID string
	Up     int64
	Down   int64
}

// Add folds deltas into the bucket containing at. Counters are added rather than
// replaced because the collector drains sing-box's counters on every poll, so each
// delta is new traffic since the last one.
func (u *Usage) Add(ctx context.Context, at time.Time, deltas []Delta) error {
	if len(deltas) == 0 {
		return nil
	}
	bucket := Bucket(at)

	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO usage (user_id, bucket, up, down) VALUES (?, ?, ?, ?)
ON CONFLICT(user_id, bucket) DO UPDATE SET up = up + excluded.up, down = down + excluded.down`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range deltas {
		if d.Up == 0 && d.Down == 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, d.UserID, bucket, d.Up, d.Down); err != nil {
			return fmt.Errorf("record usage for %s: %w", d.UserID, err)
		}
	}
	return tx.Commit()
}

// TotalSince sums a user's traffic from the bucket containing since onwards.
//
// The lower bound is the *bucket* containing since, not since itself, because a
// bucket is indivisible: traffic is only attributed to the hour, not to a moment
// within it. A quota window that starts mid-hour therefore includes that whole hour.
func (u *Usage) TotalSince(ctx context.Context, userID string, since time.Time) (up, down int64, err error) {
	row := u.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(up), 0), COALESCE(SUM(down), 0)
FROM usage WHERE user_id = ? AND bucket >= ?`, userID, Bucket(since))
	if err := row.Scan(&up, &down); err != nil {
		return 0, 0, fmt.Errorf("total usage for %s: %w", userID, err)
	}
	return up, down, nil
}

// Total sums a user's entire recorded history.
func (u *Usage) Total(ctx context.Context, userID string) (up, down int64, err error) {
	row := u.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(up), 0), COALESCE(SUM(down), 0) FROM usage WHERE user_id = ?`, userID)
	if err := row.Scan(&up, &down); err != nil {
		return 0, 0, fmt.Errorf("total usage for %s: %w", userID, err)
	}
	return up, down, nil
}

// Totals returns every user's lifetime traffic in one query, for list endpoints.
func (u *Usage) Totals(ctx context.Context) (map[string]Point, error) {
	rows, err := u.db.QueryContext(ctx, `
SELECT user_id, COALESCE(SUM(up), 0), COALESCE(SUM(down), 0) FROM usage GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Point{}
	for rows.Next() {
		var id string
		var p Point
		if err := rows.Scan(&id, &p.Up, &p.Down); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// Point is a traffic total, optionally at a point in time.
type Point struct {
	// Bucket is the start of the interval, zero when the point is not a time series.
	Bucket time.Time `json:"bucket,omitzero"`
	Up     int64     `json:"up"`
	Down   int64     `json:"down"`
}

// Series returns a user's traffic between from and to, grouped into intervals of
// width. Empty intervals are omitted rather than zero-filled; a caller drawing a
// graph knows the range it asked for.
func (u *Usage) Series(ctx context.Context, userID string, from, to time.Time, width time.Duration) ([]Point, error) {
	secs := int64(width / time.Second)
	if secs < BucketSeconds {
		secs = BucketSeconds
	}
	// Snap to a whole number of buckets so a "day" is 24 clean hours rather than a
	// window that slices one of them.
	secs = secs / BucketSeconds * BucketSeconds

	rows, err := u.db.QueryContext(ctx, `
SELECT bucket / ? * ? AS slot, COALESCE(SUM(up), 0), COALESCE(SUM(down), 0)
FROM usage
WHERE user_id = ? AND bucket >= ? AND bucket < ?
GROUP BY slot
ORDER BY slot`, secs, secs, userID, Bucket(from), Bucket(to)+BucketSeconds)
	if err != nil {
		return nil, fmt.Errorf("usage series for %s: %w", userID, err)
	}
	defer rows.Close()

	// Non-nil even with no rows: this goes straight into a JSON response, and a nil
	// slice marshals as `null` rather than `[]`. Every user has no traffic at first, so
	// the empty case is the common one, not an edge case.
	out := make([]Point, 0)
	for rows.Next() {
		var slot int64
		var p Point
		if err := rows.Scan(&slot, &p.Up, &p.Down); err != nil {
			return nil, err
		}
		p.Bucket = time.Unix(slot, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteUser drops a user's entire history.
func (u *Usage) DeleteUser(ctx context.Context, userID string) error {
	_, err := u.db.ExecContext(ctx, `DELETE FROM usage WHERE user_id = ?`, userID)
	return err
}

// Prune deletes buckets older than before and returns how many rows went.
func (u *Usage) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := u.db.ExecContext(ctx, `DELETE FROM usage WHERE bucket < ?`, Bucket(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Compact reclaims the space Prune leaves behind and empties the write-ahead log.
//
// Prune's DELETEs return pages to SQLite's free list, not to the filesystem, so without
// this stats.db never shrinks. VACUUM rewrites the whole database and needs room for a
// second copy of it, so run it on a timer rather than per request.
func (u *Usage) Compact(ctx context.Context) error {
	// Not in a transaction: VACUUM cannot run inside one.
	//
	// Before the checkpoint, not after: VACUUM's rewrite is a write, so in WAL mode it
	// lands in the -wal, and checkpointing first would only refill it.
	if _, err := u.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}

	// TRUNCATE rather than the default PASSIVE: only TRUNCATE resets the -wal to zero
	// bytes. The pragma reports failure in its result row instead of as an error, so
	// busy has to be checked or a no-op looks like success.
	var busy, walFrames, checkpointed int
	if err := u.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &walFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint wal: database busy, -wal left as it was")
	}
	return nil
}

// Row is one stored bucket, for export and import.
type Row struct {
	UserID string `json:"user_id"`
	Bucket int64  `json:"bucket"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
}

// Export returns every bucket, ordered so a dump is reproducible.
func (u *Usage) Export(ctx context.Context) ([]Row, error) {
	rows, err := u.db.QueryContext(ctx, `SELECT user_id, bucket, up, down FROM usage ORDER BY user_id, bucket`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil even with no rows, for the same reason as Series: this ends up in a JSON
	// dump, where a nil slice is `null` and an empty one is `[]`.
	out := make([]Row, 0)
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.UserID, &r.Bucket, &r.Up, &r.Down); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Import replaces all history with the given rows.
func (u *Usage) Import(ctx context.Context, rows []Row) error {
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM usage`); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO usage (user_id, bucket, up, down) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.UserID, r.Bucket, r.Up, r.Down); err != nil {
			return fmt.Errorf("import usage row for %s: %w", r.UserID, err)
		}
	}
	return tx.Commit()
}
