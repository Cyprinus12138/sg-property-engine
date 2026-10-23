package httpapi

import (
	"fmt"
	"net/http"
	"time"
)

// HDB resale uses hdb_blocks for geom; hdb_resale itself has no coordinates.
// Every query that needs geo filters JOINs hdb_blocks and uses b.geom.
const hdbBaseFrom = `
  FROM hdb_resale r
  LEFT JOIN hdb_blocks b
    ON b.street_name = r.street_name AND b.block = r.block`

func (s *Server) hdbTransactions(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("b.geom")
	q.eq("r.town", qp.Get("town"))
	q.eq("r.flat_type", qp.Get("flat_type"))
	q.eq("r.block", qp.Get("block"))
	q.prefix("r.street_name", qp.Get("street"))
	q.gte("r.month", qp.Get("min_month"))
	q.lte("r.month", qp.Get("max_month"))
	q.gte("r.resale_price", qp.Get("min_price"))
	q.lte("r.resale_price", qp.Get("max_price"))
	q.gte("r.floor_area_sqm", qp.Get("min_area"))
	q.lte("r.floor_area_sqm", qp.Get("max_area"))
	q.eq("r.storey_range", qp.Get("storey"))

	if err := q.applyNearAnchor(r.Context(), s.Store, r); err != nil {
		writeErr(w, 500, "anchor_failed", err.Error())
		return
	}
	q.applyHasPOI(r)

	limit, offset := pagination(r, defaultPageSize, maxPageSize)

	// Total count (same filters, no paging).
	countSQL := "SELECT COUNT(*)" + hdbBaseFrom + q.whereClause()
	var total int
	if err := s.Store.Pool.QueryRow(r.Context(), countSQL, q.args...).Scan(&total); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	// Distance column if we have an anchor.
	distCol := "NULL::double precision AS distance_m"
	if q.anchorGeom != "" {
		distCol = fmt.Sprintf("ST_Distance(b.geom, %s) AS distance_m", q.anchorGeom)
	}
	orderBy := "r.month DESC, r.resale_price DESC"
	switch qp.Get("sort") {
	case "price_desc":
		orderBy = "r.resale_price DESC"
	case "price_asc":
		orderBy = "r.resale_price ASC"
	case "distance_asc":
		if q.anchorGeom != "" {
			orderBy = "distance_m ASC"
		}
	}

	listSQL := `
	SELECT r.month, r.town, r.block, r.street_name, r.flat_type, r.storey_range,
	       r.floor_area_sqm, r.flat_model, r.lease_commence, r.remaining_lease,
	       r.resale_price, b.lat, b.lng, ` + distCol + hdbBaseFrom + q.whereClause() +
		" ORDER BY " + orderBy +
		fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	rows, err := s.Store.Pool.Query(r.Context(), listSQL, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()

	type outRow struct {
		Month          string   `json:"month"`
		Town           string   `json:"town"`
		Block          string   `json:"block"`
		Street         string   `json:"street_name"`
		FlatType       string   `json:"flat_type"`
		StoreyRange    string   `json:"storey_range"`
		FloorAreaSqm   float64  `json:"floor_area_sqm"`
		FlatModel      string   `json:"flat_model"`
		LeaseCommence  int      `json:"lease_commence"`
		RemainingLease string   `json:"remaining_lease"`
		ResalePrice    float64  `json:"resale_price"`
		PSF            float64  `json:"psf"`
		Lat            *float64 `json:"lat,omitempty"`
		Lng            *float64 `json:"lng,omitempty"`
		DistanceM      *float64 `json:"distance_m,omitempty"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var o outRow
		var mstr time.Time
		if err := rows.Scan(&mstr, &o.Town, &o.Block, &o.Street, &o.FlatType, &o.StoreyRange,
			&o.FloorAreaSqm, &o.FlatModel, &o.LeaseCommence, &o.RemainingLease,
			&o.ResalePrice, &o.Lat, &o.Lng, &o.DistanceM); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		o.Month = mstr.Format("2006-01-02")
		if o.FloorAreaSqm > 0 {
			o.PSF = o.ResalePrice / (o.FloorAreaSqm * 10.7639)
		}
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{
		"rows": out, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) hdbStats(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	groupBy := qp.Get("group_by")
	metric := qp.Get("metric")
	if metric == "" {
		metric = "median_psf"
	}

	var groupExpr, groupLabel string
	switch groupBy {
	case "month":
		groupExpr = "to_char(r.month, 'YYYY-MM')"
		groupLabel = "month"
	case "town":
		groupExpr = "r.town"
		groupLabel = "town"
	case "flat_type":
		groupExpr = "r.flat_type"
		groupLabel = "flat_type"
	default:
		writeErr(w, 400, "bad_group_by", "group_by must be month|town|flat_type")
		return
	}

	var valueExpr string
	switch metric {
	case "median_psf":
		valueExpr = "percentile_cont(0.5) WITHIN GROUP (ORDER BY r.resale_price / NULLIF(r.floor_area_sqm * 10.7639, 0))"
	case "median_price":
		valueExpr = "percentile_cont(0.5) WITHIN GROUP (ORDER BY r.resale_price)"
	case "avg_price":
		valueExpr = "AVG(r.resale_price)"
	case "count":
		valueExpr = "COUNT(*)::float"
	default:
		writeErr(w, 400, "bad_metric", "metric must be median_psf|median_price|avg_price|count")
		return
	}

	q := newQB("b.geom")
	q.eq("r.town", qp.Get("town"))
	q.eq("r.flat_type", qp.Get("flat_type"))
	q.gte("r.month", qp.Get("min_month"))
	q.lte("r.month", qp.Get("max_month"))
	if err := q.applyNearAnchor(r.Context(), s.Store, r); err != nil {
		writeErr(w, 500, "anchor_failed", err.Error())
		return
	}
	q.applyHasPOI(r)

	sql := fmt.Sprintf(`
		SELECT %s AS key, %s AS value, COUNT(*) AS count
		  %s
		  %s
		 GROUP BY %s
		 ORDER BY key`, groupExpr, valueExpr, hdbBaseFrom, q.whereClause(), groupExpr)

	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type bucket struct {
		Key   string   `json:"key"`
		Value *float64 `json:"value"`
		Count int      `json:"count"`
	}
	out := make([]bucket, 0)
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.Key, &b.Value, &b.Count); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		out = append(out, b)
	}
	writeJSON(w, 200, map[string]any{
		"group_by": groupLabel, "metric": metric, "buckets": out,
	})
}
