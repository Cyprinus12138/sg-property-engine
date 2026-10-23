package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("projects.geom")
	q.prefix("name", qp.Get("name"))
	q.eq("district", qp.Get("district"))
	q.anyElem("property_types", qp.Get("property_type"))
	if err := q.applyNearAnchor(r.Context(), s.Store, r); err != nil {
		writeErr(w, 500, "anchor_failed", err.Error())
		return
	}
	q.applyHasPOI(r)

	limit, offset := pagination(r, defaultPageSize, maxPageSize)

	var total int
	if err := s.Store.Pool.QueryRow(r.Context(),
		"SELECT COUNT(*) FROM projects"+q.whereClause(), q.args...).Scan(&total); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	distCol := "NULL::double precision AS distance_m"
	if q.anchorGeom != "" {
		distCol = fmt.Sprintf("ST_Distance(projects.geom, %s) AS distance_m", q.anchorGeom)
	}
	orderBy := "name"
	if qp.Get("sort") == "distance_asc" && q.anchorGeom != "" {
		orderBy = "distance_m ASC"
	}
	sql := `
	SELECT name, street, district, property_types, tenure, market_segment,
	       postal, lat, lng, planning_area, first_txn_date, last_txn_date,
	       COALESCE(txn_count, 0), ` + distCol + `
	  FROM projects` + q.whereClause() + " ORDER BY " + orderBy +
		fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()

	// URA strings come UPPERCASE; titlecase at the boundary so consumers
	// can render them directly. Raw `name` is kept as the lookup key;
	// `display_name` is the human-friendly form.
	type outRow struct {
		Name          string   `json:"name"`                     // raw UPPERCASE — lookup key
		DisplayName   string   `json:"display_name"`             // titlecased
		Street        any      `json:"street,omitempty"`         // titlecased *string
		District      *string  `json:"district,omitempty"`       // canonical "D17" — no transform
		PropertyTypes []string `json:"property_types,omitempty"` // titlecased element-wise
		Tenure        any      `json:"tenure,omitempty"`         // titlecased *string
		MarketSegment *string  `json:"market_segment,omitempty"` // CCR/RCR/OCR codes, no transform
		Postal        *string  `json:"postal,omitempty"`
		Lat           *float64 `json:"lat,omitempty"`
		Lng           *float64 `json:"lng,omitempty"`
		PlanningArea  any      `json:"planning_area,omitempty"` // titlecased *string
		FirstTxnDate  *string  `json:"first_txn_date,omitempty"`
		LastTxnDate   *string  `json:"last_txn_date,omitempty"`
		TxnCount      int      `json:"txn_count"`
		DistanceM     *float64 `json:"distance_m,omitempty"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var (
			rawName                           string
			rawStreet, rawTenure, rawPlanArea *string
			rawDistrict, rawMarketSegment     *string
			rawPostal                         *string
			rawPropertyTypes                  []string
			rawLat, rawLng, rawDist           *float64
			rawTxnCount                       int
			firstTxn, lastTxn                 *time.Time
		)
		if err := rows.Scan(&rawName, &rawStreet, &rawDistrict, &rawPropertyTypes, &rawTenure,
			&rawMarketSegment, &rawPostal, &rawLat, &rawLng, &rawPlanArea,
			&firstTxn, &lastTxn, &rawTxnCount, &rawDist); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		o := outRow{
			Name:          rawName,
			DisplayName:   titlecaseURA(rawName),
			Street:        uraStringPtr(rawStreet),
			District:      rawDistrict,
			PropertyTypes: uraStringSlice(rawPropertyTypes),
			Tenure:        uraStringPtr(rawTenure),
			MarketSegment: rawMarketSegment,
			Postal:        rawPostal,
			Lat:           rawLat,
			Lng:           rawLng,
			PlanningArea:  uraStringPtr(rawPlanArea),
			TxnCount:      rawTxnCount,
			DistanceM:     rawDist,
		}
		if firstTxn != nil {
			s := firstTxn.Format("2006-01-02")
			o.FirstTxnDate = &s
		}
		if lastTxn != nil {
			s := lastTxn.Format("2006-01-02")
			o.LastTxnDate = &s
		}
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{
		"rows": out, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) projectByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var (
		street, district, tenure, marketSegment *string
		postal, planningArea                    *string
		propertyTypes                           []string
		lat, lng                                *float64
		txnCount                                int
		nearbyMRT                               []byte
	)
	// Case-insensitive lookup: URA emits the same project in different cases
	// across endpoints (transactions in CAPS, pipeline in TitleCase). Match
	// on a normalised form to avoid 404'ing the wrong case. When duplicates
	// exist we prefer the row with the most transactions — that's the
	// canonical record.
	var canonName string
	err := s.Store.Pool.QueryRow(r.Context(), `
		SELECT name, street, district, property_types, tenure, market_segment,
		       postal, lat, lng, planning_area, COALESCE(txn_count, 0),
		       COALESCE(nearby_mrt, '[]'::jsonb)
		  FROM projects
		 WHERE regexp_replace(lower(name), '[^a-z0-9]+', '', 'g')
		     = regexp_replace(lower($1),   '[^a-z0-9]+', '', 'g')
		 ORDER BY txn_count DESC NULLS LAST
		 LIMIT 1`, name).Scan(
		&canonName, &street, &district, &propertyTypes, &tenure, &marketSegment,
		&postal, &lat, &lng, &planningArea, &txnCount, &nearbyMRT)
	if err != nil {
		writeErr(w, 404, "not_found", err.Error())
		return
	}
	if len(nearbyMRT) == 0 {
		nearbyMRT = []byte("[]")
	}
	writeJSON(w, 200, map[string]any{
		"name":           canonName,               // raw UPPERCASE — lookup key
		"display_name":   titlecaseURA(canonName), // human-friendly
		"street":         uraStringPtr(street),
		"district":       district, // already in canonical "D17"-shape (no transform)
		"property_types": uraStringSlice(propertyTypes),
		"tenure":         uraStringPtr(tenure),
		"market_segment": marketSegment, // codes (CCR/RCR/OCR), no transform
		"postal":         postal,
		"lat":            lat,
		"lng":            lng,
		"planning_area":  uraStringPtr(planningArea),
		"txn_count":      txnCount,
		// nearby_mrt is the enrichment job's output: [{station, type,
		// codes, lines, distance_m, walk_min, nearest_exit}, ...]. Empty
		// array until the nearby_mrt source has run for this project.
		"nearby_mrt": json.RawMessage(nearbyMRT),
	})
}

func (s *Server) pois(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("pois.geom")
	q.eq("type", qp.Get("type"))
	q.eq("subtype", qp.Get("subtype"))
	q.prefix("name", qp.Get("name"))
	if err := q.applyNearAnchor(r.Context(), s.Store, r); err != nil {
		writeErr(w, 500, "anchor_failed", err.Error())
		return
	}

	limit, offset := pagination(r, 100, 500)

	var total int
	if err := s.Store.Pool.QueryRow(r.Context(),
		"SELECT COUNT(*) FROM pois"+q.whereClause(), q.args...).Scan(&total); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	distCol := "NULL::double precision AS distance_m"
	orderBy := "type, name"
	if q.anchorGeom != "" {
		distCol = fmt.Sprintf("ST_Distance(pois.geom, %s) AS distance_m", q.anchorGeom)
		orderBy = "distance_m ASC"
	}
	sql := `
	SELECT id, type, name, subtype, lat, lng, ` + distCol + `
	  FROM pois` + q.whereClause() + " ORDER BY " + orderBy +
		fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type outRow struct {
		ID        int64    `json:"id"`
		Type      string   `json:"type"`
		Name      string   `json:"name"`
		Subtype   *string  `json:"subtype,omitempty"`
		Lat       float64  `json:"lat"`
		Lng       float64  `json:"lng"`
		DistanceM *float64 `json:"distance_m,omitempty"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var o outRow
		if err := rows.Scan(&o.ID, &o.Type, &o.Name, &o.Subtype, &o.Lat, &o.Lng, &o.DistanceM); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{
		"rows": out, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) poiTypes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Pool.Query(r.Context(),
		"SELECT type, COUNT(*) FROM pois GROUP BY type ORDER BY type")
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type row struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	out := make([]row, 0)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Type, &r.Count); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		out = append(out, r)
	}
	writeJSON(w, 200, map[string]any{"types": out})
}

