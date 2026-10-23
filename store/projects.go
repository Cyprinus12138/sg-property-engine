package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RebuildProjectsFromURA derives the projects master from ura_transactions.
// Uses ON CONFLICT DO UPDATE that preserves existing geo fields so geocoding
// survives across rebuilds. property_types is aggregated as a distinct array.
//
// Columns deliberately NOT touched on conflict (rebuild-safe):
//
//	postal, lat, lng, geom, planning_area, nearby_mrt
func (s *Store) RebuildProjectsFromURA(ctx context.Context) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO projects
		     (name, street, district, property_types, tenure, market_segment,
		      first_txn_date, last_txn_date, txn_count, updated_at)
		SELECT project,
		       MAX(street),
		       MAX(district),
		       ARRAY(SELECT DISTINCT pt FROM unnest(array_agg(property_type)) AS pt WHERE pt IS NOT NULL ORDER BY pt),
		       MAX(tenure),
		       MAX(market_segment),
		       MIN(contract_date),
		       MAX(contract_date),
		       COUNT(*),
		       now()
		  FROM ura_transactions
		 GROUP BY project
		ON CONFLICT (name) DO UPDATE SET
		    street         = EXCLUDED.street,
		    district       = EXCLUDED.district,
		    property_types = EXCLUDED.property_types,
		    tenure         = EXCLUDED.tenure,
		    market_segment = EXCLUDED.market_segment,
		    first_txn_date = EXCLUDED.first_txn_date,
		    last_txn_date  = EXCLUDED.last_txn_date,
		    txn_count      = EXCLUDED.txn_count,
		    updated_at     = now()`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// UpsertProjectsFromPipeline merges URA pipeline projects into the master
// `projects` table so new launches that haven't accumulated transactions yet
// are still addressable by name (search, anchors, geocoding).
//
// Filters out URA's generic placeholder labels (rows where the developer
// hasn't published a project name) — those would create non-clickable
// master rows and pollute search.
//
// On conflict we preserve every column the txn-derived rebuild also
// preserves (postal, lat, lng, geom, planning_area, txn_count) and
// only fill in pipeline-derivable fields when they're currently NULL —
// so a project that has since started transacting keeps the txn-derived
// values and isn't clobbered by a pipeline refresh.
//
// Returns (inserted, updated, skipped_placeholder).
func (s *Store) UpsertProjectsFromPipeline(ctx context.Context, rows []URAPipelineRow) (int, int, int, error) {
	if len(rows) == 0 {
		return 0, 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback(ctx)

	// Two-step per row to dedupe across case differences:
	//   1. If a row already exists whose normalised name matches, update it
	//      in place and skip the insert (prevents creating "Belgravia Ace"
	//      alongside the existing "BELGRAVIA ACE").
	//   2. Otherwise insert a new row, name UPPERCASED to match the
	//      ura_transactions convention (RebuildProjectsFromURA writes CAPS).
	//      This is what keeps a project single-rowed across its lifecycle:
	//      when it later starts transacting, the txn-derived rebuild's
	//      ON CONFLICT (name) collides on the exact PK and updates in place
	//      instead of inserting a case-twin ("Hudson Place Residences" +
	//      "HUDSON PLACE RESIDENCES"). Any case-sensitive PK conflict here is
	//      still treated as a plain update.
	const findStmt = `
		SELECT name FROM projects
		 WHERE regexp_replace(lower(name), '[^a-z0-9]+', '', 'g')
		     = regexp_replace(lower($1),   '[^a-z0-9]+', '', 'g')
		 ORDER BY txn_count DESC NULLS LAST
		 LIMIT 1`
	const insertStmt = `
		INSERT INTO projects
		     (name, street, district, property_types, market_segment,
		      txn_count, updated_at)
		VALUES (UPPER($1), NULLIF($2,''), NULLIF($3,''), $4, NULLIF($5,''), 0, now())
		ON CONFLICT (name) DO UPDATE SET
		    street         = COALESCE(projects.street, EXCLUDED.street),
		    district       = COALESCE(projects.district, EXCLUDED.district),
		    property_types = COALESCE(projects.property_types, EXCLUDED.property_types),
		    market_segment = COALESCE(projects.market_segment, EXCLUDED.market_segment),
		    updated_at     = now()`
	const updateStmt = `
		UPDATE projects SET
		    street         = COALESCE(projects.street,         NULLIF($2,'')),
		    district       = COALESCE(projects.district,       NULLIF($3,'')),
		    property_types = COALESCE(projects.property_types, $4),
		    market_segment = COALESCE(projects.market_segment, NULLIF($5,'')),
		    updated_at     = now()
		 WHERE name = $1`
	var inserted, updated, skipped int
	for _, r := range rows {
		if IsPipelinePlaceholder(r.Project) {
			skipped++
			continue
		}
		types := pipelinePropertyTypes(r)
		var existing string
		findErr := tx.QueryRow(ctx, findStmt, r.Project).Scan(&existing)
		switch {
		case findErr == nil:
			// Existing row with same normalised name — update it in place.
			if _, err := tx.Exec(ctx, updateStmt, existing, r.Street, r.District, types, r.MarketSegment); err != nil {
				return 0, 0, 0, err
			}
			updated++
		case errors.Is(findErr, pgx.ErrNoRows):
			// No normalised match — insert a fresh row.
			if _, err := tx.Exec(ctx, insertStmt, r.Project, r.Street, r.District, types, r.MarketSegment); err != nil {
				return 0, 0, 0, err
			}
			inserted++
		default:
			return 0, 0, 0, findErr
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, err
	}
	return inserted, updated, skipped, nil
}

// pipelinePropertyTypes derives the property_types[] array from a
// URA pipeline row's per-type unit counts (noOfApartment, noOfCondo,
// noOfTerrace, noOfDetachedHouse, noOfSemiDetached). Returns nil when
// no counts are present so the upsert leaves the column NULL (the
// COALESCE on conflict then keeps any txn-derived value intact).
func pipelinePropertyTypes(r URAPipelineRow) []string {
	if len(r.Raw) == 0 {
		return nil
	}
	var out []string
	add := func(label, key string) {
		if asInt(r.Raw[key]) > 0 {
			out = append(out, label)
		}
	}
	add("Apartment", "noOfApartment")
	add("Condominium", "noOfCondo")
	add("Terrace", "noOfTerrace")
	add("Detached House", "noOfDetachedHouse")
	add("Semi-Detached House", "noOfSemiDetached")
	return out
}

func asInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

// IsPipelinePlaceholder reports whether a URA pipeline `project` field
// is one of the generic category labels URA uses when the developer
// hasn't published a real name (e.g. "Service apartments", "Mixed
// development", "Mixed-use development"). Exported so API consumers can
// apply the same predicate client-side.
//
// Match is on the first token, where token boundary is space, hyphen, or
// slash — so "Mixed-use", "Residential/service", and "Mixed development"
// all reduce to a single first word ("mixed", "residential", "mixed")
// before the lookup.
func IsPipelinePlaceholder(project string) bool {
	trimmed := strings.TrimSpace(project)
	if trimmed == "" {
		return false
	}
	first := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ' ' || r == '-' || r == '/'
	})
	if len(first) == 0 {
		return false
	}
	switch strings.ToLower(first[0]) {
	case "service", "mixed", "executive", "landed", "condominium",
		"apartment", "apartments", "townhouse", "townhouses",
		"detached", "semi", "terrace", "cluster", "residential",
		"shophouse", "shophouses", "redevelopment", "development":
		return true
	}
	return false
}

// CountProjectsNeedingGeocode returns how many projects still need geocoding.
func (s *Store) CountProjectsNeedingGeocode(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE geom IS NULL`).Scan(&n)
	return n, err
}

