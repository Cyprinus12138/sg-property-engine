package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

type URAPipelineIngestor struct {
	URA   *sources.URA
	Store *store.Store
}

func (u *URAPipelineIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	report.Report("fetching", 0)
	body, err := u.URA.Invoke(ctx, sources.ServiceURAResiPipeline, nil)
	if err != nil {
		return 0, 0, err
	}
	var parsed sources.URAResiPipelineEnvelope
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, 0, err
	}
	report.Report("refreshing", 60)
	rows := parseURAPipelineRecords(parsed.Result)
	slog.Info("ura_pipeline parsed", "records", len(parsed.Result), "rows", len(rows))
	total, upserted, err := u.Store.RefreshURAPipeline(ctx, rows)
	if err != nil {
		return total, upserted, err
	}
	// Merge pipeline projects into the master `projects` table so new
	// launches are addressable by name before they accumulate transactions.
	// Failures here are logged but not fatal — the pipeline snapshot itself
	// is the primary deliverable.
	report.Report("merging projects", 90)
	ins, upd, skip, mergeErr := u.Store.UpsertProjectsFromPipeline(ctx, rows)
	if mergeErr != nil {
		slog.Warn("ura_pipeline → projects upsert failed", "err", mergeErr)
	} else {
		slog.Info("ura_pipeline → projects upsert", "inserted", ins, "updated", upd, "skipped_placeholder", skip)
	}
	return total, upserted, nil
}

func parseURAPipelineRecords(records []map[string]any) []store.URAPipelineRow {
	out := make([]store.URAPipelineRow, 0, len(records))
	for _, rec := range records {
		row := store.URAPipelineRow{
			Project:       getStr(rec, "project"),
			Developer:     getStr(rec, "developerName"),
			Street:        getStr(rec, "street"),
			District:      getStr(rec, "district"),
			MarketSegment: getStr(rec, "marketSegment"),
			ExpectedTop:   getStr(rec, "expectedTOPYear"),
			Status:        getStr(rec, "status"),
			Raw:           rec,
		}
		if v := getStr(rec, "totalUnits"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				row.TotalUnits = n
			}
		}
		if v := getStr(rec, "cumUnitsSold"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				row.UnitsSold = n
			}
		}
		if row.Project == "" {
			continue
		}
		out = append(out, row)
	}
	return out
}
