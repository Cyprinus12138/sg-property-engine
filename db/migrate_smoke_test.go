//go:build smoke

// Smoke test for db.RunMigrations end-to-end against a real PostGIS
// database. Skipped unless built with `-tags=smoke` and TEST_DATABASE_URL
// is set — pure unit tests in this repo must not depend on services.
//
// Run locally:
//
//	docker run --rm -d --name sgpe-pg -e POSTGRES_PASSWORD=test -p 55433:5432 postgis/postgis:16-3.4
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:55433/postgres?sslmode=disable \
//	  go test -tags=smoke ./db/...
package db_test

import (
	"context"
	"testing"

	"github.com/Cyprinus12138/sg-property-engine/db"
	"github.com/Cyprinus12138/sg-property-engine/internal/testdb"
	"github.com/Cyprinus12138/sg-property-engine/migrations"
)

func TestRunMigrations_IdempotentAndComplete(t *testing.T) {
	pool := testdb.Open(t) // template already migrated; a second run must be a no-op
	ctx := context.Background()

	if err := db.RunMigrations(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}

	for _, table := range []string{
		"sync_runs", "hdb_resale", "hdb_blocks",
		"ura_transactions", "ura_rentals", "ura_pipeline", "ura_developer_sales",
		"projects", "project_buildings", "pois", "geo_cache",
		"cea_salespersons", "cea_transactions",
	} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s missing after migrate (exists=%v err=%v)", table, exists, err)
		}
	}

	// PostGIS must be live: geography columns and ST_DWithin depend on it.
	var postgisVersion string
	if err := pool.QueryRow(ctx, `SELECT postgis_version()`).Scan(&postgisVersion); err != nil {
		t.Fatalf("postgis_version(): %v", err)
	}
	if postgisVersion == "" {
		t.Fatal("empty postgis_version()")
	}
}
