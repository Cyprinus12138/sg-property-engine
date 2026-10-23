package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// searchProperties — GET /search/properties?q={q}&limit={limit}
//
// Unified autocomplete over private projects and HDB blocks. Each row is
// tagged with `kind` ("project" | "hdb") so a client can label them.
//
// Matching:
//   - projects.name           ILIKE %q%
//   - hdb_blocks: name / street_name / block ILIKE %q%
//
// At ~12k HDB blocks and a few thousand projects, plain ILIKE scans are
// well under 10ms — no pg_trgm index required yet.
func (s *Server) searchProperties(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeJSON(w, 200, map[string]any{"rows": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > searchPageMax {
		limit = searchPageDefault
	}
	pattern := "%" + q + "%"
	prefix := q + "%"

	var (
		wg       sync.WaitGroup
		projects []searchRow
		hdb      []searchRow
		projErr  error
		hdbErr   error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		projects, projErr = s.searchProjects(r.Context(), pattern, prefix, limit)
	}()
	go func() {
		defer wg.Done()
		hdb, hdbErr = s.searchHDBBlocks(r.Context(), pattern, prefix, limit)
	}()
	wg.Wait()
	if projErr != nil {
		writeErr(w, 500, "db_error", projErr.Error())
		return
	}
	if hdbErr != nil {
		writeErr(w, 500, "db_error", hdbErr.Error())
		return
	}

	merged := mergeSearchRows(projects, hdb, limit)
	writeJSON(w, 200, map[string]any{"rows": merged})
}

type searchRow struct {
	Kind     string `json:"kind"` // "project" | "hdb"
	Name     string `json:"name"` // display title for the dropdown row
	Subtitle string `json:"subtitle,omitempty"`

	// project-only
	District      *string  `json:"district,omitempty"`
	Tenure        *string  `json:"tenure,omitempty"`
	PropertyTypes []string `json:"property_types,omitempty"`
	MarketSegment *string  `json:"market_segment,omitempty"`

	// hdb-only
	Block         *string `json:"block,omitempty"`
	StreetName    *string `json:"street_name,omitempty"`
	EstateName    *string `json:"estate_name,omitempty"`
	YearCompleted *int    `json:"year_completed,omitempty"`

	// shared
	Street       *string  `json:"street,omitempty"`
	Postal       *string  `json:"postal,omitempty"`
	Lat          *float64 `json:"lat,omitempty"`
	Lng          *float64 `json:"lng,omitempty"`
	PlanningArea *string  `json:"planning_area,omitempty"`

	// rank: lower = better. Used only for merging; not serialised.
	rank int
}

func (s *Server) searchProjects(ctx context.Context, pattern, prefix string, limit int) ([]searchRow, error) {
	const sql = `
		SELECT name, street, district, property_types, tenure, market_segment,
		       postal, lat, lng, planning_area,
		       CASE
		         WHEN lower(name) = lower($2) THEN 0
		         WHEN name ILIKE $3 THEN 1
		         ELSE 2
		       END AS rank
		  FROM projects
		 WHERE name ILIKE $1
		 ORDER BY rank, txn_count DESC NULLS LAST, name
		 LIMIT $4`
	rows, err := s.Store.Pool.Query(ctx, sql, pattern, strings.Trim(pattern, "%"), prefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []searchRow
	for rows.Next() {
		var r searchRow
		r.Kind = "project"
		if err := rows.Scan(&r.Name, &r.Street, &r.District, &r.PropertyTypes, &r.Tenure,
			&r.MarketSegment, &r.Postal, &r.Lat, &r.Lng, &r.PlanningArea, &r.rank); err != nil {
			return nil, err
		}
		r.Subtitle = projectSubtitle(&r)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) searchHDBBlocks(ctx context.Context, pattern, prefix string, limit int) ([]searchRow, error) {
	const sql = `
		SELECT block, street_name, name, postal, lat, lng, planning_area, year_completed,
		       CASE
		         WHEN block ILIKE $3 OR street_name ILIKE $3 OR name ILIKE $3 THEN 1
		         ELSE 2
		       END AS rank
		  FROM hdb_blocks
		 WHERE block ILIKE $1 OR street_name ILIKE $1 OR name ILIKE $1
		 ORDER BY rank, street_name, block
		 LIMIT $2`
	rows, err := s.Store.Pool.Query(ctx, sql, pattern, limit, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []searchRow
	for rows.Next() {
		var (
			block, streetName, estate *string
			postal, planningArea      *string
			lat, lng                  *float64
			yearCompleted             *int
			rank                      int
		)
		if err := rows.Scan(&block, &streetName, &estate, &postal, &lat, &lng, &planningArea, &yearCompleted, &rank); err != nil {
			return nil, err
		}
		r := searchRow{
			Kind:          "hdb",
			Block:         block,
			StreetName:    streetName,
			EstateName:    estate,
			YearCompleted: yearCompleted,
			Postal:        postal,
			Lat:           lat,
			Lng:           lng,
			PlanningArea:  planningArea,
			rank:          rank,
		}
		// Derive district from postal code so HDB hits expose the same
		// 2-digit D01..D28 scheme that private projects already use.
		// Falls through to nil when postal is empty / "NI" / non-numeric.
		if postal != nil {
			if d := postalToDistrict(*postal); d != "" {
				r.District = &d
			}
		}
		r.Name = hdbDisplayName(block, streetName, estate)
		r.Subtitle = hdbSubtitle(estate, postal, planningArea)
		// Synthesise a `street` so consumers can autofill the address field
		// without a kind-specific branch.
		if block != nil && streetName != nil {
			combined := "Blk " + *block + " " + *streetName
			r.Street = &combined
		} else if streetName != nil {
			r.Street = streetName
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// mergeSearchRows interleaves project and HDB hits by rank, capping at
// `limit` total. Within the same rank we prefer the originating order
// (already DB-sorted), alternating sources so neither dominates the
// dropdown when both have strong matches.
func mergeSearchRows(projects, hdb []searchRow, limit int) []searchRow {
	out := make([]searchRow, 0, limit)
	pi, hi := 0, 0
	for len(out) < limit && (pi < len(projects) || hi < len(hdb)) {
		switch {
		case pi >= len(projects):
			out = append(out, hdb[hi])
			hi++
		case hi >= len(hdb):
			out = append(out, projects[pi])
			pi++
		case projects[pi].rank < hdb[hi].rank:
			out = append(out, projects[pi])
			pi++
		case hdb[hi].rank < projects[pi].rank:
			out = append(out, hdb[hi])
			hi++
		default:
			// Tied rank: alternate to keep both kinds visible.
			if (len(out) % 2) == 0 {
				out = append(out, projects[pi])
				pi++
			} else {
				out = append(out, hdb[hi])
				hi++
			}
		}
	}
	return out
}

func projectSubtitle(r *searchRow) string {
	parts := make([]string, 0, 3)
	if r.District != nil && *r.District != "" {
		parts = append(parts, "District "+*r.District)
	}
	if len(r.PropertyTypes) > 0 {
		parts = append(parts, r.PropertyTypes[0])
	} else if r.MarketSegment != nil && *r.MarketSegment != "" {
		parts = append(parts, *r.MarketSegment)
	}
	if r.Tenure != nil && *r.Tenure != "" {
		parts = append(parts, *r.Tenure)
	}
	return strings.Join(parts, " · ")
}

func hdbDisplayName(block, street, estate *string) string {
	if block != nil && street != nil && *block != "" && *street != "" {
		return "Blk " + *block + " " + *street
	}
	if estate != nil && *estate != "" {
		return *estate
	}
	if street != nil {
		return *street
	}
	return ""
}

func hdbSubtitle(estate, postal, planningArea *string) string {
	parts := make([]string, 0, 3)
	if estate != nil && *estate != "" {
		parts = append(parts, *estate)
	}
	if planningArea != nil && *planningArea != "" {
		parts = append(parts, *planningArea)
	}
	if postal != nil && *postal != "" {
		parts = append(parts, *postal)
	}
	return strings.Join(parts, " · ")
}
