package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/Cyprinus12138/sg-property-engine/client"
)

// Near names the point a search is centred on. Exactly one of: lat+lng,
// project, poi_type (+ poi_name), or block+street.
type Near struct {
	Lat     *float64 `json:"lat,omitempty" jsonschema:"latitude, with lng"`
	Lng     *float64 `json:"lng,omitempty" jsonschema:"longitude, with lat"`
	Project string   `json:"project,omitempty" jsonschema:"private project name, e.g. THE SAIL @ MARINA BAY; case and punctuation do not matter"`
	POIType string   `json:"poi_type,omitempty" jsonschema:"POI type, e.g. mrt_exit for an MRT station; see poi_types"`
	POIName string   `json:"poi_name,omitempty" jsonschema:"POI name prefix with poi_type, e.g. BISHAN for Bishan MRT"`
	Block   string   `json:"block,omitempty" jsonschema:"HDB block number, with street, e.g. 123"`
	Street  string   `json:"street,omitempty" jsonschema:"HDB street name, with block, e.g. BISHAN ST 12"`
	RadiusM int      `json:"radius_m,omitempty" jsonschema:"search radius in metres; default 1000, max 5000"`
}

// Anchor is the point a Near resolved to, echoed in results so the caller
// can say exactly what the search was centred on.
type Anchor struct {
	Kind    string  `json:"kind"`
	Name    string  `json:"name,omitempty"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	RadiusM int     `json:"radius_m,omitempty"`
}

// HasPOI keeps only rows with a POI of this type within within_m.
type HasPOI struct {
	Type    string `json:"type" jsonschema:"POI type, e.g. mrt_exit, school, park; see poi_types"`
	Subtype string `json:"subtype,omitempty" jsonschema:"optional POI subtype"`
	WithinM int    `json:"within_m,omitempty" jsonschema:"distance in metres; default 500, max 5000"`
}

// Singapore's bounding box with a margin. Catches swapped lat/lng, which
// otherwise returns an empty result that reads as "nothing nearby".
const (
	sgLatMin, sgLatMax = 1.15, 1.48
	sgLngMin, sgLngMax = 103.59, 104.10
)

// resolveAnchor turns a Near into coordinates through the engine, so the
// engine is only ever given near_lat/near_lng. The HTTP API silently drops
// a near_* filter whose anchor it cannot find; resolving here makes that
// an error the caller sees instead of island-wide results.
func resolveAnchor(ctx context.Context, c *client.Client, n *Near) (*Anchor, error) {
	if n == nil {
		return nil, nil
	}
	kinds := 0
	for _, set := range []bool{n.Lat != nil || n.Lng != nil, n.Project != "", n.POIType != "" || n.POIName != "", n.Block != "" || n.Street != ""} {
		if set {
			kinds++
		}
	}
	if kinds == 0 {
		return nil, nil
	}
	if kinds > 1 {
		return nil, errors.New("near: give exactly one of lat+lng, project, poi_type(+poi_name), or block+street")
	}
	if n.RadiusM < 0 || n.RadiusM > 5000 {
		return nil, fmt.Errorf("near.radius_m %d: must be 1-5000", n.RadiusM)
	}

	a := &Anchor{RadiusM: n.RadiusM}
	switch {
	case n.Lat != nil || n.Lng != nil:
		if n.Lat == nil || n.Lng == nil {
			return nil, errors.New("near: lat and lng go together")
		}
		if *n.Lat < sgLatMin || *n.Lat > sgLatMax || *n.Lng < sgLngMin || *n.Lng > sgLngMax {
			return nil, fmt.Errorf("near: (%g, %g) is outside Singapore; check lat/lng are not swapped", *n.Lat, *n.Lng)
		}
		a.Kind, a.Lat, a.Lng = "point", *n.Lat, *n.Lng

	case n.Project != "":
		p, err := c.Project(ctx, n.Project)
		if errors.Is(err, client.ErrNotFound) {
			return nil, fmt.Errorf("near.project: no project named %q; use search_properties to find the exact name", n.Project)
		}
		if err != nil {
			return nil, err
		}
		if p.Lat == nil || p.Lng == nil {
			return nil, fmt.Errorf("near.project: %s has no coordinates yet; the geo sync geocodes projects", p.Name)
		}
		a.Kind, a.Name, a.Lat, a.Lng = "project", p.Name, *p.Lat, *p.Lng

	case n.POIType != "":
		q := url.Values{"type": {n.POIType}, "limit": {"1"}}
		if n.POIName != "" {
			q.Set("name", normUpper(n.POIName))
		}
		page, err := c.POIs(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(page.Rows) == 0 {
			return nil, fmt.Errorf("near: no %s POI named %q; use find_pois or poi_types to check", n.POIType, n.POIName)
		}
		p := page.Rows[0]
		a.Kind, a.Name, a.Lat, a.Lng = p.Type, p.Name, p.Lat, p.Lng

	case n.POIName != "":
		return nil, errors.New("near.poi_name needs poi_type, e.g. mrt_exit")

	default:
		if n.Block == "" || n.Street == "" {
			return nil, errors.New("near: block and street go together")
		}
		q := url.Values{"block": {normUpper(n.Block)}, "street": {normUpper(n.Street)}, "limit": {"1"}}
		page, err := c.HDBTransactions(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(page.Rows) == 0 {
			return nil, fmt.Errorf("near: no HDB block %s on a street starting %q; use search_properties", n.Block, n.Street)
		}
		r := page.Rows[0]
		if r.Lat == nil || r.Lng == nil {
			return nil, fmt.Errorf("near: block %s %s has no coordinates yet; the geo sync geocodes HDB blocks", r.Block, r.Street)
		}
		a.Kind, a.Name, a.Lat, a.Lng = "hdb_block", r.Block+" "+r.Street, *r.Lat, *r.Lng
	}
	return a, nil
}

// addGeo writes the resolved anchor and has-POI filter into q.
func addGeo(q url.Values, a *Anchor, h *HasPOI) error {
	if a != nil {
		q.Set("near_lat", strconv.FormatFloat(a.Lat, 'f', 6, 64))
		q.Set("near_lng", strconv.FormatFloat(a.Lng, 'f', 6, 64))
		if a.RadiusM > 0 {
			q.Set("radius_m", strconv.Itoa(a.RadiusM))
		}
	}
	if h != nil {
		if h.Type == "" {
			return errors.New("has_poi.type is required")
		}
		if h.WithinM < 0 || h.WithinM > 5000 {
			return fmt.Errorf("has_poi.within_m %d: must be 1-5000", h.WithinM)
		}
		q.Set("has_poi_type", h.Type)
		setIf(q, "has_poi_subtype", h.Subtype)
		if h.WithinM > 0 {
			q.Set("has_poi_within_m", strconv.Itoa(h.WithinM))
		}
	}
	return nil
}

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func setNum(q url.Values, k string, v *float64) {
	if v != nil {
		q.Set(k, strconv.FormatFloat(*v, 'f', -1, 64))
	}
}
