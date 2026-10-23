package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type GeoCacheEntry struct {
	Query        string         `json:"query"`
	Postal       string         `json:"postal"`
	Lat          float64        `json:"lat"`
	Lng          float64        `json:"lng"`
	PlanningArea string         `json:"planning_area"`
	Raw          map[string]any `json:"raw"`
	FetchedAt    time.Time      `json:"fetched_at"`
}

func (s *Store) GetGeoCache(ctx context.Context, query string) (*GeoCacheEntry, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT query, COALESCE(postal,''), COALESCE(lat,0), COALESCE(lng,0),
		       COALESCE(planning_area,''), raw, fetched_at
		  FROM geo_cache WHERE query = $1`, query)
	var e GeoCacheEntry
	var raw []byte
	if err := row.Scan(&e.Query, &e.Postal, &e.Lat, &e.Lng, &e.PlanningArea, &raw, &e.FetchedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &e.Raw)
	}
	return &e, nil
}

func (s *Store) PutGeoCache(ctx context.Context, e GeoCacheEntry) error {
	raw, _ := json.Marshal(e.Raw)
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO geo_cache (query, postal, lat, lng, planning_area, raw, fetched_at)
		VALUES ($1, NULLIF($2,''), $3, $4, NULLIF($5,''), $6, now())
		ON CONFLICT (query) DO UPDATE SET
		    postal        = EXCLUDED.postal,
		    lat           = EXCLUDED.lat,
		    lng           = EXCLUDED.lng,
		    planning_area = EXCLUDED.planning_area,
		    raw           = EXCLUDED.raw,
		    fetched_at    = now()`,
		e.Query, e.Postal, e.Lat, e.Lng, e.PlanningArea, raw)
	return err
}
