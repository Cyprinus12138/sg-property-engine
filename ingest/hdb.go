package ingest

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

type HDBIngestor struct {
	DG    *sources.DataGov
	Store *store.Store
}

// data.gov.sg CKAN throttles aggressively on bulk paging, so we pace
// requests and back off on transient failures. Values are conservative
// on purpose — the ingest is a background job, throughput doesn't matter.
const (
	hdbPageDelay   = 1500 * time.Millisecond
	hdbMaxAttempts = 5
	hdbBaseBackoff = 2 * time.Second
)

// Run ingests HDB resale transactions. In incremental mode it only pulls the
// last two months (current + previous) and relies on the source_hash upsert
// for idempotency. In backfill mode it pages through the entire dataset.
//
// After the resale upsert we refresh the hdb_blocks master in two passes:
//  1. runPropertyInfo — pulls block-level facts (year_completed, max floor,
//     total dwelling units, residential flag) from data.gov.sg's HDB
//     Property Information dataset. Authoritative for ~10.7k residential
//     blocks, including ones that have never appeared in resale.
//  2. RebuildHDBBlocksFromResale — fallback that adds any (street, block)
//     pair that's in resale but not in PI (rare, e.g. renamed/demolished).
//
// New blocks land with NULL coordinates; GeoIngestor fills them in on a
// subsequent run. The geocoding columns (postal/lat/lng/geom/planning_area/
// name) are deliberately untouched by both passes.
func (h *HDBIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	var (
		in, up int
		err    error
	)
	switch mode {
	case store.ModeBackfill:
		in, up, err = h.runBackfill(ctx, report)
	default:
		in, up, err = h.runIncremental(ctx, report)
	}
	if err != nil {
		return in, up, err
	}
	if err := h.runPropertyInfo(ctx, report); err != nil {
		return in, up, fmt.Errorf("hdb property info: %w", err)
	}
	added, berr := h.Store.RebuildHDBBlocksFromResale(ctx)
	if berr != nil {
		return in, up, fmt.Errorf("hdb blocks rebuild: %w", berr)
	}
	slog.Info("hdb blocks rebuild", "new_blocks", added)
	return in, up, nil
}

// runPropertyInfo pulls block-level facts from data.gov.sg's HDB Property
// Information dataset and upserts into hdb_blocks. Filters to residential
// rows only (~10.7k of ~13.3k total) — non-residential blocks (markets,
// multistorey carparks) live in the same dataset but don't belong in the
// HDB-flat master table.
func (h *HDBIngestor) runPropertyInfo(ctx context.Context, report store.ProgressReporter) error {
	report.Report("property info: fetch", 0)
	const pageSize = hdbPropertyInfoPageSize
	offset, fetched, totalIn, totalNew := 0, 0, 0, 0
	for {
		records, err := h.fetchPropertyInfoPage(ctx, pageSize, offset)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			break
		}
		fetched += len(records)
		report.Report(fmt.Sprintf("property info: ingesting (%d rows)", fetched), 0)
		rows := parseHDBPropertyInfo(records)
		in, newRows, err := h.Store.UpsertHDBPropertyInfo(ctx, rows)
		if err != nil {
			return err
		}
		totalIn += in
		totalNew += newRows
		if len(records) < pageSize {
			break
		}
		offset += pageSize
		if err := sleepCtx(ctx, hdbPageDelay); err != nil {
			return err
		}
	}
	slog.Info("hdb property info done",
		"rows_fetched", fetched, "rows_in", totalIn, "new_blocks", totalNew)
	return nil
}

