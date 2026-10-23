package ingest

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// CEATransactionsIngestor pulls the residential CEA salesperson transaction
// records CSV from data.gov.sg and appends fresh rows to cea_transactions.
// The source publishes a full snapshot each refresh (~1.3M rows, monthly);
// idempotency is enforced by source_hash UNIQUE so incremental and backfill
// share the same code path.
type CEATransactionsIngestor struct {
	DG    *sources.DataGov
	Store *store.Store
}

func (c *CEATransactionsIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	report.Report("downloading", 0)
	body, err := c.DG.DownloadDatasetCSV(ctx, sources.CEATransactionResourceID)
	if err != nil {
		return 0, 0, fmt.Errorf("cea_txn download: %w", err)
	}
	defer body.Close()

	r := csv.NewReader(body)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return 0, 0, fmt.Errorf("cea_txn csv header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[normaliseCEAHeader(name)] = i
	}

	const batchSize = hdbResaleBatchSize
	batch := make([]store.CEATransactionRow, 0, batchSize)
	totalIn, totalUp := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		in, up, err := c.Store.UpsertCEATransactions(ctx, batch)
		if err != nil {
			return err
		}
		totalIn += in
		totalUp += up
		slog.Info("cea_txn batch", "got", len(batch), "inserted", up, "total_inserted", totalUp)
		report.Report(fmt.Sprintf("ingesting (%d rows)", totalIn), 0)
		batch = batch[:0]
		return nil
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return totalIn, totalUp, fmt.Errorf("cea_txn csv read: %w", err)
		}
		row, ok := parseCEATransactionRecord(rec, idx)
		if !ok {
			continue
		}
		batch = append(batch, row)
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return totalIn, totalUp, err
			}
		}
	}
	if err := flush(); err != nil {
		return totalIn, totalUp, err
	}
	slog.Info("cea_txn ingest done", "mode", mode, "rows_in", totalIn, "rows_inserted", totalUp)
	return totalIn, totalUp, nil
}

func parseCEATransactionRecord(rec []string, idx map[string]int) (store.CEATransactionRow, bool) {
	get := func(key string) string {
		if i, ok := idx[key]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	regNo := get("salesperson reg num")
	if regNo == "" {
		// historical header variant
		regNo = get("registration no")
	}
	row := store.CEATransactionRow{
		SalespersonRegNum: regNo,
		SalespersonName:   get("salesperson name"),
		PropertyType:      cleanCEAPlaceholder(get("property type")),
		TransactionType:   cleanCEAPlaceholder(get("transaction type")),
		Represented:       cleanCEAPlaceholder(get("represented")),
		Town:              cleanCEAPlaceholder(get("town")),
		District:          cleanCEAPlaceholder(get("district")),
		GeneralLocation:   cleanCEAPlaceholder(get("general location")),
	}
	if t, ok := parseCEATransactionDate(get("transaction date")); ok {
		row.TransactionMonth = t
	}
	if row.SalespersonRegNum == "" || row.TransactionMonth.IsZero() {
		return row, false
	}
	return row, true
}

// cleanCEAPlaceholder normalises the dataset's empty-cell convention.
// data.gov.sg publishes "-" (and sometimes "NA" / "N.A.") for missing
// district/general_location/town cells; we store NULL instead so downstream
// queries can use the standard IS NULL semantics.
func cleanCEAPlaceholder(s string) string {
	s = strings.TrimSpace(s)
	switch strings.ToUpper(s) {
	case "", "-", "--", "NA", "N.A.", "N/A":
		return ""
	}
	return s
}

// parseCEATransactionDate accepts the published "MMM-YYYY" form (e.g.
// "OCT-2017") and falls back to a few defensive variants. Returns the first
// of the month, since the source has no day granularity. Go's reference
// month layout is case-sensitive on the abbreviation, so we title-case the
// input first ("OCT-2017" → "Oct-2017").
func parseCEATransactionDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// Title-case the 3-letter month if present (handles "OCT-2017").
	if len(s) >= 3 {
		s = strings.ToUpper(s[:1]) + strings.ToLower(s[1:3]) + s[3:]
	}
	for _, layout := range []string{"Jan-2006", "Jan 2006", "2006-01", "01/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}
