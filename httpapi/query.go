package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Shared helpers for building filtered SQL queries across HDB/URA/projects.
// Every list endpoint uses the same three stages:
//
//   1. Collect filter args from the URL into a *qb (query builder)
//   2. Append any geo filter (near-anchor or has-poi)
//   3. Run count + page query and return {rows,total,limit,offset}
//
// Keeping this centralized means /hdb, /ura, /projects all expose an
// identical filter vocabulary — tools calling the engine never have to
// remember which endpoint supports which geo option.

type qb struct {
	where []string
	args  []any
	// For near-anchor geo filter: an anchor point to compute distance from.
	// When set, SELECT adds an ST_Distance(row.geom, anchor_geom) AS distance_m
	// column, ORDER BY can use it, and rows get a `distance_m` field.
	anchorGeom string // SQL expression resolving to a geography point, e.g. "ST_SetSRID(ST_MakePoint($3, $2), 4326)::geography"
	// Optional "has poi within X metres" filter.
	hasPOIType    string
	hasPOISubtype string
	hasPOIWithinM int
	// Table alias used for the row being filtered. Callers set this.
	rowGeomExpr string // e.g. "b.geom" or "p.geom"
}

func newQB(rowGeomExpr string) *qb {
	return &qb{rowGeomExpr: rowGeomExpr}
}

func (q *qb) eq(column, value string) {
	if value == "" {
		return
	}
	q.args = append(q.args, value)
	q.where = append(q.where, fmt.Sprintf("%s = $%d", column, len(q.args)))
}

// anyElem filters an array column: WHERE $value = ANY(column).
func (q *qb) anyElem(column, value string) {
	if value == "" {
		return
	}
	q.args = append(q.args, value)
	q.where = append(q.where, fmt.Sprintf("$%d = ANY(%s)", len(q.args), column))
}

func (q *qb) prefix(column, value string) {
	if value == "" {
		return
	}
	q.args = append(q.args, value+"%")
	q.where = append(q.where, fmt.Sprintf("%s ILIKE $%d", column, len(q.args)))
}

func (q *qb) gte(column, value string) {
	if value == "" {
		return
	}
	q.args = append(q.args, value)
	q.where = append(q.where, fmt.Sprintf("%s >= $%d", column, len(q.args)))
}

func (q *qb) lte(column, value string) {
	if value == "" {
		return
	}
	q.args = append(q.args, value)
	q.where = append(q.where, fmt.Sprintf("%s <= $%d", column, len(q.args)))
}

// applyNearAnchor resolves the "near this anchor" set of query params into
// a geom expression. Supports: near_lat+near_lng, near_project, near_poi_*,
// near_block+near_street. Returns false if no anchor was requested.
func (q *qb) applyNearAnchor(ctx context.Context, store interface {
	ResolveAnchor(ctx context.Context, kind, a, b string) (lat, lng float64, ok bool, err error)
}, r *http.Request) error {
	qp := r.URL.Query()
	radius := qp.Get("radius_m")
	if radius == "" {
		radius = "1000"
	}
	radiusI, _ := strconv.Atoi(radius)
	if radiusI <= 0 {
		radiusI = 1000
	}
	if radiusI > 5000 {
		radiusI = 5000
	}

	var lat, lng float64
	haveAnchor := false

	if v := qp.Get("near_lat"); v != "" {
		lat, _ = strconv.ParseFloat(v, 64)
		lng, _ = strconv.ParseFloat(qp.Get("near_lng"), 64)
		haveAnchor = lat != 0 || lng != 0
	} else if name := qp.Get("near_project"); name != "" {
		la, ln, ok, err := store.ResolveAnchor(ctx, "project", name, "")
		if err != nil {
			return err
		}
		if ok {
			lat, lng, haveAnchor = la, ln, true
		}
	} else if t := qp.Get("near_poi_type"); t != "" {
		la, ln, ok, err := store.ResolveAnchor(ctx, "poi", t, qp.Get("near_poi_name"))
		if err != nil {
			return err
		}
		if ok {
			lat, lng, haveAnchor = la, ln, true
		}
	} else if blk := qp.Get("near_block"); blk != "" {
		la, ln, ok, err := store.ResolveAnchor(ctx, "hdb_block", qp.Get("near_street"), blk)
		if err != nil {
			return err
		}
		if ok {
			lat, lng, haveAnchor = la, ln, true
		}
	}

	if !haveAnchor {
		return nil
	}

	q.args = append(q.args, lng, lat, radiusI)
	lngIdx := len(q.args) - 2
	latIdx := len(q.args) - 1
	radIdx := len(q.args)
	anchor := fmt.Sprintf("ST_SetSRID(ST_MakePoint($%d, $%d), 4326)::geography", lngIdx, latIdx)
	q.anchorGeom = anchor
	q.where = append(q.where, fmt.Sprintf("ST_DWithin(%s, %s, $%d)", q.rowGeomExpr, anchor, radIdx))
	return nil
}

func (q *qb) applyHasPOI(r *http.Request) {
	qp := r.URL.Query()
	t := qp.Get("has_poi_type")
	if t == "" {
		return
	}
	within := qp.Get("has_poi_within_m")
	withinI, _ := strconv.Atoi(within)
	if withinI <= 0 {
		withinI = 500
	}
	if withinI > 5000 {
		withinI = 5000
	}
	q.hasPOIType = t
	q.hasPOISubtype = qp.Get("has_poi_subtype")
	q.hasPOIWithinM = withinI

	q.args = append(q.args, t, withinI)
	tIdx := len(q.args) - 1
	wIdx := len(q.args)
	clause := fmt.Sprintf(`EXISTS (
		SELECT 1 FROM pois p
		WHERE p.type = $%d
		  AND ST_DWithin(%s, p.geom, $%d)`, tIdx, q.rowGeomExpr, wIdx)
	if q.hasPOISubtype != "" {
		q.args = append(q.args, q.hasPOISubtype)
		clause += fmt.Sprintf(" AND p.subtype = $%d", len(q.args))
	}
	clause += ")"
	q.where = append(q.where, clause)
}

func (q *qb) whereClause() string {
	if len(q.where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(q.where, " AND ")
}

// pagination reads limit/offset from the URL, clamped to sensible bounds.
func pagination(r *http.Request, defaultLimit, maxLimit int) (int, int) {
	qp := r.URL.Query()
	limit, _ := strconv.Atoi(qp.Get("limit"))
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset, _ := strconv.Atoi(qp.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
