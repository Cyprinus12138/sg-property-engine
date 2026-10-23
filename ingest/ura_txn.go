package ingest

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

type URATxnIngestor struct {
	URA   *sources.URA
	Store *store.Store
}

// Run fetches all 4 batches of PMI_Resi_Transaction and upserts. URA doesn't
// expose a delta filter, so incremental and backfill are the same operation;
// idempotency comes from source_hash.
//
// After the txn upsert, we rebuild the projects master from the distinct
// project names in ura_transactions — same pattern as HDBIngestor rebuilding
// hdb_blocks after its resale upsert. Geocoding is still a separate step
// handled by GeoIngestor.
func (u *URATxnIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	report.Report("fetching", 0)
	records, err := u.URA.InvokeAllBatches(ctx, sources.ServiceURAResiTransaction)
	if err != nil {
		return 0, 0, err
	}
	report.Report("parsing", 50)
	rows := parseURATxnRecords(records)
	slog.Info("ura_txn parsed", "records", len(records), "rows", len(rows))
	report.Report("upserting", 75)
	in, up, err := u.Store.UpsertURATransactions(ctx, rows)
	if err != nil {
		return in, up, err
	}
	report.Report("rebuilding projects", 95)
	n, err := u.Store.RebuildProjectsFromURA(ctx)
	if err != nil {
		return in, up, err
	}
	slog.Info("projects rebuilt", "rows", n)
	return in, up, nil
}

// URA groups transactions under a project, with a nested "transaction" array.
// Flatten that structure into one row per transaction.
func parseURATxnRecords(records []map[string]any) []store.URATransactionRow {
	var out []store.URATransactionRow
	for _, rec := range records {
		project := getStr(rec, "project")
		street := getStr(rec, "street")
		marketSeg := getStr(rec, "marketSegment")
		typeOfArea := getStr(rec, "typeOfArea")
		_ = typeOfArea
		txns, ok := rec["transaction"].([]any)
		if !ok {
			continue
		}
		for _, tAny := range txns {
			t, ok := tAny.(map[string]any)
			if !ok {
				continue
			}
			row := store.URATransactionRow{
				Project:       project,
				Street:        street,
				District:      getStr(t, "district"),
				Tenure:        getStr(t, "tenure"),
				MarketSegment: marketSeg,
				PropertyType:  getStr(t, "propertyType"),
				FloorRange:    getStr(t, "floorRange"),
				TypeOfSale:    getStr(t, "typeOfSale"),
			}
			row.AreaSqm = parseFloat(t, "area")
			row.Price = parseFloat(t, "price")
			if row.AreaSqm > 0 {
				row.PSF = row.Price / (row.AreaSqm * 10.7639)
			}
			if v := getStr(t, "contractDate"); v != "" {
				if d, ok := parseURAContractDate(v); ok {
					row.ContractDate = d
				}
			}
			if row.ContractDate.IsZero() || row.Price == 0 {
				continue
			}
			out = append(out, row)
		}
	}
	return out
}

// URA contractDate format is "MMYY", e.g. "0324" = March 2024. Day defaults
// to the 1st since the API only exposes month precision.
func parseURAContractDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return time.Time{}, false
	}
	mm, err1 := strconv.Atoi(s[:2])
	yy, err2 := strconv.Atoi(s[2:])
	if err1 != nil || err2 != nil {
		return time.Time{}, false
	}
	year := 2000 + yy
	return time.Date(year, time.Month(mm), 1, 0, 0, 0, 0, time.UTC), true
}
