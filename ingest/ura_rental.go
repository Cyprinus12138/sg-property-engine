package ingest

import (
	"context"
	"log/slog"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

type URARentalIngestor struct {
	URA   *sources.URA
	Store *store.Store
}

func (u *URARentalIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	var backQuarterNum int
	switch mode {
	case store.ModeBackfill:
		backQuarterNum = 20 // 5 yeara = 5 * 4 = 20
	default:
		backQuarterNum = 2
	}
	report.Report("fetching", 0)
	records, err := u.URA.InvokeQuarters(ctx, sources.ServiceURAResiRental, backQuarterNum)
	if err != nil {
		return 0, 0, err
	}
	report.Report("parsing", 50)
	rows := parseURARentalRecords(records)
	slog.Info("ura_rental parsed", "records", len(records), "rows", len(rows))
	report.Report("upserting", 75)
	return u.Store.UpsertURARentals(ctx, rows)
}

func parseURARentalRecords(records []map[string]any) []store.URARentalRow {
	var out []store.URARentalRow
	for _, rec := range records {
		project := getStr(rec, "project")
		street := getStr(rec, "street")
		rentals, ok := rec["rental"].([]any)
		if !ok {
			continue
		}
		for _, rAny := range rentals {
			r, ok := rAny.(map[string]any)
			if !ok {
				continue
			}
			row := store.URARentalRow{
				Project:      project,
				Street:       street,
				District:     getStr(r, "district"),
				PropertyType: getStr(r, "propertyType"),
			}
			if lo, hi, ok := parseRange(r, "areaSqm"); ok {
				row.AreaSqmMin, row.AreaSqmMax = lo, hi
			}
			row.Rent = parseFloat(r, "rent")
			row.BedroomNum = parseFloat(r, "noOfBedRoom")
			if v := getStr(r, "leaseDate"); v != "" {
				if d, ok := parseURAContractDate(v); ok {
					row.LeaseDate = d
				}
			}
			if row.LeaseDate.IsZero() || row.Rent == 0 {
				continue
			}
			out = append(out, row)
		}
	}
	return out
}
