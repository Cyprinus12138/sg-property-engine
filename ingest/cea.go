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

// CEAIngestor pulls the CEA salesperson register CSV from data.gov.sg and
// upserts it into cea_salespersons. The source publishes a full snapshot on
// every refresh, so incremental and backfill share the same code path —
// idempotency comes from the source_hash unique index.
type CEAIngestor struct {
	DG    *sources.DataGov
	Store *store.Store
}

func (c *CEAIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	report.Report("downloading", 0)
	body, err := c.DG.DownloadDatasetCSV(ctx, sources.CEASalespersonResourceID)
	if err != nil {
		return 0, 0, fmt.Errorf("cea download: %w", err)
	}
	defer body.Close()

	r := csv.NewReader(body)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return 0, 0, fmt.Errorf("cea csv header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[normaliseCEAHeader(name)] = i
	}

	const batchSize = ceaSalespersonBatchSize
	batch := make([]store.CEASalespersonRow, 0, batchSize)
	totalIn, totalUp := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		in, up, err := c.Store.UpsertCEASalespersons(ctx, batch)
		if err != nil {
			return err
		}
		totalIn += in
		totalUp += up
		slog.Info("cea batch", "got", len(batch), "upserted", up, "total_upserted", totalUp)
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
			return totalIn, totalUp, fmt.Errorf("cea csv read: %w", err)
		}
		row, ok := parseCEARecord(rec, idx)
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
	slog.Info("cea ingest done", "mode", mode, "rows_in", totalIn, "rows_upserted", totalUp)
	return totalIn, totalUp, nil
}

// normaliseCEAHeader lower-cases, trims, and converts underscores to spaces
// so the field map matches both the historical "Salesperson Name" header and
// the current "salesperson_name" form data.gov.sg now publishes.
func normaliseCEAHeader(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", " ")
}

func parseCEARecord(rec []string, idx map[string]int) (store.CEASalespersonRow, bool) {
	get := func(key string) string {
		if i, ok := idx[key]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	row := store.CEASalespersonRow{
		SalespersonName:      get("salesperson name"),
		RegistrationNo:       get("registration no"),
		EstateAgentName:      get("estate agent name"),
		EstateAgentLicenseNo: get("estate agent license no"),
	}
	if v := get("registration start date"); v != "" {
		if t, ok := parseCEADate(v); ok {
			row.RegistrationStartDate = t
		}
	}
	if v := get("registration end date"); v != "" {
		if t, ok := parseCEADate(v); ok {
			row.RegistrationEndDate = t
		}
	}
	if row.RegistrationNo == "" || row.SalespersonName == "" {
		return row, false
	}
	return row, true
}

// parseCEADate accepts the published DD/MM/YYYY layout and falls back to a
// few common variants in case the upstream format drifts.
func parseCEADate(s string) (time.Time, bool) {
	for _, layout := range []string{"02/01/2006", "2/1/2006", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
