package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Station is one entry in the nearby_mrt JSONB array on projects.
// Codes / Lines are populated by the LTA TrainStationCodes ingest; both
// are empty when the LTA key is absent or the station hasn't been
// enriched yet — the rest of the fields still render usefully.
//
// DistanceM is the metres-along-the-ground (straight-line) from the
// query point to the closest exit of this station. WalkMin = distance_m / 80
// rounded — 80 m/min is the standard SG property-portal convention and
// directionally correct enough for buyer decisions.
type Station struct {
	Name      string   `json:"station"`
	Type      string   `json:"type"`  // "mrt_exit" | "lrt_exit"
	Codes     []string `json:"codes"` // ["EW16","NE3","TE17"] for Outram Park
	Lines     []string `json:"lines"` // ["EWL","NEL","TEL"]
	DistanceM int      `json:"distance_m"`
	WalkMin   int      `json:"walk_min"`
	// NearestExit is the exit label/code of the closest physical exit
	// of this station to the query point (e.g. "Exit A", "4", "C"). The
	// underlying pois row stores it in attrs.exit; the DISTINCT-ON
	// dedupe picks the closest-exit row per station, so this is the
	// exit a buyer would naturally walk to.
	NearestExit string `json:"nearest_exit,omitempty"`
}

// NearestStations returns the union of (top `limit` stations by walking
// distance) and (all stations within `withinMeters`), deduped by station
// name. Sorted by distance ascending. Backed by the existing `pois` table
// (type='mrt_exit' / 'lrt_exit'); reuses the GIST index on pois.geom.
//
// pois stores one row per EXIT, not per station — Tanjong Pagar exits
// A/B/C/D/E are five rows. For "nearest station" semantics we group by
// attrs.station and take the closest exit as that station's effective
// distance.
//
// limit=3, withinMeters=1000 is the default product setting (always
// surface top-3 even in low-MRT areas; also surface anything within 1km
// because Singapore CBD properties commonly have 4–5 stations that close).
func (s *Store) NearestStations(ctx context.Context, lat, lng float64, limit int, withinMeters int) ([]Station, error) {
	if limit <= 0 {
		limit = DefaultNearestStationsLimit
	}
	if withinMeters < 0 {
		withinMeters = 0
	}
	rows, err := s.Pool.Query(ctx, `
		WITH closest_exit_per_station AS (
		    SELECT DISTINCT ON (attrs->>'station')
		        attrs->>'station' AS station,
		        attrs->>'exit'    AS nearest_exit,
		        type,
		        COALESCE(attrs->'codes', '[]'::jsonb) AS codes,
		        COALESCE(attrs->'lines', '[]'::jsonb) AS lines,
		        ST_Distance(geom, ST_MakePoint($1, $2)::geography) AS distance_m
		    FROM pois
		    WHERE type IN ('mrt_exit', 'lrt_exit')
		    ORDER BY attrs->>'station',
		             ST_Distance(geom, ST_MakePoint($1, $2)::geography)
		),
		ranked AS (
		    SELECT *, row_number() OVER (ORDER BY distance_m) AS rnk
		    FROM closest_exit_per_station
		)
		SELECT station, nearest_exit, type, codes, lines, distance_m
		FROM ranked
		WHERE rnk <= $3 OR distance_m <= $4
		ORDER BY distance_m`,
		lng, lat, limit, withinMeters,
	)
	if err != nil {
		return nil, fmt.Errorf("nearest stations query: %w", err)
	}
	defer rows.Close()

	var out []Station
	for rows.Next() {
		var st Station
		var codesRaw, linesRaw []byte
		var distM float64
		var nearestExit *string // NULL when attrs.exit is absent
		if err := rows.Scan(&st.Name, &nearestExit, &st.Type, &codesRaw, &linesRaw, &distM); err != nil {
			return nil, fmt.Errorf("nearest stations scan: %w", err)
		}
		if nearestExit != nil {
			st.NearestExit = strings.TrimSpace(*nearestExit)
		}
		// codes / lines come back as JSONB arrays — decode into []string.
		// A NULL or unparseable attrs leaves the slice empty rather than
		// failing the whole query.
		_ = json.Unmarshal(codesRaw, &st.Codes)
		_ = json.Unmarshal(linesRaw, &st.Lines)
		if st.Codes == nil {
			st.Codes = []string{}
		}
		if st.Lines == nil {
			st.Lines = []string{}
		}
		st.DistanceM = int(math.Round(distM))
		st.WalkMin = walkMinutes(st.DistanceM)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nearest stations iterate: %w", err)
	}
	return out, nil
}

