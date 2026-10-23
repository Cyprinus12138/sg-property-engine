package httpapi

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// URA transactions get geom via a JOIN to projects (every row belongs to a
// named project, and projects carries the canonical coordinates).
const uraTxnFrom = `
  FROM ura_transactions t
  LEFT JOIN projects p ON p.name = t.project`

const uraRentalFrom = `
  FROM ura_rentals r
  LEFT JOIN projects p ON p.name = r.project`

func (s *Server) uraTransactions(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("p.geom")
	q.eq("t.project", qp.Get("project"))
	q.eq("t.district", qp.Get("district"))
	q.eq("t.property_type", qp.Get("property_type"))
	q.eq("t.tenure", qp.Get("tenure"))
	q.eq("t.market_segment", qp.Get("market_segment"))
	q.gte("t.contract_date", qp.Get("min_date"))
	q.lte("t.contract_date", qp.Get("max_date"))
	q.gte("t.price", qp.Get("min_price"))
	q.lte("t.price", qp.Get("max_price"))
	q.gte("t.psf", qp.Get("min_psf"))
	q.lte("t.psf", qp.Get("max_psf"))

	if err := q.applyNearAnchor(r.Context(), s.Store, r); err != nil {
		writeErr(w, 500, "anchor_failed", err.Error())
		return
	}
	q.applyHasPOI(r)

	limit, offset := pagination(r, defaultPageSize, maxPageSize)

	countSQL := "SELECT COUNT(*)" + uraTxnFrom + q.whereClause()
	var total int
	if err := s.Store.Pool.QueryRow(r.Context(), countSQL, q.args...).Scan(&total); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	distCol := "NULL::double precision AS distance_m"
	if q.anchorGeom != "" {
		distCol = fmt.Sprintf("ST_Distance(p.geom, %s) AS distance_m", q.anchorGeom)
	}
	orderBy := "t.contract_date DESC"
	if qp.Get("sort") == "distance_asc" && q.anchorGeom != "" {
		orderBy = "distance_m ASC"
	}

	sql := `
	SELECT t.project, t.street, t.district, t.property_type, t.tenure,
	       t.market_segment, t.area_sqm, t.price, t.psf, t.contract_date,
	       t.floor_range, t.type_of_sale, p.lat, p.lng, ` + distCol + uraTxnFrom +
		q.whereClause() + " ORDER BY " + orderBy +
		fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type outRow struct {
		Project       string   `json:"project"`
		Street        string   `json:"street"`
		District      string   `json:"district"`
		PropertyType  string   `json:"property_type"`
		Tenure        string   `json:"tenure"`
		MarketSegment string   `json:"market_segment"`
		AreaSqm       float64  `json:"area_sqm"`
		Price         float64  `json:"price"`
		PSF           float64  `json:"psf"`
		ContractDate  string   `json:"contract_date"`
		FloorRange    string   `json:"floor_range"`
		TypeOfSale    string   `json:"type_of_sale"`
		Lat           *float64 `json:"lat,omitempty"`
		Lng           *float64 `json:"lng,omitempty"`
		DistanceM     *float64 `json:"distance_m,omitempty"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var o outRow
		var cd time.Time
		if err := rows.Scan(&o.Project, &o.Street, &o.District, &o.PropertyType, &o.Tenure,
			&o.MarketSegment, &o.AreaSqm, &o.Price, &o.PSF, &cd, &o.FloorRange, &o.TypeOfSale,
			&o.Lat, &o.Lng, &o.DistanceM); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		o.ContractDate = cd.Format("2006-01-02")
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{
		"rows": out, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) uraRentals(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("p.geom")
	q.eq("r.project", qp.Get("project"))
	q.eq("r.district", qp.Get("district"))
	q.eq("r.property_type", qp.Get("property_type"))
	q.gte("r.lease_date", qp.Get("min_date"))
	q.lte("r.lease_date", qp.Get("max_date"))
	if err := q.applyNearAnchor(r.Context(), s.Store, r); err != nil {
		writeErr(w, 500, "anchor_failed", err.Error())
		return
	}
	q.applyHasPOI(r)

	limit, offset := pagination(r, defaultPageSize, maxPageSize)

	var total int
	if err := s.Store.Pool.QueryRow(r.Context(),
		"SELECT COUNT(*)"+uraRentalFrom+q.whereClause(), q.args...).Scan(&total); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	distCol := "NULL::double precision AS distance_m"
	if q.anchorGeom != "" {
		distCol = fmt.Sprintf("ST_Distance(p.geom, %s) AS distance_m", q.anchorGeom)
	}
	sql := `
	SELECT r.project, r.street, r.district, r.property_type,
	       r.area_sqm_min, r.area_sqm_max, r.bedroom_num,
	       r.rent, r.lease_date, p.lat, p.lng, ` + distCol + uraRentalFrom +
		q.whereClause() + " ORDER BY r.lease_date DESC" +
		fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type outRow struct {
		Project      string   `json:"project"`
		Street       string   `json:"street"`
		District     string   `json:"district"`
		PropertyType string   `json:"property_type"`
		AreaSqmMin   *float64 `json:"area_sqm_min,omitempty"`
		AreaSqmMax   *float64 `json:"area_sqm_max,omitempty"`
		BedroomNum   *float64 `json:"bedroom_num,omitempty"`
		Rent         float64  `json:"rent"`
		LeaseDate    string   `json:"lease_date"`
		Lat          *float64 `json:"lat,omitempty"`
		Lng          *float64 `json:"lng,omitempty"`
		DistanceM    *float64 `json:"distance_m,omitempty"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var o outRow
		var ld time.Time
		if err := rows.Scan(&o.Project, &o.Street, &o.District, &o.PropertyType,
			&o.AreaSqmMin, &o.AreaSqmMax, &o.BedroomNum, &o.Rent, &ld,
			&o.Lat, &o.Lng, &o.DistanceM); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		o.LeaseDate = ld.Format("2006-01-02")
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{
		"rows": out, "total": total, "limit": limit, "offset": offset,
	})
}

