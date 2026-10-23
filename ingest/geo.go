package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

// geocodeThrottle is the pause between successive geocode resolves in
// geocodeBlocks / geocodeProjects. Keeps us well under OneMap's and
// Google Maps' per-second rate limits on large first runs.
const geocodeThrottle = 150 * time.Millisecond

type GeoIngestor struct {
	Store    *store.Store
	Resolver *GeoResolver
	// Max blocks/projects to geocode per run — OneMap is unauth'd but we
	// still don't want a runaway first run hammering it.
	PerRunLimit int
}

// Run geocodes newly-ingested HDB blocks and URA projects that don't yet
// have coordinates. Project geocoding also discovers per-building records
// from the same OneMap response.
//
// In incremental mode it processes up to PerRunLimit records and stops.
// In backfill mode it loops in PerRunLimit-sized batches until every
// record needing geocoding has been processed.
func (g *GeoIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	limit := g.PerRunLimit
	if limit <= 0 {
		limit = geoIngestBatchSize
	}

	// Count totals upfront so progress can be reported against the real total.
	totalBlocks, err := g.Store.CountBlocksNeedingGeocode(ctx)
	if err != nil {
		return 0, 0, err
	}
	totalProjects, err := g.Store.CountProjectsNeedingGeocode(ctx)
	if err != nil {
		return 0, 0, err
	}
	grand := totalBlocks + totalProjects

	if mode != store.ModeBackfill {
		capped := min(totalBlocks, limit) + min(totalProjects, limit)
		blockIn, blockUp, err := g.geocodeBlocks(ctx, limit, 0, capped, report)
		if err != nil {
			return blockIn, blockUp, err
		}
		projIn, projUp, err := g.geocodeProjects(ctx, limit, blockIn, capped, report)
		if err != nil {
			return blockIn + projIn, blockUp + projUp, err
		}
		return blockIn + projIn, blockUp + projUp, nil
	}

	// Backfill: drain all blocks, then all projects, in limit-sized batches.
	totalIn, totalUp := 0, 0

	for {
		n, up, err := g.geocodeBlocks(ctx, limit, totalIn, grand, report)
		totalIn += n
		totalUp += up
		if err != nil {
			return totalIn, totalUp, err
		}
		if n == 0 {
			break
		}
		slog.Info("geo backfill blocks batch", "batch", n, "total_in", totalIn)
	}

	for {
		n, up, err := g.geocodeProjects(ctx, limit, totalIn, grand, report)
		totalIn += n
		totalUp += up
		if err != nil {
			return totalIn, totalUp, err
		}
		if n == 0 {
			break
		}
		slog.Info("geo backfill projects batch", "batch", n, "total_in", totalIn)
	}

	slog.Info("geo backfill complete", "total_in", totalIn, "total_up", totalUp)
	return totalIn, totalUp, nil
}

// geocodeBlocks resolves HDB blocks via OneMap. Matches the result whose
// BLK_NO and ROAD_NAME match the block/street to extract the correct
// BUILDING (estate name) into hdb_blocks.name.
func (g *GeoIngestor) geocodeBlocks(ctx context.Context, limit, done, grand int, report store.ProgressReporter) (int, int, error) {
	pairs, err := g.Store.DistinctBlocksNeedingGeocode(ctx, limit)
	if err != nil {
		return 0, 0, err
	}
	upserted := 0
	for i, p := range pairs {
		report.Report("geocoding blocks", float64(done+i)/float64(max(grand, 1))*100)
		street, block := p[0], p[1]
		query := strings.TrimSpace(block + " " + street)

		// Use ResolveAllOneMap so we can match on BLK_NO + ROAD_NAME and
		// extract the BUILDING name for the HDB estate.
		results := g.Resolver.ResolveAllOneMap(ctx, query)

		var lat, lng float64
		var postal, buildingName string

		if matched := matchHDBBlock(results, block, street); matched != nil {
			lat, lng, postal, buildingName = matched.Lat, matched.Lng, matched.Postal, matched.BuildingName
		} else if len(results) > 0 {
			// Fall back to first result with a usable postal.
			for _, r := range results {
				if hasUsablePostal(r.Postal) {
					lat, lng, postal = r.Lat, r.Lng, r.Postal
					buildingName = r.BuildingName
					break
				}
			}
			if lat == 0 && lng == 0 {
				lat, lng = results[0].Lat, results[0].Lng
				buildingName = results[0].BuildingName
			}
		} else {
			slog.Warn("geo block: no OneMap results, skipping until next run", "block", block, "street", street)
			continue
		}

		if strings.EqualFold(buildingName, "NIL") {
			buildingName = fmt.Sprintf("BLK %s %s", block, street)
		}
		if err := g.Store.SetHDBBlockGeo(ctx, street, block, postal, lat, lng, "", buildingName); err != nil {
			return len(pairs), upserted, err
		}
		upserted++
		time.Sleep(geocodeThrottle)
	}
	slog.Info("geo blocks", "in", len(pairs), "upserted", upserted)
	return len(pairs), upserted, nil
}