// walkMinutes converts straight-line metres to a walking-time estimate
// at 80 m/min, rounded to the nearest minute (min 1). Same heuristic
// most Singapore property portals use; not exact (real walking distance
// accounts for covered walkways and overhead bridges), but directionally
// correct and stable enough for "5 min to MRT" UX copy.
func walkMinutes(distanceM int) int {
	if distanceM <= 0 {
		return 0
	}
	m := int(math.Round(float64(distanceM) / 80.0))
	if m < 1 {
		return 1
	}
	return m
}

// UpdateProjectNearbyMRT writes the JSONB and stamps nearby_mrt_synced_at.
// Called by the enrichment job per-project after computing NearestStations.
// projectName is the URA primary key (projects.name).
func (s *Store) UpdateProjectNearbyMRT(ctx context.Context, projectName string, stations []Station) error {
	raw, err := json.Marshal(stations)
	if err != nil {
		return fmt.Errorf("marshal nearby_mrt: %w", err)
	}
	if len(raw) == 0 {
		raw = []byte("[]")
	}
	_, err = s.Pool.Exec(ctx, `
		UPDATE projects
		SET nearby_mrt = $2::jsonb,
		    nearby_mrt_synced_at = now()
		WHERE name = $1`,
		projectName, string(raw),
	)
	if err != nil {
		return fmt.Errorf("update projects.nearby_mrt: %w", err)
	}
	return nil
}

// ProjectNeedingMRT returns (name, lat, lng) tuples for projects
// that have coordinates but no nearby_mrt yet, OR whose nearby_mrt is
// older than the most recent pois.ingested_at (i.e. the underlying MRT
// data refreshed since this project was enriched). Bounded by `limit`
// so the worker can stream in batches.
//
// When `force` is true the freshness check is skipped — every project
// with coords is returned, even if its nearby_mrt was just synced.
// Use this with mode=backfill so an operator can force a re-enrichment
// after a schema change (new fields like nearest_exit added to the
// query) without having to manually bump pois.ingested_at.
type ProjectNeedingMRT struct {
	Name string
	Lat  float64
	Lng  float64
}

// limit=0 (or negative) means "no LIMIT" — the query returns every
// project with coords. Used by force-backfill so a single fetch covers
// the whole set; the row-by-row UPDATE loop in the ingestor doesn't
// loop again because the result already contained everything.
func (s *Store) ProjectsNeedingMRTEnrichment(ctx context.Context, limit int, force bool) ([]ProjectNeedingMRT, error) {
	freshnessClause := `
		  AND (p.nearby_mrt_synced_at IS NULL
		       OR p.nearby_mrt_synced_at < l.ts)`
	if force {
		freshnessClause = ""
	}
	limitClause := ""
	args := []any{}
	if limit > 0 {
		limitClause = " LIMIT $1"
		args = append(args, limit)
	}
	rows, err := s.Pool.Query(ctx, `
		WITH pois_latest AS (
		    SELECT COALESCE(MAX(ingested_at), '1970-01-01'::timestamptz) AS ts
		    FROM pois
		    WHERE type IN ('mrt_exit', 'lrt_exit')
		)
		SELECT p.name, p.lat, p.lng
		FROM projects p, pois_latest l
		WHERE p.lat IS NOT NULL
		  AND p.lng IS NOT NULL`+freshnessClause+`
		ORDER BY (p.nearby_mrt_synced_at IS NULL) DESC,
		         p.txn_count DESC NULLS LAST`+limitClause, args...)
	if err != nil {
		return nil, fmt.Errorf("projects needing mrt: %w", err)
	}
	defer rows.Close()
	var out []ProjectNeedingMRT
	for rows.Next() {
		var p ProjectNeedingMRT
		if err := rows.Scan(&p.Name, &p.Lat, &p.Lng); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