// uraRentalsTrend returns monthly P25/Median/P75 rent stats aggregated at the
// DB level. Supports filtering by district, property_type, bedroom_num, min_date,
// max_date. This avoids the 500-row API limit that would otherwise only return
// the most recent month of data.
func (s *Server) uraRentalsTrend(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("NULL")
	q.eq("r.district", qp.Get("district"))
	q.eq("r.property_type", qp.Get("property_type"))
	q.gte("r.lease_date", qp.Get("min_date"))
	q.lte("r.lease_date", qp.Get("max_date"))

	if bd := qp.Get("bedroom_num"); bd != "" {
		n, err := strconv.ParseFloat(bd, 64)
		if err == nil {
			q.args = append(q.args, int64(math.Round(n)))
			q.where = append(q.where, fmt.Sprintf("ROUND(r.bedroom_num::numeric) = $%d", len(q.args)))
		}
	}

	sql := `
	SELECT
	    to_char(r.lease_date, 'YYYY-MM') AS month,
	    COUNT(*) AS txns,
	    ROUND(percentile_cont(0.25) WITHIN GROUP (ORDER BY r.rent))::bigint AS p25,
	    ROUND(percentile_cont(0.50) WITHIN GROUP (ORDER BY r.rent))::bigint AS median_rent,
	    ROUND(percentile_cont(0.75) WITHIN GROUP (ORDER BY r.rent))::bigint AS p75
	FROM ura_rentals r` + q.whereClause() + `
	GROUP BY 1
	ORDER BY 1 ASC`

	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()

	type monthRow struct {
		Month      string `json:"month"`
		Txns       int64  `json:"txns"`
		P25        int64  `json:"p25"`
		MedianRent int64  `json:"median_rent"`
		P75        int64  `json:"p75"`
	}
	out := make([]monthRow, 0)
	for rows.Next() {
		var mo monthRow
		if err := rows.Scan(&mo.Month, &mo.Txns, &mo.P25, &mo.MedianRent, &mo.P75); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		out = append(out, mo)
	}
	writeJSON(w, 200, map[string]any{"months": out, "total": len(out)})
}