// matchHDBBlock finds the OneMap result whose BLK_NO and ROAD_NAME match
// the HDB block and street. OneMap ROAD_NAME is uppercase and may have
// slight formatting differences, so we normalise before comparing.
func matchHDBBlock(results []GeoResult, block, street string) *GeoResult {
	normBlock := strings.TrimSpace(strings.ToUpper(block))
	normStreet := strings.TrimSpace(strings.ToUpper(street))
	for i := range results {
		r := &results[i]
		if strings.TrimSpace(strings.ToUpper(r.BlkNo)) == normBlock &&
			strings.TrimSpace(strings.ToUpper(r.RoadName)) == normStreet {
			return r
		}
	}
	return nil
}

// geocodeProjects resolves private projects via OneMap and simultaneously
// discovers per-building records from the same response. This avoids a
// separate building-discovery pass that would duplicate the API call.
func (g *GeoIngestor) geocodeProjects(ctx context.Context, limit, done, grand int, report store.ProgressReporter) (int, int, error) {
	projects, err := g.Store.ProjectsNeedingGeocode(ctx, limit)
	if err != nil {
		return 0, 0, err
	}
	upserted := 0
	for i, p := range projects {
		report.Report("geocoding projects", float64(done+i)/float64(max(grand, 1))*100)
		name, street := p[0], p[1]

		// Search OneMap with "project_name street" for best specificity.
		query := strings.TrimSpace(name + " " + street)
		results := g.Resolver.ResolveAllOneMap(ctx, query)

		// Filter to results whose ROAD_NAME matches the URA street.
		normStreet := strings.TrimSpace(strings.ToUpper(street))
		matched := filterByStreet(results, normStreet)

		var lat, lng float64
		var postal string

		if len(matched) > 0 {
			// Use first matched result for project-level geo.
			best := pickBestResult(matched)
			lat, lng, postal = best.Lat, best.Lng, best.Postal

			// Extract buildings: each unique postal = one building.
			buildings := buildingsFromResults(name, matched)
			if len(buildings) > 0 {
				n, err := g.Store.UpsertProjectBuildings(ctx, buildings)
				if err != nil {
					return len(projects), upserted, err
				}
				slog.Info("buildings discovered", "project", name, "buildings", n)
			}
		} else {
			slog.Warn("geo project: no matching OneMap results, skipping until next run", "project", name, "street", street)
			continue
		}

		if err := g.Store.SetProjectGeo(ctx, name, postal, lat, lng, ""); err != nil {
			return len(projects), upserted, err
		}
		upserted++
		time.Sleep(geocodeThrottle)
	}
	slog.Info("geo projects", "in", len(projects), "upserted", upserted)
	return len(projects), upserted, nil
}

// filterByStreet keeps only results whose ROAD_NAME matches the expected
// street. If street is empty, all results are returned.
func filterByStreet(results []GeoResult, normStreet string) []GeoResult {
	if normStreet == "" {
		return results
	}
	var out []GeoResult
	for _, r := range results {
		if strings.TrimSpace(strings.ToUpper(r.RoadName)) == normStreet {
			out = append(out, r)
		}
	}
	return out
}

// pickBestResult returns the first result with a usable postal code, or
// the first result if none have one.
func pickBestResult(results []GeoResult) GeoResult {
	for _, r := range results {
		if hasUsablePostal(r.Postal) {
			return r
		}
	}
	return results[0]
}

// buildingsFromResults deduplicates OneMap results by postal code and
// returns one ProjectBuildingRow per unique building.
func buildingsFromResults(project string, results []GeoResult) []store.ProjectBuildingRow {
	seen := make(map[string]bool)
	var out []store.ProjectBuildingRow
	for _, r := range results {
		if !hasUsablePostal(r.Postal) || seen[r.Postal] {
			continue
		}
		seen[r.Postal] = true
		out = append(out, store.ProjectBuildingRow{
			Project:      project,
			BlkNo:        r.BlkNo,
			RoadName:     r.RoadName,
			BuildingName: r.BuildingName,
			Postal:       r.Postal,
			Lat:          r.Lat,
			Lng:          r.Lng,
		})
	}
	return out
}
