package httpapi

import (
	"net/http"
	"strconv"
)

// hottestProjects ranks projects by ura_transactions volume over the
// trailing 90-day window (anchored to the most recent contract_date in
// the table, not NOW(), because URA reporting lags by weeks). Returns
// txn_count, median price, median psf, and a PSF % delta comparing the
// trailing 45d to the 45d before that.
//
// Joined to `projects` for canonical district / property_types; LEFT
// JOIN tolerates URA rows whose project name hasn't been minted into
// projects yet (the URA-side district and property_type are used as
// fallback so the row still renders).
//
// GET /explore/hottest?limit=N (default hottestPageDefault, max hottestPageMax).
func (s *Server) hottestProjects(w http.ResponseWriter, r *http.Request) {
	limit := hottestPageDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > hottestPageMax {
				n = hottestPageMax
			}
			limit = n
		}
	}

	// One SQL pass over ura_transactions with three CTEs (recent 90d
	// summary, recent 45d for the delta numerator, prior 45d for the
	// denominator) joined to projects for canonical district / types.
	// psf > 0 filters out the placeholder zeros URA occasionally emits
	// for bulk transactions where price-per-sqft isn't meaningful.
	const sql = `
		WITH anchor AS (
			SELECT MAX(contract_date)::date AS d FROM ura_transactions
		),
		recent AS (
			SELECT project,
			       COUNT(*) AS txn_count,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY price) AS median_price,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY psf)   AS median_psf,
			       MAX(district)      AS ura_district,
			       MAX(property_type) AS ura_property_type
			  FROM ura_transactions, anchor
			 WHERE contract_date >= anchor.d - INTERVAL '90 days'
			   AND psf > 0
			 GROUP BY project
		),
		recent45 AS (
			SELECT project,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY psf) AS median_psf
			  FROM ura_transactions, anchor
			 WHERE contract_date >= anchor.d - INTERVAL '45 days'
			   AND psf > 0
			 GROUP BY project
		),
		prior45 AS (
			SELECT project,
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY psf) AS median_psf
			  FROM ura_transactions, anchor
			 WHERE contract_date >= anchor.d - INTERVAL '90 days'
			   AND contract_date <  anchor.d - INTERVAL '45 days'
			   AND psf > 0
			 GROUP BY project
		)
		SELECT r.project,
		       p.name,
		       p.district,
		       p.property_types,
		       r.ura_district,
		       r.ura_property_type,
		       r.txn_count,
		       r.median_price,
		       r.median_psf,
		       r45.median_psf,
		       p45.median_psf
		  FROM recent r
		  LEFT JOIN projects p
		    ON regexp_replace(lower(p.name),     '[^a-z0-9]+', '', 'g')
		     = regexp_replace(lower(r.project),  '[^a-z0-9]+', '', 'g')
		  LEFT JOIN recent45 r45 ON r45.project = r.project
		  LEFT JOIN prior45  p45 ON p45.project = r.project
		 ORDER BY r.txn_count DESC, r.median_psf DESC NULLS LAST
		 LIMIT $1`

	rows, err := s.Store.Pool.Query(r.Context(), sql, limit)
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}
	defer rows.Close()

	type outRow struct {
		Name         string   `json:"name"`         // raw UPPERCASE — lookup key
		DisplayName  string   `json:"display_name"` // titlecased
		District     *string  `json:"district,omitempty"`
		PropertyType *string  `json:"property_type,omitempty"`
		TxnCount     int      `json:"txn_count"`
		MedianPrice  *float64 `json:"median_price,omitempty"`
		MedianPSF    *float64 `json:"median_psf,omitempty"`
		PSFChangePct *float64 `json:"psf_change_pct,omitempty"`
		Direction    string   `json:"direction"` // "up" | "down" | "flat"
	}
	out := make([]outRow, 0, limit)
	for rows.Next() {
		var (
			uraProject      string
			canonName       *string
			canonDistrict   *string
			canonTypes      []string
			uraDistrict     *string
			uraPropertyType *string
			txnCount        int
			medianPrice     *float64
			medianPSF       *float64
			recentPSF       *float64
			priorPSF        *float64
		)
		if err := rows.Scan(
			&uraProject, &canonName, &canonDistrict, &canonTypes,
			&uraDistrict, &uraPropertyType,
			&txnCount, &medianPrice, &medianPSF, &recentPSF, &priorPSF,
		); err != nil {
			writeErr(w, 500, "scan_error", err.Error())
			return
		}
		name := uraProject
		if canonName != nil && *canonName != "" {
			name = *canonName
		}
		// District: prefer the projects-canonical "D17" form; fall back
		// to the URA transaction column when no projects row exists yet.
		var district *string
		if canonDistrict != nil && *canonDistrict != "" {
			d := *canonDistrict
			district = &d
		} else if uraDistrict != nil && *uraDistrict != "" {
			d := *uraDistrict
			district = &d
		}
		// PropertyType: surface the first element of the canonical
		// projects.property_types[] (titlecased); fall back to the URA
		// transaction property_type when no projects join.
		var propType *string
		if len(canonTypes) > 0 && canonTypes[0] != "" {
			tc := titlecaseURA(canonTypes[0])
			propType = &tc
		} else if uraPropertyType != nil && *uraPropertyType != "" {
			tc := titlecaseURA(*uraPropertyType)
			propType = &tc
		}
		// PSF delta: % change from prior 45d to recent 45d. Skip the
		// computation when either window is empty or the prior figure
		// is zero (would divide-by-zero / produce nonsense).
		dir, changePct := psfDirection(recentPSF, priorPSF)
		out = append(out, outRow{
			Name:         name,
			DisplayName:  titlecaseURA(name),
			District:     district,
			PropertyType: propType,
			TxnCount:     txnCount,
			MedianPrice:  medianPrice,
			MedianPSF:    medianPSF,
			PSFChangePct: changePct,
			Direction:    dir,
		})
	}
	writeJSON(w, 200, map[string]any{"rows": out, "limit": limit})
}

// psfDirection classifies the recent-vs-prior median PSF move. Returns
// ("flat", nil) when either window is missing or the prior figure is
// zero; otherwise the percentage change and "up" / "down" / "flat" with
// a ±0.1% dead band so rounding noise doesn't flip the arrow.
func psfDirection(recent, prior *float64) (string, *float64) {
	if recent == nil || prior == nil || *prior <= 0 {
		return "flat", nil
	}
	pct := (*recent - *prior) / *prior * 100.0
	switch {
	case pct > 0.1:
		return "up", &pct
	case pct < -0.1:
		return "down", &pct
	}
	return "flat", &pct
}
