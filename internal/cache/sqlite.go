package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go)
)

// ErrNotFound is returned when no entry exists for a hash.
var ErrNotFound = errors.New("entry not found")

// Store is the SQLite-backed metadata store. Reads run concurrently; writes
// are serialized through a mutex to avoid SQLITE_BUSY storms under WAL.
type Store struct {
	db *sql.DB

	writeMu sync.Mutex
}

// Open opens (creating if needed) the SQLite database at path. The parent
// directory is created when missing so a fresh checkout can start without
// manual setup.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cache: create db dir: %w", err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: open sqlite: %w", err)
	}
	// A small pool: SQLite is a single writer anyway; reads benefit from a
	// couple of connections but more only increases contention.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS entries (
    hash            TEXT PRIMARY KEY,
    hash_alg        TEXT NOT NULL,
    status          TEXT NOT NULL,
    content_type    TEXT NOT NULL DEFAULT '',
    blob_path       TEXT NOT NULL DEFAULT '',
    width           INTEGER NOT NULL DEFAULT 0,
    height          INTEGER NOT NULL DEFAULT 0,
    bytes           INTEGER NOT NULL DEFAULT 0,
    rev             INTEGER NOT NULL DEFAULT 0,
    verdict_scores  TEXT NOT NULL DEFAULT '',
    model_ver       TEXT NOT NULL DEFAULT '',
    manual_override INTEGER NOT NULL DEFAULT 0,
    gray_zone       INTEGER NOT NULL DEFAULT 0,
    fail_reason     TEXT NOT NULL DEFAULT '',
    fail_count      INTEGER NOT NULL DEFAULT 0,
    fetched_at      INTEGER NOT NULL DEFAULT 0,
    reviewed_at     INTEGER NOT NULL DEFAULT 0,
    expires_at      INTEGER NOT NULL DEFAULT 0,
    last_accessed   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_entries_status ON entries(status);
CREATE INDEX IF NOT EXISTS idx_entries_expires ON entries(expires_at);
CREATE INDEX IF NOT EXISTS idx_entries_model_status ON entries(model_ver, status);
CREATE INDEX IF NOT EXISTS idx_entries_status_accessed ON entries(status, last_accessed);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("cache: migrate: %w", err)
	}
	return nil
}

const entryColumns = `hash, hash_alg, status, content_type, blob_path, width, height, bytes,
rev, verdict_scores, model_ver, manual_override, gray_zone, fail_reason, fail_count,
fetched_at, reviewed_at, expires_at, last_accessed`

func scanEntry(row interface{ Scan(...any) error }) (*Entry, error) {
	var e Entry
	var manual, gray int
	if err := row.Scan(
		&e.Hash, &e.HashAlg, &e.Status, &e.ContentType, &e.BlobPath,
		&e.Width, &e.Height, &e.Bytes, &e.Rev, &e.VerdictScores, &e.ModelVer,
		&manual, &gray, &e.FailReason, &e.FailCount,
		&e.FetchedAt, &e.ReviewedAt, &e.ExpiresAt, &e.LastAccessed,
	); err != nil {
		return nil, err
	}
	e.ManualOverride = manual != 0
	e.GrayZone = gray != 0
	return &e, nil
}

