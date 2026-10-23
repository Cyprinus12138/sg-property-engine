package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ResolveAnchor looks up a lat/lng for a geo filter anchor.
//
//	kind="project"  , a=project name
//	kind="poi"      , a=poi type, b=poi name (b may be "" — picks any of that type)
//	kind="hdb_block", a=street, b=block
func (s *Store) ResolveAnchor(ctx context.Context, kind, a, b string) (float64, float64, bool, error) {
	var lat, lng float64
	var err error
	switch kind {
	case "project":
		err = s.Pool.QueryRow(ctx, `
			SELECT lat, lng FROM projects
			 WHERE name = $1 AND lat IS NOT NULL AND lng IS NOT NULL
			 LIMIT 1`, a).Scan(&lat, &lng)
	case "poi":
		if b == "" {
			err = s.Pool.QueryRow(ctx, `
				SELECT lat, lng FROM pois
				 WHERE type = $1
				 ORDER BY name LIMIT 1`, a).Scan(&lat, &lng)
		} else {
			err = s.Pool.QueryRow(ctx, `
				SELECT lat, lng FROM pois
				 WHERE type = $1 AND name ILIKE $2
				 ORDER BY name LIMIT 1`, a, b+"%").Scan(&lat, &lng)
		}
	case "hdb_block":
		err = s.Pool.QueryRow(ctx, `
			SELECT lat, lng FROM hdb_blocks
			 WHERE street_name ILIKE $1 AND block = $2 AND lat IS NOT NULL
			 LIMIT 1`, a+"%", b).Scan(&lat, &lng)
	default:
		return 0, 0, false, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return lat, lng, true, nil
}