// uraDeveloperSales lists rows from ura_developer_sales filtered by project /
// district / market_segment / refPeriod range. Joins to projects for lat/lng
// when available, mirroring /ura/transactions and /ura/rentals.
func (s *Server) uraDeveloperSales(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("p.geom")
	q.eq("d.project", qp.Get("project"))
	q.eq("d.district", qp.Get("district"))
	q.eq("d.market_segment", qp.Get("market_segment"))
	q.eq("d.developer", qp.Get("developer"))
	q.gte("d.ref_period", qp.Get("min_period"))
	q.lte("d.ref_period", qp.Get("max_period"))

	limit, offset := pagination(r, defaultPageSize, maxPageSize)

	const fromClause = `
	  FROM ura_developer_sales d
	  LEFT JOIN projects p ON p.name = d.project`

	var total int
	if err := s.Store.Pool.QueryRow(r.Context(),
		"SELECT COUNT(*)"+fromClause+q.whereClause(), q.args...).Scan(&total); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	sql := `
	SELECT d.project, d.developer, d.street, d.district, d.market_segment, d.property_type,
	       d.ref_period, d.units_avail, d.launched_to_date, d.sold_to_date,
	       d.launched_in_month, d.sold_in_month,
	       d.lowest_price, d.median_price, d.highest_price,
	       p.lat, p.lng` + fromClause +
		q.whereClause() + " ORDER BY d.ref_period DESC, d.project ASC" +
		fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type outRow struct {
		Project         string   `json:"project"`
		Developer       string   `json:"developer"`
		Street          string   `json:"street"`
		District        string   `json:"district"`
		MarketSegment   string   `json:"market_segment"`
		PropertyType    string   `json:"property_type"`
		RefPeriod       string   `json:"ref_period"`
		UnitsAvail      int      `json:"units_avail"`
		LaunchedToDate  int      `json:"launched_to_date"`
		SoldToDate      int      `json:"sold_to_date"`
		LaunchedInMonth int      `json:"launched_in_month"`
		SoldInMonth     int      `json:"sold_in_month"`
		LowestPrice     float64  `json:"lowest_price"`
		MedianPrice     float64  `json:"median_price"`
		HighestPrice    float64  `json:"highest_price"`
		Lat             *float64 `json:"lat,omitempty"`
		Lng             *float64 `json:"lng,omitempty"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var o outRow
		var rp time.Time
		if err := rows.Scan(&o.Project, &o.Developer, &o.Street, &o.District, &o.MarketSegment, &o.PropertyType,
			&rp, &o.UnitsAvail, &o.LaunchedToDate, &o.SoldToDate,
			&o.LaunchedInMonth, &o.SoldInMonth,
			&o.LowestPrice, &o.MedianPrice, &o.HighestPrice,
			&o.Lat, &o.Lng); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		o.RefPeriod = rp.Format("2006-01")
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{
		"rows": out, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) uraPipeline(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	q := newQB("NULL")
	q.eq("up.district", qp.Get("district"))
	q.eq("up.status", qp.Get("status"))

	// The PMI_Resi_Pipeline feed returns some projects as several records
	// (multi-parcel phases; placeholder labels like "Residential
	// apartments" reused across undisclosed projects), and ingest stores
	// them verbatim. DISTINCT ON the normalised name collapses them to one
	// row each, keeping the row with the most units sold / largest parcel.
	sql := `SELECT DISTINCT ON (regexp_replace(lower(up.project), '[^a-z0-9]+', '', 'g'))
	               up.project, up.developer, up.street, up.district, up.market_segment,
	               up.total_units, up.units_sold, up.expected_top, up.status
	          FROM ura_pipeline up
	          ` + q.whereClause() + `
	         ORDER BY regexp_replace(lower(up.project), '[^a-z0-9]+', '', 'g'),
	                  up.units_sold DESC, up.total_units DESC, up.id`
	rows, err := s.Store.Pool.Query(r.Context(), sql, q.args...)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type outRow struct {
		Project       string `json:"project"`
		Developer     string `json:"developer"`
		Street        string `json:"street"`
		District      string `json:"district"`
		MarketSegment string `json:"market_segment"`
		TotalUnits    int    `json:"total_units"`
		UnitsSold     int    `json:"units_sold"`
		ExpectedTop   string `json:"expected_top"`
		Status        string `json:"status"`
	}
	out := make([]outRow, 0)
	for rows.Next() {
		var o outRow
		if err := rows.Scan(&o.Project, &o.Developer, &o.Street, &o.District, &o.MarketSegment,
			&o.TotalUnits, &o.UnitsSold, &o.ExpectedTop, &o.Status); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		out = append(out, o)
	}
	writeJSON(w, 200, map[string]any{"rows": out, "total": len(out)})
}
