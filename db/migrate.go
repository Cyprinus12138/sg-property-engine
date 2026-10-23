// Package db owns process-wide database helpers that don't belong to
// any single domain store — most notably the migration runner that
// every cmd/ binary wires in during boot.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// Side-effect import — registers the "pgx5" database scheme with
	// migrate.NewWithSourceInstance.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey wraps the entire RunMigrations call in a
// pg_advisory_lock so two processes booting against the same database
// (e.g. two engine replicas) cannot race each other on the migration
// table. golang-migrate already takes its own lock around individual
// migrations, but the outer lock here is cheap insurance — without
// it, a flapping deploy could see the second binary attempt to run
// migrations while the first is still applying them.
//
// Magic constant is just a random int64; persistence doesn't matter,
// only uniqueness across other advisory-lock keys in the codebase.
const advisoryLockKey int64 = 0x4D49475241544501 // "MIGRATE\x01"

// RunMigrations brings the database to the latest version embedded in
// the supplied filesystem. Idempotent: a database already at the
// highest version is a no-op (logs "already at version N").
//
// Safe to call from every binary's boot path simultaneously — the
// advisory-lock guard serialises concurrent calls across binaries.
//
// Down() is intentionally not exposed in Go — operators reset a dev
// database via the standalone migrate CLI (see migrations/README.md), so
// a guarded wrapper here would be dead code with a more dangerous blast
// radius (one stray call from a future caller deletes production data).
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	if pool == nil {
		return fmt.Errorf("db.RunMigrations: pgx pool is nil")
	}

	release, err := acquireLock(ctx, pool)
	if err != nil {
		return err
	}
	defer release()

	m, err := openMigrator(pool, fsys)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	beforeVersion, _, _ := m.Version()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db.RunMigrations: up: %w", err)
	}
	afterVersion, _, _ := m.Version()

	if beforeVersion == afterVersion {
		slog.Info("db.RunMigrations: already at version, no migrations to apply", "version", afterVersion)
	} else {
		slog.Info("db.RunMigrations: migrated", "from", beforeVersion, "to", afterVersion)
	}
	return nil
}

// acquireLock takes the outer advisory lock. We use the blocking
// pg_advisory_lock (not the try variant) because boot is allowed to
// wait — failing fast would just push the operator into a retry loop.
// Returns a release func that runs the unlock on its own background
// context so a cancelled boot ctx still releases.
func acquireLock(ctx context.Context, pool *pgxpool.Pool) (func(), error) {
	if _, err := pool.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("db: acquire advisory lock: %w", err)
	}
	return func() {
		if _, err := pool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey); err != nil {
			slog.Warn("db: release advisory lock failed", "err", err)
		}
	}, nil
}

func openMigrator(pool *pgxpool.Pool, fsys fs.FS) (*migrate.Migrate, error) {
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("db: build iofs source: %w", err)
	}
	// Hand migrate the DSN as a pgx5 URL. The migrate driver opens its
	// own connection (separate from the application pool) for the
	// migration session — it has to, because golang-migrate locks the
	// connection it uses and we don't want the pool starved by it.
	url := "pgx5://" + stripDSNScheme(pool.Config().ConnString())
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return nil, fmt.Errorf("db: open migrator: %w", err)
	}
	return m, nil
}

// stripDSNScheme normalises a DSN like "postgres://…" or
// "postgresql://…" to bare "user:pw@host/db?…" so we can prepend the
// pgx5:// scheme golang-migrate expects without doubling up.
func stripDSNScheme(dsn string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(dsn, prefix) {
			return dsn[len(prefix):]
		}
	}
	return dsn
}
