package store

import (
	"context"
	"errors"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
)

// SyncRun mirrors a row in sync_runs.
type SyncRun struct {
	ID           int64      `json:"id"`
	Source       string     `json:"source"`
	Mode         string     `json:"mode"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Status       string     `json:"status"`
	RowsIn       int        `json:"rows_in"`
	RowsUpserted int        `json:"rows_upserted"`
	Stage        string     `json:"stage,omitempty"`
	Progress     float64    `json:"progress"`
	Error        string     `json:"error,omitempty"`
}

const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusError   = "error"
	// StatusSkipped marks a run that started (sync_runs row inserted)
	// but never executed because another process won the advisory lock.
	// Distinct from "error" so the run history doesn't show false
	// failures when a duplicate engine instance races for the lock.
	StatusSkipped = "skipped"

	ModeIncremental = "incremental"
	ModeBackfill    = "backfill"
)

// ErrLocked is returned when another run for the same source is in progress.
var ErrLocked = errors.New("sync: another run is already active for this source")

// ProgressReporter lets a job report its current stage and completion
// percentage (0-100) for the in-flight sync_run. Always safe to call — a nil
// reporter is treated as a no-op by Report.
type ProgressReporter func(stage string, progress float64)

// Report invokes r if non-nil. Lets callers write `report.Report(...)` without
// a nil check at every call site.
func (r ProgressReporter) Report(stage string, progress float64) {
	if r != nil {
		r(stage, progress)
	}
}

// advisoryKey hashes a source name into an int64 so pg_try_advisory_lock can
// be called without tracking a central key registry.
func advisoryKey(source string) int64 {
	h := fnv.New64a()
	h.Write([]byte("sgpe.sync."))
	h.Write([]byte(source))
	return int64(h.Sum64())
}

// WithLock runs fn while holding a Postgres session-level advisory lock keyed
// on the source. Two engine processes against the same DB cannot run the
// same job concurrently; neither can a manual trigger and a cron tick.
func (s *Store) WithLock(ctx context.Context, source string, fn func(ctx context.Context) error) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	key := advisoryKey(source)
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		return err
	}
	if !got {
		return ErrLocked
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
	}()

	return fn(ctx)
}

func (s *Store) StartRun(ctx context.Context, source, mode string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO sync_runs (source, mode, status)
		VALUES ($1, $2, $3)
		RETURNING id`, source, mode, StatusRunning).Scan(&id)
	return id, err
}

// UpdateProgress records the current stage and completion percentage (0–100)
// for an in-flight run. Safe to call repeatedly; callers should throttle to
// avoid hammering the DB (e.g. once per batch).
func (s *Store) UpdateProgress(ctx context.Context, id int64, stage string, progress float64) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE sync_runs
		   SET stage    = NULLIF($2, ''),
		       progress = $3
		 WHERE id = $1`, id, stage, progress)
	return err
}

func (s *Store) FinishRun(ctx context.Context, id int64, rowsIn, rowsUpserted int, runErr error) error {
	status := StatusSuccess
	errStr := ""
	if runErr != nil {
		status = StatusError
		errStr = runErr.Error()
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE sync_runs
		   SET finished_at   = now(),
		       status        = $2,
		       rows_in       = $3,
		       rows_upserted = $4,
		       error         = NULLIF($5, ''),
		       progress      = CASE WHEN $2 = 'success' THEN 100 ELSE progress END
		 WHERE id = $1`, id, status, rowsIn, rowsUpserted, errStr)
	return err
}

// MarkSkipped finalises a run that never executed because the advisory
// lock was already held (i.e. another process / goroutine got there
// first). Distinct from FinishRun(...err) so the run history doesn't
// report this as a failure.
func (s *Store) MarkSkipped(ctx context.Context, id int64, reason string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sync_runs
		   SET finished_at = now(),
		       status      = $2,
		       error       = NULLIF($3, '')
		 WHERE id = $1`, id, StatusSkipped, reason)
	return err
}

func (s *Store) GetRun(ctx context.Context, id int64) (*SyncRun, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, source, mode, started_at, finished_at, status,
		       rows_in, rows_upserted, COALESCE(stage, ''),
		       COALESCE(progress, 0), COALESCE(error, '')
		  FROM sync_runs WHERE id = $1`, id)
	return scanRun(row)
}

// SourcesWithSuccess returns the set of sources that have at least one
// successful sync_runs row. Used at boot to decide which jobs still need a
// first-time backfill.
func (s *Store) SourcesWithSuccess(ctx context.Context) (map[string]bool, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT source
		  FROM sync_runs
		 WHERE status = $1`, StatusSuccess)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, err
		}
		out[src] = true
	}
	return out, rows.Err()
}

// LatestRuns returns the most recent run per source.
func (s *Store) LatestRuns(ctx context.Context) ([]SyncRun, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT ON (source)
		       id, source, mode, started_at, finished_at, status,
		       rows_in, rows_upserted, COALESCE(stage, ''),
		       COALESCE(progress, 0), COALESCE(error, '')
		  FROM sync_runs
		 ORDER BY source, started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (*SyncRun, error) {
	var r SyncRun
	var finished *time.Time
	if err := row.Scan(&r.ID, &r.Source, &r.Mode, &r.StartedAt, &finished, &r.Status,
		&r.RowsIn, &r.RowsUpserted, &r.Stage, &r.Progress, &r.Error); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.FinishedAt = finished
	return &r, nil
}