func (h *HDBIngestor) fetchPropertyInfoPage(ctx context.Context, limit, offset int) ([]map[string]any, error) {
	var lastErr error
	for attempt := 1; attempt <= hdbMaxAttempts; attempt++ {
		records, err := h.DG.DatastoreSearchOffset(ctx, sources.HDBPropertyInfoResourceID, nil, limit, offset)
		if err == nil {
			return records, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == hdbMaxAttempts {
			break
		}
		backoff := hdbBaseBackoff << (attempt - 1)
		slog.Warn("hdb property info fetch retry", "attempt", attempt, "offset", offset, "backoff_ms", backoff.Milliseconds(), "err", err)
		if err := sleepCtx(ctx, backoff); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("hdb property info fetch after %d attempts: %w", hdbMaxAttempts, lastErr)
}

func parseHDBPropertyInfo(records []map[string]any) []store.HDBPropertyInfoRow {
	out := make([]store.HDBPropertyInfoRow, 0, len(records))
	for _, r := range records {
		street := strings.ToUpper(strings.TrimSpace(getStr(r, "street")))
		block := strings.TrimSpace(getStr(r, "blk_no"))
		if street == "" || block == "" {
			continue
		}
		// Only residential blocks belong in hdb_blocks. Markets / carparks /
		// pavilions ship in the same feed but are out of scope here.
		residential := strings.EqualFold(strings.TrimSpace(getStr(r, "residential")), "Y")
		if !residential {
			continue
		}
		out = append(out, store.HDBPropertyInfoRow{
			StreetName:    street,
			Block:         block,
			YearCompleted: parseIntPtr(r, "year_completed"),
			MaxFloorLvl:   parseIntPtr(r, "max_floor_lvl"),
			TotalDwelling: parseIntPtr(r, "total_dwelling_units"),
			Residential:   true,
			TownCode:      strings.ToUpper(strings.TrimSpace(getStr(r, "bldg_contract_town"))),
		})
	}
	return out
}

// parseIntPtr returns nil when the value is missing, blank, or not a
// plain integer — the PI feed occasionally uses sentinel strings like
// "uc" / "na" for unfinished blocks, which must not silently become 0.
func parseIntPtr(m map[string]any, k string) *int {
	s := strings.TrimSpace(getStr(m, k))
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

func (h *HDBIngestor) runIncremental(ctx context.Context, report store.ProgressReporter) (int, int, error) {
	// CKAN datastore_search only supports equality filters, so pull the last
	// 2 months one at a time.
	now := time.Now()
	months := []string{
		now.Format("2006-01"),
		now.AddDate(0, -1, 0).Format("2006-01"),
	}
	totalIn, totalUp := 0, 0
	for i, m := range months {
		report.Report("fetch "+m, float64(i)/float64(len(months))*100)
		in, up, err := h.fetchMonth(ctx, m)
		if err != nil {
			return totalIn, totalUp, err
		}
		totalIn += in
		totalUp += up
	}
	slog.Info("hdb incremental done", "rows_in", totalIn, "rows_upserted", totalUp)
	return totalIn, totalUp, nil
}

func (h *HDBIngestor) runBackfill(ctx context.Context, report store.ProgressReporter) (int, int, error) {
	// Backfill uses data.gov.sg's initiate-download endpoint to grab the
	// entire resale dataset as a single CSV. This is much faster than paging
	// through datastore_search and avoids the CKAN rate limit entirely.
	report.Report("downloading", 0)
	body, err := h.DG.DownloadDatasetCSV(ctx, sources.HDBResaleResourceID)
	if err != nil {
		return 0, 0, fmt.Errorf("hdb backfill download: %w", err)
	}
	defer body.Close()

	r := csv.NewReader(body)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return 0, 0, fmt.Errorf("hdb csv header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}

	const batchSize = hdbResaleBatchSize
	batch := make([]map[string]any, 0, batchSize)
	totalIn, totalUp := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		rows := parseHDBRecords(batch)
		in, up, err := h.Store.UpsertHDBResale(ctx, rows)
		if err != nil {
			return err
		}
		totalIn += in
		totalUp += up
		slog.Info("hdb backfill batch", "got", len(batch), "upserted", up, "total_upserted", totalUp)
		// No reliable total upfront (streaming CSV), so report the running
		// row count as stage text and leave the numeric progress unset — the
		// scheduler will set it to 100 on success.
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
			return totalIn, totalUp, fmt.Errorf("hdb csv read: %w", err)
		}
		m := make(map[string]any, len(idx))
		for name, i := range idx {
			if i < len(rec) {
				m[name] = rec[i]
			}
		}
		batch = append(batch, m)
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return totalIn, totalUp, err
			}
		}
	}
	if err := flush(); err != nil {
		return totalIn, totalUp, err
	}
	slog.Info("hdb backfill done", "rows_in", totalIn, "rows_upserted", totalUp)
	return totalIn, totalUp, nil
}

