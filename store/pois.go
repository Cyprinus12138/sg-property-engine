package store

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrEmptyUpstream is returned by SyncPOIsBySource when the caller supplies
// zero rows. The sync is skipped to preserve the existing table data rather
// than wiping it on a transient upstream failure.
var ErrEmptyUpstream = errors.New("upstream returned 0 rows; sync skipped to preserve existing data")

type POIRow struct {
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Subtype string         `json:"subtype"`
	Lat     float64        `json:"lat"`
	Lng     float64        `json:"lng"`
	Attrs   map[string]any `json:"attrs"`
	Source  string         `json:"source"`
}

// HashPOI produces a stable content hash for a POI row. Source is included so
// that identical locations from different sources never collide on the unique
// index.
func HashPOI(r POIRow) []byte {
	h := md5.New()
	fmt.Fprintf(h, "poi|%s|%s|%s|%s|%.6f|%.6f", r.Source, r.Type, r.Name, r.Subtype, r.Lat, r.Lng)
	return h.Sum(nil)
}

// SyncPOIsBySource upserts all supplied rows for the given source, then prunes
// any existing rows for that source that are absent from the new batch. Both
// operations run in one transaction so readers always see a consistent snapshot.
//
// Returns (upstream, changed, pruned):
//   - upstream: len(rows) — rows received from the API
//   - changed:  rows inserted or updated (content actually differed)
//   - pruned:   rows deleted because they disappeared from upstream
func (s *Store) SyncPOIsBySource(ctx context.Context, source string, rows []POIRow) (int, int, int, error) {
	if len(rows) == 0 {
		return 0, 0, 0, ErrEmptyUpstream
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback(ctx)

	const stmt = `
		INSERT INTO pois
		  (type, name, subtype, lat, lng, geom, attrs, source, source_hash)
		VALUES ($1, $2, NULLIF($3,''), $4, $5,
		        ST_SetSRID(ST_MakePoint($5, $4), 4326)::geography,
		        $6, $7, $8)
		ON CONFLICT (source_hash) DO UPDATE SET
		    name        = EXCLUDED.name,
		    attrs       = EXCLUDED.attrs,
		    ingested_at = now()
		WHERE pois.name  IS DISTINCT FROM EXCLUDED.name
		   OR pois.attrs IS DISTINCT FROM EXCLUDED.attrs`

	hashes := make([][]byte, 0, len(rows))
	changed := 0
	for _, r := range rows {
		attrs, _ := json.Marshal(r.Attrs)
		h := HashPOI(r)
		hashes = append(hashes, h)
		tag, err := tx.Exec(ctx, stmt,
			r.Type, r.Name, r.Subtype, r.Lat, r.Lng, attrs, r.Source, h)
		if err != nil {
			return 0, 0, 0, err
		}
		changed += int(tag.RowsAffected())
	}

	// Remove rows that were present in the previous snapshot but dropped from
	// upstream. The NOT ALL check is efficient because source_hash is indexed.
	tag, err := tx.Exec(ctx,
		`DELETE FROM pois WHERE source = $1 AND source_hash != ALL($2)`,
		source, hashes)
	if err != nil {
		return 0, 0, 0, err
	}
	pruned := int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, err
	}
	return len(rows), changed, pruned, nil
}
