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

// URADeveloperSalesIngestor pulls PMI_Resi_Developer_Sales month-by-month.
// URA's developer-sales endpoint takes a single mmyy refPeriod per call and
// exposes ~3 years of history; backfill walks 36 months, incremental walks 3.
type URADeveloperSalesIngestor struct {
	URA   *sources.URA
	Store *store.Store
}

func (u *URADeveloperSalesIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	monthNum := 3
	if mode == store.ModeBackfill {
		monthNum = 36
	}
	report.Report("fetching", 0)
	records, err := u.URA.InvokeMonths(ctx, sources.ServiceURAResiDeveloperSale, monthNum)
	if err != nil {
		return 0, 0, err
	}
	report.Report("parsing", 60)
	rows := parseURADeveloperSalesRecords(records)
	slog.Info("ura_developer_sales parsed", "records", len(records), "rows", len(rows))
	report.Report("upserting", 85)
	return u.Store.UpsertURADeveloperSales(ctx, rows)
}

// URA returns one project per call with a `developerSales` array carrying the
// per-refPeriod numbers. The same project may appear in multiple monthly
// pulls; we flatten to one row per (project, refPeriod) and let the store
// upsert dedupe.
func parseURADeveloperSalesRecords(records []map[string]any) []store.URADeveloperSalesRow {
	var out []store.URADeveloperSalesRow
	for _, rec := range records {
		project := getStr(rec, "project")
		if project == "" {
			continue
		}
		base := store.URADeveloperSalesRow{
			Project:       project,
			Developer:     getStr(rec, "developer"),
			Street:        getStr(rec, "street"),
			District:      getStr(rec, "district"),
			MarketSegment: getStr(rec, "marketSegment"),
			PropertyType:  getStr(rec, "propertyType"),
		}
		sales, ok := rec["developerSales"].([]any)
		if !ok {
			continue
		}
		for _, sAny := range sales {
			s, ok := sAny.(map[string]any)
			if !ok {
				continue
			}
			row := base
			period := getStr(s, "refPeriod")
			d, ok := parseURAMonthPeriod(period)
			if !ok {
				continue
			}
			row.RefPeriod = d
			row.UnitsAvail = parseInt(s, "unitsAvail")
			row.LaunchedToDate = parseInt(s, "launchedToDate")
			row.SoldToDate = parseInt(s, "soldToDate")
			row.LaunchedInMonth = parseInt(s, "launchedInMonth")
			row.SoldInMonth = parseInt(s, "soldInMonth")
			row.LowestPrice = parseFloat(s, "lowestPrice")
			row.MedianPrice = parseFloat(s, "medianPrice")
			row.HighestPrice = parseFloat(s, "highestPrice")
			out = append(out, row)
		}
	}
	return out
}

// parseURAMonthPeriod parses URA's mmyy refPeriod (e.g. "0913" = Sep 2013) to
// the 1st of that month in UTC. Same {2 digit month}{2 digit year} convention
// used by parseURAContractDate, but kept separate so an upstream typo in
// developer-sales doesn't silently parse as a contract date.
func parseURAMonthPeriod(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return time.Time{}, false
	}
	mm, err1 := strconv.Atoi(s[:2])
	yy, err2 := strconv.Atoi(s[2:])
	if err1 != nil || err2 != nil || mm < 1 || mm > 12 {
		return time.Time{}, false
	}
	return time.Date(2000+yy, time.Month(mm), 1, 0, 0, 0, 0, time.UTC), true
}

func parseInt(m map[string]any, k string) int {
	s := strings.TrimSpace(getStr(m, k))
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}