func (h *HDBIngestor) fetchMonth(ctx context.Context, month string) (int, int, error) {
	// Page through a single month. CKAN datastore_search accepts offset,
	// so we loop until we drain the month.
	const pageSize = hdbResalePageSize
	offset := 0
	totalIn, totalUp := 0, 0
	for {
		records, err := h.fetchPage(ctx, map[string]string{"month": month}, pageSize, offset)
		if err != nil {
			return totalIn, totalUp, err
		}
		if len(records) == 0 {
			break
		}
		rows := parseHDBRecords(records)
		in, up, err := h.Store.UpsertHDBResale(ctx, rows)
		if err != nil {
			return totalIn, totalUp, err
		}
		totalIn += in
		totalUp += up
		if len(records) < pageSize {
			break
		}
		offset += pageSize
		if err := sleepCtx(ctx, hdbPageDelay); err != nil {
			return totalIn, totalUp, err
		}
	}
	return totalIn, totalUp, nil
}

// fetchPage wraps DatastoreSearchOffset with retry + exponential backoff.
// data.gov.sg returns 429 / 5xx / timeout under load; we retry all of them
// the same way since the call is idempotent.
func (h *HDBIngestor) fetchPage(ctx context.Context, filters map[string]string, limit, offset int) ([]map[string]any, error) {
	var lastErr error
	for attempt := 1; attempt <= hdbMaxAttempts; attempt++ {
		records, err := h.DG.DatastoreSearchOffset(ctx, sources.HDBResaleResourceID, filters, limit, offset)
		if err == nil {
			return records, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == hdbMaxAttempts {
			break
		}
		backoff := hdbBaseBackoff << (attempt - 1)
		slog.Warn("hdb fetch retry", "attempt", attempt, "offset", offset, "backoff_ms", backoff.Milliseconds(), "err", err)
		if err := sleepCtx(ctx, backoff); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("hdb fetch after %d attempts: %w", hdbMaxAttempts, lastErr)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func parseHDBRecords(records []map[string]any) []store.HDBResaleRow {
	out := make([]store.HDBResaleRow, 0, len(records))
	for _, r := range records {
		row := store.HDBResaleRow{
			Town:           getStr(r, "town"),
			Block:          getStr(r, "block"),
			StreetName:     getStr(r, "street_name"),
			FlatType:       getStr(r, "flat_type"),
			StoreyRange:    getStr(r, "storey_range"),
			FlatModel:      getStr(r, "flat_model"),
			RemainingLease: getStr(r, "remaining_lease"),
		}
		if v := getStr(r, "month"); v != "" {
			if t, err := time.Parse("2006-01", v); err == nil {
				row.Month = t
			}
		}
		row.FloorAreaSqm = parseFloat(r, "floor_area_sqm")
		row.ResalePrice = parseFloat(r, "resale_price")
		if v := getStr(r, "lease_commence_date"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				row.LeaseCommence = n
			}
		}
		if row.Month.IsZero() || row.ResalePrice == 0 {
			continue
		}
		out = append(out, row)
	}
	return out
}

func getStr(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func parseFloat(m map[string]any, k string) float64 {
	s := strings.TrimSpace(getStr(m, k))
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// parseRange reads a URA band string like "900-1000" into (min, max). URA
// publishes rental areaSqm/areaSqft as ranges for privacy, so a plain
// ParseFloat silently zeroes them. Single-value strings are accepted and
// returned as min=max. Returns ok=false if the field is missing or malformed.
func parseRange(m map[string]any, k string) (min, max float64, ok bool) {
	s := strings.TrimSpace(getStr(m, k))
	if s == "" {
		return 0, 0, false
	}
	if i := strings.Index(s, "-"); i > 0 {
		lo, loErr := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
		hi, hiErr := strconv.ParseFloat(strings.TrimSpace(s[i+1:]), 64)
		if loErr == nil && hiErr == nil && hi >= lo {
			return lo, hi, true
		}
		return 0, 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, 0, false
	}
	return f, f, true
}
