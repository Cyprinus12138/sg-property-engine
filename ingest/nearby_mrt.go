package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

// NearbyMRTIngestor fills the projects.nearby_mrt column by querying the
// pois table per project. Runs after the pois ingest (which keeps the
// mrt_exit rows + station codes/lines fresh) and any URA ingest (which
// keeps projects.lat/lng fresh).
//
// Pure SQL — no external API calls — so the only failure modes are DB
// errors. Per-project errors are logged and the loop continues; one
// bad project must not stop the whole sweep.
type NearbyMRTIngestor struct {
	Store *store.Store

	// BatchLimit caps how many projects are enriched per Run invocation.
	// Empty/zero defaults to 500, which is enough to enrich all of
	// Singapore's residential projects in a few seconds. Useful for
	// throttling if we ever scale to a much larger project set.
	BatchLimit int

	// Limit + WithinMeters control the NearestStations query for each
	// project. Defaults: top-3 stations always returned, plus any
	// additional stations within 1km. Matches the product spec.
	Limit        int
	WithinMeters int
}

// Run is the scheduler entry point. Reports progress via the supplied
// reporter so the operator UI shows a moving bar; matches the other
// ingestors' contract.
func (i *NearbyMRTIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	if i.Store == nil {
		return 0, 0, fmt.Errorf("nearby_mrt: store not configured")
	}
	limit := i.Limit
	if limit <= 0 {
		limit = store.DefaultNearestStationsLimit
	}
	within := i.WithinMeters
	if within <= 0 {
		within = 1000
	}
	batch := i.BatchLimit
	if batch <= 0 {
		batch = 500
	}

	started := time.Now()

	// mode=backfill skips the "newer than pois.ingested_at" freshness
	// gate AND fetches every project in a single query (LIMIT 0 in the
	// store helper). Without the single-query shape, force mode would
	// loop on the same first N projects forever — there's no
	// watermark to advance them out of the result set when the
	// freshness check is disabled.
	//
	// Incremental mode keeps the batched-loop semantics: each
	// UpdateProjectNearbyMRT bumps nearby_mrt_synced_at past pois.ts,
	// so the next query returns a fresh slice. Bounded by maxBatches
	// to defend against a pathological always-dirty condition (e.g.
	// pois ingest running concurrently and bumping its ingested_at on
	// every tick).
	force := mode == store.ModeBackfill
	const maxBatches = 50

	var totalSeen, totalWritten, totalFailed int
	processBatch := func(projects []store.ProjectNeedingMRT, pass int) {
		if pass == 0 {
			slog.Info("nearby_mrt: enriching",
				"size", len(projects), "limit", limit, "within_m", within, "force", force)
		} else {
			slog.Info("nearby_mrt: next batch", "pass", pass, "size", len(projects))
		}
		totalSeen += len(projects)
		for idx, p := range projects {
			if ctx.Err() != nil {
				break
			}
			stations, err := i.Store.NearestStations(ctx, p.Lat, p.Lng, limit, within)
			if err != nil {
				slog.Warn("nearby_mrt: nearest stations failed",
					"project", p.Name, "err", err)
				totalFailed++
				continue
			}
			if err := i.Store.UpdateProjectNearbyMRT(ctx, p.Name, stations); err != nil {
				slog.Warn("nearby_mrt: update failed",
					"project", p.Name, "err", err)
				totalFailed++
				continue
			}
			totalWritten++
			if (idx+1)%50 == 0 || idx == len(projects)-1 {
				report.Report("nearby_mrt", float64(idx+1)/float64(len(projects))*100)
			}
		}
	}

	if force {
		// One query, no LIMIT, processes everything.
		projects, err := i.Store.ProjectsNeedingMRTEnrichment(ctx, 0, true)
		if err != nil {
			return 0, 0, fmt.Errorf("list projects needing mrt: %w", err)
		}
		if len(projects) > 0 {
			processBatch(projects, 0)
		}
	} else {
		for pass := 0; pass < maxBatches; pass++ {
			if ctx.Err() != nil {
				break
			}
			projects, err := i.Store.ProjectsNeedingMRTEnrichment(ctx, batch, false)
			if err != nil {
				return totalSeen, totalWritten, fmt.Errorf("list projects needing mrt: %w", err)
			}
			if len(projects) == 0 {
				break
			}
			processBatch(projects, pass)
			if len(projects) < batch {
				break
			}
		}
	}

	if totalSeen == 0 {
		slog.Info("nearby_mrt: nothing to enrich")
		return 0, 0, nil
	}
	slog.Info("nearby_mrt: enrichment complete",
		"written", totalWritten, "failed", totalFailed,
		"seen", totalSeen,
		"duration_ms", time.Since(started).Milliseconds())
	return totalSeen, totalWritten, nil
}
