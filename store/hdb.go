package store

import (
	"context"
	"crypto/md5"
	"fmt"
	"time"
)

// HDBResaleRow is one HDB resale transaction, in the shape we store.
type HDBResaleRow struct {
	Month          time.Time `json:"month"`
	Town           string    `json:"town"`
	Block          string    `json:"block"`
	StreetName     string    `json:"street_name"`
	FlatType       string    `json:"flat_type"`
	StoreyRange    string    `json:"storey_range"`
	FloorAreaSqm   float64   `json:"floor_area_sqm"`
	FlatModel      string    `json:"flat_model"`
	LeaseCommence  int       `json:"lease_commence"`
	RemainingLease string    `json:"remaining_lease"`
	ResalePrice    float64   `json:"resale_price"`
}

// HashHDBResale computes the idempotent upsert key for an HDB resale row.
// Natural key is (month, town, block, street_name, flat_type, storey_range,
// floor_area_sqm, lease_commence, resale_price) — enough to disambiguate
// multiple sales of the same unit in the same month.
func HashHDBResale(r HDBResaleRow) []byte {
	h := md5.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%.2f|%d|%.2f",
		r.Month.Format("2006-01"), r.Town, r.Block, r.StreetName,
		r.FlatType, r.StoreyRange, r.FloorAreaSqm, r.LeaseCommence, r.ResalePrice)
	return h.Sum(nil)
}

// UpsertHDBResale bulk-inserts/updates HDB resale rows. Returns (rowsIn,
// rowsUpserted). "rowsUpserted" counts rows that caused an actual insert;
// conflicting rows are no-ops.
func (s *Store) UpsertHDBResale(ctx context.Context, rows []HDBResaleRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	upserted := 0
	const stmt = `
		INSERT INTO hdb_resale
		   (month, town, block, street_name, flat_type, storey_range,
		    floor_area_sqm, flat_model, lease_commence, remaining_lease,
		    resale_price, source_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (source_hash) DO NOTHING`
	for _, r := range rows {
		tag, err := tx.Exec(ctx, stmt,
			r.Month, r.Town, r.Block, r.StreetName, r.FlatType, r.StoreyRange,
			r.FloorAreaSqm, r.FlatModel, r.LeaseCommence, r.RemainingLease,
			r.ResalePrice, HashHDBResale(r))
		if err != nil {
			return 0, 0, err
		}
		upserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(rows), upserted, nil
}

// RebuildHDBBlocksFromResale derives the hdb_blocks master from
// hdb_resale. Inserts master rows with NULL coordinates; geocoding is a
// separate step (GeoIngestor). Mirrors RebuildProjectsFromURA.
//
// Runs after UpsertHDBPropertyInfo as a fallback: PI is the authoritative
// source for HDB blocks (~10.7k residential blocks), but rare
// renamed/demolished entries (e.g. MACPHERSON LANE 82) survive only in
// resale data, so we still seed any missing rows from there.
func (s *Store) RebuildHDBBlocksFromResale(ctx context.Context) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO hdb_blocks (street_name, block)
		SELECT DISTINCT street_name, block FROM hdb_resale
		ON CONFLICT (street_name, block) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// HDBPropertyInfoRow is one block from data.gov.sg's HDB Property
// Information dataset. Pointer fields are nullable since CKAN returns
// every column as text and a row may legitimately have empty values.
type HDBPropertyInfoRow struct {
	StreetName    string
	Block         string
	YearCompleted *int
	MaxFloorLvl   *int
	TotalDwelling *int
	Residential   bool
	TownCode      string // bldg_contract_town, e.g. "BD", "KWN", "TAP"
}

// UpsertHDBPropertyInfo bulk-upserts block-level facts from the HDB
// Property Information feed. The geocoding columns (postal/lat/lng/geom/
// planning_area/name) are not touched — those are owned by GeoIngestor
// and would be wiped on every refresh otherwise.
//
// Returns (rowsIn, rowsInserted). rowsInserted counts new blocks added;
// existing blocks have their year_completed / max_floor_lvl /
// total_dwelling_units / residential refreshed in place.
func (s *Store) UpsertHDBPropertyInfo(ctx context.Context, rows []HDBPropertyInfoRow) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	// `xmax = 0` on the returned row distinguishes a fresh INSERT from an
	// ON-CONFLICT UPDATE — RowsAffected alone counts both as 1.
	const stmt = `
		INSERT INTO hdb_blocks
		   (street_name, block, year_completed, max_floor_lvl,
		    total_dwelling_units, residential, town_code)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''))
		ON CONFLICT (street_name, block) DO UPDATE SET
		    year_completed       = EXCLUDED.year_completed,
		    max_floor_lvl        = EXCLUDED.max_floor_lvl,
		    total_dwelling_units = EXCLUDED.total_dwelling_units,
		    residential          = EXCLUDED.residential,
		    town_code            = EXCLUDED.town_code
		RETURNING (xmax = 0) AS inserted`
	inserted := 0
	for _, r := range rows {
		var newRow bool
		if err := tx.QueryRow(ctx, stmt,
			r.StreetName, r.Block, r.YearCompleted, r.MaxFloorLvl,
			r.TotalDwelling, r.Residential, r.TownCode,
		).Scan(&newRow); err != nil {
			return 0, 0, err
		}
		if newRow {
			inserted++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(rows), inserted, nil
}

// CountBlocksNeedingGeocode returns how many hdb_blocks rows still need geocoding.
func (s *Store) CountBlocksNeedingGeocode(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM hdb_blocks WHERE geom IS NULL`).Scan(&n)
	return n, err
}

// DistinctBlocksNeedingGeocode returns (street_name, block) tuples from
// hdb_blocks that don't yet have a resolved point. Rows with a NULL geom
// include both never-attempted blocks and previously-failed ones — both
// should be retried next run.
func (s *Store) DistinctBlocksNeedingGeocode(ctx context.Context, limit int) ([][2]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT street_name, block
		  FROM hdb_blocks
		 WHERE geom IS NULL
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var s, b string
		if err := rows.Scan(&s, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{s, b})
	}
	return out, rows.Err()
}

// SetHDBBlockGeo writes geo columns for an existing hdb_blocks row.
// lat/lng of 0,0 is treated as "failed this attempt" — geom stays NULL
// so the row will be retried on the next geo run. The name field stores
// the HDB estate/project name extracted from OneMap's BUILDING field.
func (s *Store) SetHDBBlockGeo(ctx context.Context, street, block, postal string, lat, lng float64, planningArea, name string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE hdb_blocks SET
		    name          = NULLIF($3,''),
		    postal        = NULLIF($4,''),
		    lat           = NULLIF($5::float8, 0),
		    lng           = NULLIF($6::float8, 0),
		    geom          = CASE WHEN $5::float8 = 0 AND $6::float8 = 0 THEN NULL
		                         ELSE ST_SetSRID(ST_MakePoint($6::float8, $5::float8), 4326)::geography END,
		    planning_area = NULLIF($7,''),
		    geocoded_at   = now()
		  WHERE street_name = $1 AND block = $2`,
		street, block, name, postal, lat, lng, planningArea)
	return err
}

// MaxHDBMonth returns the latest month present in hdb_resale, or zero if empty.
// Used by the incremental ingestor to bound its fetch window.
func (s *Store) MaxHDBMonth(ctx context.Context) (time.Time, error) {
	var t *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT MAX(month) FROM hdb_resale`).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}
