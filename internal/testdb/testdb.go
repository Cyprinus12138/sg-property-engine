//go:build smoke

// Package testdb is the shared bootstrap for `-tags=smoke` integration
// tests that hit a real Postgres. It centralises the give-me-a-clean-
// database dance that every smoke test would otherwise repeat.
//
// Run locally:
//
//	docker run --rm -d --name sgpe-pg \
//	  -e POSTGRES_PASSWORD=test -p 55433:5432 postgis/postgis:16-3.4
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:55433/postgres?sslmode=disable \
//	  go test -tags=smoke -count=1 ./...
//
// # Why a template database
//
// Re-running every migration per test is correct but slow, because the
// init migration creates PostGIS — an extension that registers thousands
// of functions. Instead the migrations run once into a template database,
// and each test gets `CREATE DATABASE ... TEMPLATE`, which Postgres serves
// as a file copy. Setup stops scaling with schema complexity.
//
// Two things fall out of that. Tests get a whole database each rather
// than sharing one schema, so they cannot see each other's rows at all —
// including the seed rows migrations insert, which a TRUNCATE-based reset
// would have silently wiped. And with no shared state left, `-p 1` is no
// longer needed: packages can run in parallel.
//
// The template is named after a hash of the migration files. Edit a
// migration and the name changes, so a stale template is never reused —
// which matters on the CI runner, where the Postgres volume outlives the
// job.
//
// The build tag keeps the helper out of the default `go test ./...` path
// — pure unit tests must not import it (and can't, without the tag).
package testdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cyprinus12138/sg-property-engine/db"
	"github.com/Cyprinus12138/sg-property-engine/migrations"
)

// templateLockKey namespaces the advisory lock that serialises template
// creation. Arbitrary, but must not collide with the lock
// db.RunMigrations takes — that one is held *inside* the template while
// this one is held around it, so sharing a key would self-deadlock.
const templateLockKey = 0x70775F746D706C

// testDBPrefix marks the throwaway per-test databases so leftovers from a
// killed run can be recognised and dropped.
const testDBPrefix = "sgpe_t_"

// runPrefix is testDBPrefix plus an id shared by every test binary in this
// CI run. Cleanup can then tell "left behind by a run that died" from
// "in use right now by a package running alongside me" — with packages
// parallel, a prefix match alone would have one process dropping another
// process's live database out from under it.
//
// Local runs share the "local" id, so a previous local run's leftovers
// survive until the next one. Harmless: a dev box is not the disk that
// keeps filling.
func runPrefix() string {
	id := strings.TrimSpace(os.Getenv("GITHUB_RUN_ID"))
	if id == "" {
		id = "local"
	}
	return testDBPrefix + id + "_"
}

var (
	templateOnce sync.Once
	templateName string
	templateErr  error

	dbCounter atomic.Int64
)

// Open returns a pgx pool against a database of this test's very own,
// cloned from a template that already has every migration applied.
//
// Skips the test (not fails) when TEST_DATABASE_URL is unset — so
// `go test ./...` without the env var still passes on a dev box.
//
// The pool is closed and the database dropped on test cleanup.
func Open(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping smoke test")
	}
	ctx := context.Background()

	templateOnce.Do(func() { templateName, templateErr = ensureTemplate(ctx, adminDSN) })
	if templateErr != nil {
		t.Fatalf("testdb.Open: prepare template: %v", templateErr)
	}

	name := fmt.Sprintf("%s%d_%d", runPrefix(), os.Getpid(), dbCounter.Add(1))
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("testdb.Open: connect: %v", err)
	}
	defer admin.Close(ctx)

	// CREATE DATABASE cannot run in a transaction and cannot be
	// parameterised, hence the Sanitize. Both names are ours — a pid and
	// a content hash — but quoting them keeps the rule "identifiers get
	// sanitised" unbroken for whoever edits this next.
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s",
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{templateName}.Sanitize()))
	if err != nil {
		t.Fatalf("testdb.Open: clone template: %v", err)
	}

	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		t.Fatalf("testdb.Open: build dsn: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb.Open: pgxpool.New: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropDatabase(context.Background(), adminDSN, name)
	})
	return pool
}