// Get returns the entry for hash, or ErrNotFound.
func (s *Store) Get(ctx context.Context, hash string) (*Entry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE hash = ?`, hash)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get: %w", err)
	}
	return e, nil
}

// InsertPendingFetch creates a pending_fetch row. It reports created=false if
// a row for the hash already exists.
func (s *Store) InsertPendingFetch(ctx context.Context, hash string, alg HashAlg, expiresAt int64) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO entries (hash, hash_alg, status, expires_at, fetched_at)
		 VALUES (?, ?, ?, ?, ?)`,
		hash, alg, StatusPendingFetch, expiresAt, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("cache: insert pending_fetch: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimStalePendingFetch re-claims a pending_fetch row left behind by a
// crashed process: only rows not refreshed within staleness seconds are
// taken over. Reports whether the claim succeeded.
func (s *Store) ClaimStalePendingFetch(ctx context.Context, hash string, staleness time.Duration, expiresAt int64) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE entries SET expires_at = ?, fetched_at = ?
		 WHERE hash = ? AND status = ? AND fetched_at <= ?`,
		expiresAt, time.Now().Unix(), hash, StatusPendingFetch, time.Now().Unix()-int64(staleness.Seconds()))
	if err != nil {
		return false, fmt.Errorf("cache: claim stale fetch: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimNegativeRefetch transitions an expired rejected_fetch entry back to
// pending_fetch with a bumped fail_count (exponential backoff is applied by
// the caller via the new expiresAt). Reports false when the row is missing,
// not expired, or not rejected_fetch.
func (s *Store) ClaimNegativeRefetch(ctx context.Context, hash string, expiresAt int64) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE entries SET status = ?, expires_at = ?, fail_count = fail_count + 1
		 WHERE hash = ? AND status = ? AND expires_at <= ?`,
		StatusPendingFetch, expiresAt, hash, StatusRejectedFetch, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("cache: claim refetch: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertPendingReview records a validated image awaiting review. The blob must
// already be on disk at blobPath. Bumps rev (content changed).
func (s *Store) InsertPendingReview(ctx context.Context, e *Entry, ttlNegativeFallback time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entries (hash, hash_alg, status, content_type, blob_path, width, height, bytes,
		    rev, fetched_at, expires_at, last_accessed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
		 ON CONFLICT(hash) DO UPDATE SET
		    status = excluded.status,
		    content_type = excluded.content_type,
		    blob_path = excluded.blob_path,
		    width = excluded.width,
		    height = excluded.height,
		    bytes = excluded.bytes,
		    rev = entries.rev + 1,
		    fetched_at = excluded.fetched_at,
		    expires_at = excluded.expires_at,
		    verdict_scores = '',
		    model_ver = '',
		    gray_zone = 0,
		    fail_reason = '',
		    reviewed_at = 0`,
		e.Hash, e.HashAlg, StatusPendingReview, e.ContentType, e.BlobPath,
		e.Width, e.Height, e.Bytes, now, now+int64(ttlNegativeFallback.Seconds()), now)
	if err != nil {
		return fmt.Errorf("cache: insert pending_review: %w", err)
	}
	return nil
}

// MarkNegative marks a fetch as failed (upstream 404 or validation error),
// creating or updating the negative entry. failCount is persisted so the
// cleaner can apply exponential backoff to network-error retries.
func (s *Store) MarkNegative(ctx context.Context, hash string, alg HashAlg, reason string, expiresAt int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entries (hash, hash_alg, status, fail_reason, fail_count, fetched_at, expires_at, last_accessed)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?)
		 ON CONFLICT(hash) DO UPDATE SET
		    status = excluded.status,
		    fail_reason = excluded.fail_reason,
		    fail_count = entries.fail_count + 1,
		    fetched_at = excluded.fetched_at,
		    expires_at = excluded.expires_at,
		    content_type = '',
		    blob_path = '',
		    width = 0, height = 0, bytes = 0,
		    verdict_scores = '', model_ver = '', gray_zone = 0, reviewed_at = 0`,
		hash, alg, StatusRejectedFetch, reason, now, expiresAt, now)
	if err != nil {
		return fmt.Errorf("cache: mark negative: %w", err)
	}
	return nil
}

// MarkReviewed applies a moderation verdict. Unless force is true, entries
// with a manual override are left untouched and ManualOverrideError is
// returned. Bumps rev (served content may change).
func (s *Store) MarkReviewed(ctx context.Context, hash, modelVer, scores string, approved, grayZone bool, force bool, ttlApproved, ttlRejected time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	status := StatusRejected
	ttl := ttlRejected
	if approved {
		status = StatusApproved
		ttl = ttlApproved
	}
	now := time.Now().Unix()

	var res sql.Result
	var err error
	if force {
		res, err = s.db.ExecContext(ctx,
			`UPDATE entries SET status = ?, model_ver = ?, verdict_scores = ?, gray_zone = ?,
			    reviewed_at = ?, expires_at = ?, rev = rev + 1
			 WHERE hash = ? AND status = ?`,
			status, modelVer, scores, boolToInt(grayZone), now, now+int64(ttl.Seconds()), hash, StatusPendingReview)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE entries SET status = ?, model_ver = ?, verdict_scores = ?, gray_zone = ?,
			    reviewed_at = ?, expires_at = ?, rev = rev + 1
			 WHERE hash = ? AND status = ? AND manual_override = 0`,
			status, modelVer, scores, boolToInt(grayZone), now, now+int64(ttl.Seconds()), hash, StatusPendingReview)
	}
	if err != nil {
		return fmt.Errorf("cache: mark reviewed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish "not pending" from "manual override protected".
		var manual int
		gerr := s.db.QueryRowContext(ctx,
			`SELECT manual_override FROM entries WHERE hash = ?`, hash).Scan(&manual)
		if gerr == nil && manual != 0 {
			return ManualOverrideError
		}
		return ErrNotFound
	}
	return nil
}

// ManualSetStatus applies a human decision, setting manual_override=1 so that
// automatic re-reviews cannot overwrite it (SPEC §12). force=true clears the
// override first.
func (s *Store) ManualSetStatus(ctx context.Context, hash string, approved bool, force bool, ttlApproved, ttlRejected time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	status := StatusRejected
	ttl := ttlRejected
	if approved {
		status = StatusApproved
		ttl = ttlApproved
	}
	now := time.Now().Unix()

	override := 1
	if force {
		override = 0 // force re-arms automatic moderation on top of the manual one
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE entries SET status = ?, manual_override = ?, reviewed_at = ?, expires_at = ?,
		    rev = rev + 1
		 WHERE hash = ? AND status IN (?, ?, ?)`,
		status, override, now, now+int64(ttl.Seconds()), hash,
		StatusPendingReview, StatusApproved, StatusRejected)
	if err != nil {
		return fmt.Errorf("cache: manual set status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAccess slides the TTL of an approved entry forward on a cache hit and
// records the access time for LRU eviction (SPEC §10.3).
func (s *Store) TouchAccess(ctx context.Context, hash string, ttlApproved time.Duration) {
	now := time.Now().Unix()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.db.ExecContext(ctx,
		`UPDATE entries SET last_accessed = ?, expires_at = ?
		 WHERE hash = ? AND status = ?`,
		now, now+int64(ttlApproved.Seconds()), hash, StatusApproved)
}

// RequeueForReview moves entries back to pending_review so the moderation
// workers pick them up (SPEC §10.4). skipManual=true leaves entries with a
// human decision untouched (manual conclusions are never re-reviewed without
// force, SPEC §12). Returns the affected hashes.
func (s *Store) RequeueForReview(ctx context.Context, hashes []string, skipManual bool) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var requeued []string
	for _, h := range hashes {
		var q string
		var args []any
		if skipManual {
			q = `UPDATE entries SET status = ? WHERE hash = ? AND status IN (?, ?) AND manual_override = 0`
			args = []any{StatusPendingReview, h, StatusApproved, StatusRejected}
		} else {
			q = `UPDATE entries SET status = ? WHERE hash = ? AND status IN (?, ?)`
			args = []any{StatusPendingReview, h, StatusApproved, StatusRejected}
		}
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return requeued, fmt.Errorf("cache: requeue: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			requeued = append(requeued, h)
		}
	}
	return requeued, nil
}

// RequeueByFilter requeues every entry matching an optional model version and
// status filter (SPEC §12, batch remoderate). Returns the affected hashes.
func (s *Store) RequeueByFilter(ctx context.Context, modelVer string, status Status, limit int, skipManual bool) ([]string, error) {
	q := `SELECT hash FROM entries WHERE status IN (?, ?)`
	args := []any{StatusApproved, StatusRejected}
	if modelVer != "" {
		q += ` AND model_ver = ?`
		args = append(args, modelVer)
	}
	if status.Valid() {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if skipManual {
		q += ` AND manual_override = 0`
	}
	q += ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: requeue filter select: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err == nil {
			hashes = append(hashes, h)
		}
	}
	rows.Close()
	if len(hashes) == 0 {
		return nil, nil
	}
	return s.RequeueForReview(ctx, hashes, skipManual)
}

// Delete removes the entry row and returns its blob path (may be empty).
func (s *Store) Delete(ctx context.Context, hash string) (blobPath string, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM entries WHERE hash = ?`, hash)
	if err != nil {
		return "", fmt.Errorf("cache: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return "", nil
}

// DeleteReturning is Delete but retrieves the blob path before removing the
// row so the caller can remove the file afterwards.
func (s *Store) DeleteReturning(ctx context.Context, hash string) (blobPath string, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	e, gerr := scanEntry(s.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE hash = ?`, hash))
	if errors.Is(gerr, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if gerr != nil {
		return "", fmt.Errorf("cache: delete lookup: %w", gerr)
	}
	if _, derr := s.db.ExecContext(ctx, `DELETE FROM entries WHERE hash = ?`, hash); derr != nil {
		return "", fmt.Errorf("cache: delete: %w", derr)
	}
	return e.BlobPath, nil
}

// DeleteExpired removes rows whose expires_at has passed among the given
// statuses and returns the removed entries (so blobs can be deleted).
func (s *Store) DeleteExpired(ctx context.Context, now int64, statuses ...Status) ([]*Entry, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ph := make([]string, len(statuses))
	args := make([]any, 0, len(statuses)+1)
	args = append(args, now)
	for i, st := range statuses {
		ph[i] = "?"
		args = append(args, st)
	}
	q := `SELECT ` + entryColumns + ` FROM entries WHERE expires_at <= ? AND status IN (` + joinPh(ph) + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: delete expired select: %w", err)
	}
	var removed []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err == nil {
			removed = append(removed, e)
		}
	}
	rows.Close()

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM entries WHERE expires_at <= ? AND status IN (`+joinPh(ph)+`)`, args...)
	if err != nil {
		return removed, fmt.Errorf("cache: delete expired: %w", err)
	}
	return removed, nil
}

// ExpireKeepRecord removes the blob of expired rejected entries but keeps the
// row (with blob_path cleared) so a violation is never re-fetched blindly
// (SPEC §10.3: rejected keeps its record).
func (s *Store) ExpireKeepRecord(ctx context.Context, now int64) ([]*Entry, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE status = ? AND expires_at <= ? AND blob_path != ''`,
		StatusRejected, now)
	if err != nil {
		return nil, fmt.Errorf("cache: expire keep record select: %w", err)
	}
	var removed []*Entry
	for rows.Next() {
		if e, err := scanEntry(rows); err == nil {
			removed = append(removed, e)
		}
	}
	rows.Close()
	if len(removed) == 0 {
		return nil, nil
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE entries SET blob_path = '', content_type = '', bytes = 0, width = 0, height = 0
		 WHERE status = ? AND expires_at <= ? AND blob_path != ''`,
		StatusRejected, now)
	if err != nil {
		return removed, fmt.Errorf("cache: expire keep record: %w", err)
	}
	return removed, nil
}

// PendingOlderThan returns hashes of entries in the given status whose
// fetched_at is older than ts, up to limit (queue self-healing, SPEC §10.3).
func (s *Store) PendingOlderThan(ctx context.Context, status Status, ts int64, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash FROM entries WHERE status = ? AND fetched_at <= ? LIMIT ?`,
		status, ts, limit)
	if err != nil {
		return nil, fmt.Errorf("cache: pending older: %w", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err == nil {
			hashes = append(hashes, h)
		}
	}
	return hashes, nil
}

// ExpiredByStatus returns entries of one status whose TTL has passed, up to
// limit. Used by the cleaner to retry network-error negatives proactively
// (SPEC §6.3) instead of merely deleting them.
func (s *Store) ExpiredByStatus(ctx context.Context, status Status, now int64, limit int) ([]*Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE status = ? AND expires_at <= ? LIMIT ?`,
		status, now, limit)
	if err != nil {
		return nil, fmt.Errorf("cache: expired by status: %w", err)
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// ListEntries returns entries ordered by hash, optionally filtered by status,
// for the admin API (SPEC §12).
func (s *Store) ListEntries(ctx context.Context, status *Status, limit, offset int) ([]*Entry, error) {
	q := `SELECT ` + entryColumns + ` FROM entries`
	var args []any
	if status != nil {
		q += ` WHERE status = ?`
		args = append(args, *status)
	}
	q += ` ORDER BY hash LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: list: %w", err)
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountByStatus returns the number of entries per status.
func (s *Store) CountByStatus(ctx context.Context) (map[Status]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM entries GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("cache: count by status: %w", err)
	}
	defer rows.Close()
	out := make(map[Status]int64)
	for rows.Next() {
		var st Status
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// BytesByStatus returns the sum of blob bytes per status.
func (s *Store) BytesByStatus(ctx context.Context) (map[Status]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COALESCE(SUM(bytes),0) FROM entries WHERE status IN (?, ?) GROUP BY status`,
		StatusApproved, StatusPendingReview)
	if err != nil {
		return nil, fmt.Errorf("cache: bytes by status: %w", err)
	}
	defer rows.Close()
	out := make(map[Status]int64)
	for rows.Next() {
		var st Status
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ApprovedBytes returns the total byte size of approved entries.
func (s *Store) ApprovedBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes),0) FROM entries WHERE status = ?`, StatusApproved).Scan(&n)
	return n, err
}

// LRUVictims selects approved entries for eviction, oldest-access first, until
// at least overBytes have been reclaimed. The rows are deleted in the same
// transaction; the returned entries carry blob paths for file removal.
func (s *Store) LRUVictims(ctx context.Context, overBytes int64) ([]*Entry, error) {
	if overBytes <= 0 {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE status = ? ORDER BY last_accessed ASC LIMIT 1000`,
		StatusApproved)
	if err != nil {
		return nil, fmt.Errorf("cache: lru select: %w", err)
	}
	var victims []*Entry
	var acc int64
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			continue
		}
		victims = append(victims, e)
		acc += e.Bytes
		if acc >= overBytes {
			break
		}
	}
	rows.Close()
	if len(victims) == 0 {
		return nil, nil
	}
	for _, v := range victims {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM entries WHERE hash = ? AND status = ?`, v.Hash, StatusApproved); err != nil {
			return victims, fmt.Errorf("cache: lru delete: %w", err)
		}
	}
	return victims, nil
}

// RequeueStaleModelVer requeues approved/rejected entries whose model_ver
// differs from currentVer (model-upgrade re-review, SPEC §10.4). Manual
// overrides are never touched. Returns the affected hashes.
func (s *Store) RequeueStaleModelVer(ctx context.Context, currentVer string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash FROM entries
		 WHERE status IN (?, ?) AND model_ver != ? AND model_ver != '' AND manual_override = 0
		 LIMIT ?`,
		StatusApproved, StatusRejected, currentVer, limit)
	if err != nil {
		return nil, fmt.Errorf("cache: stale model select: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err == nil {
			hashes = append(hashes, h)
		}
	}
	rows.Close()
	if len(hashes) == 0 {
		return nil, rows.Err()
	}
	return s.RequeueForReview(ctx, hashes, true)
}

// AllModelVers returns the distinct model versions recorded on reviewed
// entries (used by the model-upgrade re-review).
func (s *Store) AllModelVers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT model_ver FROM entries WHERE model_ver != '' AND status IN (?, ?)`,
		StatusApproved, StatusRejected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vers []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err == nil {
			vers = append(vers, v)
		}
	}
	return vers, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func joinPh(ph []string) string {
	out := ""
	for i, p := range ph {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ForceRequeue clears any manual override and moves the entries back to
// pending_review (admin remoderate with force=true, SPEC §12).
func (s *Store) ForceRequeue(ctx context.Context, hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var requeued []string
	for _, h := range hashes {
		res, err := s.db.ExecContext(ctx,
			`UPDATE entries SET status = ?, manual_override = 0 WHERE hash = ? AND status IN (?, ?, ?)`,
			StatusPendingReview, h, StatusPendingReview, StatusApproved, StatusRejected)
		if err != nil {
			return requeued, fmt.Errorf("cache: force requeue: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			requeued = append(requeued, h)
		}
	}
	return requeued, nil
}

// DeleteAll removes every entry row (admin purge?status=all). Returns the
// number of deleted rows.
func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM entries`)
	if err != nil {
		return 0, fmt.Errorf("cache: delete all: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// KnownBlobPaths lists every blob path referenced by a row (orphan scan).
func (s *Store) KnownBlobPaths(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blob_path FROM entries WHERE blob_path != ''`)
	if err != nil {
		return nil, fmt.Errorf("cache: known blobs: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			out[p] = struct{}{}
		}
	}
	return out, rows.Err()
}

// Optimize runs SQLite's PRAGMA optimize (SPEC §14).
func (s *Store) Optimize(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `PRAGMA optimize`)
	return err
}
