package httpapi

import (
	"net/http"
	"strconv"
)

// districtMedian computes the median price and PSF of ura_transactions
// rows in a single district over a recent window, optionally narrowed by
// an area_sqm band. ura_transactions has no bedroom_num column, so the
// caller approximates "N-bedroom resale" by passing an area_sqm band the
// caller derived from N (e.g. 3BR ≈ 80–120 sqm). The label/basis is
// re-attached by the caller; this endpoint only knows about sqm.
//
// Anchor (most recent contract_date) matches /explore/hottest so the
// window stays consistent across the buyer Explore surface. URA reporting
// lags by weeks; anchoring on NOW() would routinely return zero rows.
//
// GET /explore/district_median?district=15
//
//	[&min_area_sqm=80][&max_area_sqm=120][&window_days=90]
func (s *Server) districtMedian(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	district := qp.Get("district")
	if district == "" {
		writeErr(w, 400, "missing_district", "district is required")
		return
	}

	windowDays := 90
	if v := qp.Get("window_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			windowDays = n
		}
	}

	var (
		minArea, maxArea float64
		hasMin, hasMax   bool
	)
	if v := qp.Get("min_area_sqm"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			minArea, hasMin = n, true
		}
	}
	if v := qp.Get("max_area_sqm"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			maxArea, hasMax = n, true
		}
	}

	// Build the WHERE incrementally so the SQL stays parameter-safe and
	// the area filter only fires when the caller actually supplied one.
	where := "WHERE district = $1 AND contract_date >= anchor.d - ($2 || ' days')::interval AND psf > 0"
	args := []any{district, strconv.Itoa(windowDays)}
	if hasMin {
		args = append(args, minArea)
		where += " AND area_sqm >= $" + strconv.Itoa(len(args))
	}
	if hasMax {
		args = append(args, maxArea)
		where += " AND area_sqm <= $" + strconv.Itoa(len(args))
	}

	sql := `
		WITH anchor AS (
			SELECT MAX(contract_date)::date AS d FROM ura_transactions
		)
		SELECT (SELECT d FROM anchor)::text AS anchor_date,
		       COUNT(*)                                                       AS sample_count,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY price)             AS median_price,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY psf)               AS median_psf
		  FROM ura_transactions, anchor
		  ` + where

	// anchorDate is nullable: when ura_transactions is empty (e.g. fresh
	// dev DB before ingest), MAX(contract_date) returns NULL and pgx
	// fails to scan NULL into a non-pointer string. Pointer + emit JSON
	// null lets callers treat sample_count=0 as "no signal" honestly.
	var (
		anchorDate  *string
		sampleCount int
		medianPrice *float64
		medianPSF   *float64
	)
	if err := s.Store.Pool.QueryRow(r.Context(), sql, args...).Scan(
		&anchorDate, &sampleCount, &medianPrice, &medianPSF,
	); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	resp := map[string]any{
		"district":     district,
		"window_days":  windowDays,
		"sample_count": sampleCount,
		"anchor_date":  anchorDate,
		"median_price": medianPrice,
		"median_psf":   medianPSF,
	}
	if hasMin {
		resp["min_area_sqm"] = minArea
	}
	if hasMax {
		resp["max_area_sqm"] = maxArea
	}
	writeJSON(w, 200, resp)
}