// ProjectsNeedingGeocode returns (name, street) for projects that don't
// yet have a resolved point. geom IS NULL covers both never-attempted
// projects and previously-failed ones, so both retry each run.
func (s *Store) ProjectsNeedingGeocode(ctx context.Context, limit int) ([][2]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT name, COALESCE(street, '') FROM projects
		 WHERE geom IS NULL
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var name, street string
		if err := rows.Scan(&name, &street); err != nil {
			return nil, err
		}
		out = append(out, [2]string{name, street})
	}
	return out, rows.Err()
}

func (s *Store) SetProjectGeo(ctx context.Context, name, postal string, lat, lng float64, planningArea string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE projects SET
		    postal        = NULLIF($2,''),
		    lat           = NULLIF($3::float8, 0),
		    lng           = NULLIF($4::float8, 0),
		    geom          = CASE WHEN $3::float8 = 0 AND $4::float8 = 0 THEN NULL
		                         ELSE ST_SetSRID(ST_MakePoint($4::float8, $3::float8), 4326)::geography END,
		    planning_area = NULLIF($5,''),
		    updated_at    = now()
		  WHERE name = $1`,
		name, postal, lat, lng, planningArea)
	return err
}

// ProjectBuildingRow is a single building within a private project.
type ProjectBuildingRow struct {
	Project      string
	BlkNo        string
	RoadName     string
	BuildingName string
	Postal       string
	Lat          float64
	Lng          float64
}

// UpsertProjectBuildings inserts buildings for a project. Conflicts on
// (project, postal) are updated so re-runs refresh the data.
func (s *Store) UpsertProjectBuildings(ctx context.Context, rows []ProjectBuildingRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	const stmt = `
		INSERT INTO project_buildings
		    (project, blk_no, road_name, building_name, postal, lat, lng, geom)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
		        CASE WHEN $6::float8 = 0 AND $7::float8 = 0 THEN NULL
		             ELSE ST_SetSRID(ST_MakePoint($7::float8, $6::float8), 4326)::geography END)
		ON CONFLICT (project, postal) DO UPDATE SET
		    blk_no        = EXCLUDED.blk_no,
		    road_name     = EXCLUDED.road_name,
		    building_name = EXCLUDED.building_name,
		    lat           = EXCLUDED.lat,
		    lng           = EXCLUDED.lng,
		    geom          = EXCLUDED.geom,
		    discovered_at = now()`
	upserted := 0
	for _, r := range rows {
		tag, err := s.Pool.Exec(ctx, stmt,
			r.Project, r.BlkNo, r.RoadName, r.BuildingName, r.Postal, r.Lat, r.Lng)
		if err != nil {
			return upserted, err
		}
		upserted += int(tag.RowsAffected())
	}
	return upserted, nil
}