// nearestStations — GET /geo/nearest-stations?lat=&lng=[&limit=3][&within_m=1000]
//
// Exposes the same station lookup the nearby_mrt enrichment job uses, for
// an arbitrary point: the union of the top `limit` stations by distance and
// every station within `within_m` metres, one row per station (closest exit
// wins), with codes/lines when the station-code lookup resolved them.
func (s *Server) nearestStations(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	lat, errLat := strconv.ParseFloat(qp.Get("lat"), 64)
	lng, errLng := strconv.ParseFloat(qp.Get("lng"), 64)
	if errLat != nil || errLng != nil || (lat == 0 && lng == 0) {
		writeErr(w, 400, "bad_point", "lat and lng are required")
		return
	}
	limit, _ := strconv.Atoi(qp.Get("limit"))
	if limit <= 0 {
		limit = 3
	}
	if limit > 20 {
		limit = 20
	}
	within, _ := strconv.Atoi(qp.Get("within_m"))
	if within <= 0 {
		within = 1000
	}
	if within > 5000 {
		within = 5000
	}
	stations, err := s.Store.NearestStations(r.Context(), lat, lng, limit, within)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	if stations == nil {
		stations = []store.Station{}
	}
	writeJSON(w, 200, map[string]any{
		"lat": lat, "lng": lng, "limit": limit, "within_m": within,
		"stations": stations,
	})
}
