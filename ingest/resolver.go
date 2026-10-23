package ingest

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// GeoResolver turns a free-text query into (lat, lng, postal) using OneMap
// first and Google Maps Places as a fallback, writing the result (including
// negatives) into geo_cache so repeated lookups are free. It is shared by
// GeoIngestor (blocks / projects) and POIIngestor (schools) so both paths
// hit the same cache and selection rules.
type GeoResolver struct {
	OneMap     *sources.OneMap
	GoogleMaps *sources.GoogleMaps
	Store      *store.Store
}

// GeoResult is a single resolved location from OneMap or Google Maps.
type GeoResult struct {
	Lat          float64
	Lng          float64
	Postal       string
	BlkNo        string
	RoadName     string
	BuildingName string
}

// Resolve consults geo_cache first; on miss it calls OneMap, then falls
// back to Google Maps Places API if OneMap errors or returns no usable
// result. Results are cached (including negatives) so we don't retry on
// every run.
func (r *GeoResolver) Resolve(ctx context.Context, query string) (lat, lng float64, postal string) {
	cached, _ := r.Store.GetGeoCache(ctx, query)
	if cached != nil {
		return cached.Lat, cached.Lng, cached.Postal
	}
	lat, lng, postal, ok := r.resolveOneMap(ctx, query)
	if !ok {
		lat, lng, postal, ok = r.resolveGoogleMaps(ctx, query)
	}
	if !ok {
		_ = r.Store.PutGeoCache(ctx, store.GeoCacheEntry{Query: query})
		return 0, 0, ""
	}
	_ = r.Store.PutGeoCache(ctx, store.GeoCacheEntry{
		Query: query, Lat: lat, Lng: lng, Postal: postal,
	})
	return lat, lng, postal
}

// ResolveAllOneMap searches OneMap and returns ALL results with valid
// coordinates, not just the first. Does NOT fall back to Google Maps —
// building discovery needs structured OneMap fields (BLK_NO, ROAD_NAME,
// BUILDING) that Google doesn't provide. Results are not cached in
// geo_cache because they are multi-valued.
func (r *GeoResolver) ResolveAllOneMap(ctx context.Context, query string) []GeoResult {
	results, err := r.OneMap.Search(ctx, query)
	if err != nil || len(results) == 0 {
		return nil
	}
	var out []GeoResult
	for _, row := range results {
		la, _ := strconv.ParseFloat(row.Latitude, 64)
		ln, _ := strconv.ParseFloat(row.Longitude, 64)
		if la == 0 && ln == 0 {
			continue
		}
		out = append(out, GeoResult{
			Lat:          la,
			Lng:          ln,
			Postal:       row.Postal,
			BlkNo:        row.BlkNo,
			RoadName:     row.Road,
			BuildingName: row.Building,
		})
	}
	return out
}

// hasUsablePostal reports whether a postal-code string from OneMap /
// Google Maps is a real postal code. OneMap in particular returns "NIL"
// for rows without a postal (e.g. many MRT station rows).
func hasUsablePostal(p string) bool {
	p = strings.TrimSpace(p)
	return p != "" && !strings.EqualFold(p, "NIL")
}

func (r *GeoResolver) resolveOneMap(ctx context.Context, query string) (lat, lng float64, postal string, ok bool) {
	results, err := r.OneMap.Search(ctx, query)
	if err != nil || len(results) == 0 {
		return 0, 0, "", false
	}
	// Prefer a result with a real postal code over the arbitrary first
	// row — OneMap often returns multiple rows for the same building and
	// only a subset carry the postal. Fall back to the first row with
	// valid coordinates so callers still get a point.
	var fallback *sources.OneMapResult
	for i := range results {
		row := &results[i]
		la, _ := strconv.ParseFloat(row.Latitude, 64)
		ln, _ := strconv.ParseFloat(row.Longitude, 64)
		if la == 0 && ln == 0 {
			continue
		}
		if hasUsablePostal(row.Postal) {
			return la, ln, row.Postal, true
		}
		if fallback == nil {
			fallback = row
		}
	}
	if fallback == nil {
		return 0, 0, "", false
	}
	la, _ := strconv.ParseFloat(fallback.Latitude, 64)
	ln, _ := strconv.ParseFloat(fallback.Longitude, 64)
	return la, ln, "", true
}

func (r *GeoResolver) resolveGoogleMaps(ctx context.Context, query string) (lat, lng float64, postal string, ok bool) {
	if r.GoogleMaps == nil || !r.GoogleMaps.HasAuth() {
		return 0, 0, "", false
	}
	// Append ", Singapore" when absent so the Places API biases to SG.
	q := query
	if !strings.Contains(strings.ToLower(q), "singapore") {
		q = q + ", Singapore"
	}
	places, err := r.GoogleMaps.SearchText(ctx, q)
	if err != nil || len(places) == 0 {
		return 0, 0, "", false
	}
	var fallback *sources.GoogleMapsPlace
	for i := range places {
		p := &places[i]
		if p.Latitude == 0 && p.Longitude == 0 {
			continue
		}
		if hasUsablePostal(p.PostalCode) {
			slog.Info("geo google fallback", "query", query, "place", p.DisplayName)
			return p.Latitude, p.Longitude, p.PostalCode, true
		}
		if fallback == nil {
			fallback = p
		}
	}
	if fallback == nil {
		return 0, 0, "", false
	}
	slog.Info("geo google fallback", "query", query, "place", fallback.DisplayName, "postal", "missing")
	return fallback.Latitude, fallback.Longitude, "", true
}