// ensureTemplate creates the migrated template if it is not there yet and
// returns its name.
//
// The advisory lock covers other test binaries, not just other goroutines
// — with packages running in parallel, several processes reach this at
// the same moment on a cold database. Whoever loses the race waits, then
// finds the template already present.
func ensureTemplate(ctx context.Context, adminDSN string) (string, error) {
	name, err := templateNameFor(migrations.FS)
	if err != nil {
		return "", err
	}
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(templateLockKey)); err != nil {
		return "", fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(templateLockKey))
	}()

	var exists bool
	if err := admin.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return "", fmt.Errorf("look up template: %w", err)
	}
	if exists {
		dropStaleTestDatabases(ctx, admin, name)
		return name, nil
	}

	if _, err := admin.Exec(ctx,
		"CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", fmt.Errorf("create template: %w", err)
	}

	// Scoped so the pool is definitely closed before anyone clones from
	// this database — CREATE DATABASE ... TEMPLATE is refused while another
	// session is connected to the template.
	if err := func() error {
		dsn, err := withDatabase(adminDSN, name)
		if err != nil {
			return err
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return fmt.Errorf("pgxpool.New: %w", err)
		}
		defer pool.Close()
		if err := db.RunMigrations(ctx, pool, migrations.FS); err != nil {
			return fmt.Errorf("RunMigrations: %w", err)
		}
		return nil
	}(); err != nil {
		// A half-migrated template would be reused forever, since the name
		// only changes when the migrations do. Drop it so the next run
		// starts clean.
		dropDatabaseWith(context.Background(), admin, name)
		return "", err
	}

	dropStaleTestDatabases(ctx, admin, name)

	// Confirm what we are about to hand out actually exists, so a cleanup
	// race surfaces here with a pointed message rather than as "template
	// database does not exist" from whichever test runs first.
	if err := admin.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return "", fmt.Errorf("verify template: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("template %s vanished between creation and use", name)
	}
	return name, nil
}

// templateNameFor derives a database name from the content of every
// migration, so editing one produces a different template rather than
// silently reusing the old schema.
func templateNameFor(fsys fs.FS) (string, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return "", fmt.Errorf("glob migrations: %w", err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no migrations found")
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", n, err)
		}
		_, _ = h.Write([]byte(n))
		_, _ = h.Write(b)
	}
	// 12 hex chars: collision-free in practice, and short enough to leave
	// room under Postgres's 63-byte identifier limit.
	return "sgpe_tmpl_" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// withDatabase rewrites the database component of a connection URL.
func withDatabase(dsn, dbname string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	u.Path = "/" + dbname
	return u.String(), nil
}

// dropStaleTestDatabases clears per-test databases a killed run left
// behind. The CI runner reuses its Postgres volume between jobs, so
// without this they accumulate until the disk fills — which has happened
// here before, with anonymous service-container volumes.
func dropStaleTestDatabases(ctx context.Context, admin *pgx.Conn, keep string) {
	// starts_with, not LIKE. "_" is a single-character wildcard in LIKE,
	// so the obvious pattern "sgpe_t_%" also matches "sgpe_tmpl_<hash>" — this
	// function deleted the template it had just built, and every clone
	// then failed with 3D000. `keep` is the second belt: whatever the
	// matching rule becomes, the live template is never a candidate.
	//
	// The NOT starts_with(runPrefix) is the third: databases belonging to
	// this run are in use by packages running in parallel with us.
	rows, err := admin.Query(ctx,
		`SELECT datname FROM pg_database
		  WHERE starts_with(datname, $1)
		    AND NOT starts_with(datname, $2)
		    AND datname <> $3`,
		testDBPrefix, runPrefix(), keep)
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		dropDatabaseWith(ctx, admin, n)
	}
}

func dropDatabase(ctx context.Context, adminDSN, name string) {
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return
	}
	defer admin.Close(ctx)
	dropDatabaseWith(ctx, admin, name)
}

// dropDatabaseWith uses FORCE (Postgres 13+) so a connection a test left
// open cannot keep the database alive and leak it into the next run.
func dropDatabaseWith(ctx context.Context, admin *pgx.Conn, name string) {
	_, _ = admin.Exec(ctx,
		"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// Truncate empties the named tables in one go with RESTART IDENTITY so
// sequences reset too. Useful for tests that need a clean global count
// mid-binary.
//
// Less necessary than it was — each test now starts from its own copy of
// the template — but still the right tool for resetting between phases
// inside a single test.
//
// CASCADE follows FK chains — pass the parent tables and let the
// children fall.
func Truncate(t *testing.T, pool *pgxpool.Pool, tables ...string) {
	t.Helper()
	if len(tables) == 0 {
		return
	}
	quoted := make([]string, 0, len(tables))
	for _, tbl := range tables {
		quoted = append(quoted, pgx.Identifier{tbl}.Sanitize())
	}
	stmt := "TRUNCATE " + strings.Join(quoted, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := pool.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("testdb.Truncate: %v", err)
	}
}
