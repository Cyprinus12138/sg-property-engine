//go:build smoke

package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cyprinus12138/sg-property-engine/internal/testdb"
	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/sources/sourcestest"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// fakeUpstreams serves canned data.gov.sg, URA and OneMap responses, so the
// ingestors below run their real fetch → parse → upsert path against a real
// PostGIS without touching the network.
type fakeUpstreams struct {
	// csv is the body served for a data.gov.sg dataset download, by id.
	csv map[string]string
	// datastore is the records array served by datastore_search, by
	// resource id; offset > 0 returns an empty page.
	datastore map[string][]map[string]any
	// uraBatch1 is PMI_Resi_Transaction batch 1; batches 2–4 are empty.
	uraBatch1 []map[string]any
	// onemap maps a searchVal to its results.
	onemap map[string][]sources.OneMapResult
}

func (f *fakeUpstreams) client(t *testing.T) *http.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/public/api/datasets/{id}/initiate-download", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"url":"https://s3.example.test/%s.csv"}}`, r.PathValue("id"))
	})
	mux.HandleFunc("/{file}", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(r.PathValue("file"), ".csv")
		body, ok := f.csv[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/api/action/datastore_search", func(w http.ResponseWriter, r *http.Request) {
		recs := f.datastore[r.URL.Query().Get("resource_id")]
		if r.URL.Query().Get("offset") != "" {
			recs = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"records": recs}})
	})
	mux.HandleFunc("/uraDataService/insertNewToken/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Status":"Success","Result":"tok"}`)
	})
	mux.HandleFunc("/uraDataService/invokeUraDS/v1", func(w http.ResponseWriter, r *http.Request) {
		result := []map[string]any{}
		if r.URL.Query().Get("batch") == "1" {
			result = f.uraBatch1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Status": "Success", "Result": result})
	})
	mux.HandleFunc("/api/common/elastic/search", func(w http.ResponseWriter, r *http.Request) {
		res := f.onemap[r.URL.Query().Get("searchVal")]
		if res == nil {
			res = []sources.OneMapResult{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"found": len(res), "results": res})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sourcestest.Client(srv)
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

const hdbResaleCSV = `month,town,flat_type,block,street_name,storey_range,floor_area_sqm,flat_model,lease_commence_date,remaining_lease,resale_price
2024-03,BISHAN,4 ROOM,123,BISHAN ST 12,07 TO 09,93,Model A,1987,62 years 04 months,680000
2024-03,BISHAN,5 ROOM,123,BISHAN ST 12,10 TO 12,121,Improved,1987,62 years 04 months,850000
2024-04,PUNGGOL,4 ROOM,301A,PUNGGOL PL,04 TO 06,93,Premium Apartment,2012,87 years,560000
2024-04,PUNGGOL,4 ROOM,301A,PUNGGOL PL,04 TO 06,93,Premium Apartment,2012,87 years,
`

func TestHDBBackfill_ThenGeocode(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()

	up := &fakeUpstreams{
		csv: map[string]string{sources.HDBResaleResourceID: hdbResaleCSV},
		datastore: map[string][]map[string]any{sources.HDBPropertyInfoResourceID: {
			{"blk_no": "123", "street": "BISHAN ST 12", "residential": "Y", "year_completed": "1987", "max_floor_lvl": "12", "total_dwelling_units": "96", "bldg_contract_town": "BH"},
			{"blk_no": "9", "street": "BISHAN MARKET", "residential": "N"},
		}},
		onemap: map[string][]sources.OneMapResult{
			"123 BISHAN ST 12": {
				// A decoy first: same block number, different road.
				{BlkNo: "123", Road: "BISHAN ST 13", Postal: "570999", Latitude: "1.3600", Longitude: "103.8500", Building: "WRONG"},
				{BlkNo: "123", Road: "BISHAN ST 12", Postal: "570123", Latitude: "1.3512", Longitude: "103.8485", Building: "BISHAN LOFT"},
			},
			// 301A PUNGGOL PL: no results, stays pending.
		},
	}
	httpc := up.client(t)
	dg := sources.NewDataGov()
	dg.SetHTTPClient(httpc)
	om := sources.NewOneMap(sources.OneMapCreds{})
	om.SetHTTPClient(httpc)

	var stages []string
	report := store.ProgressReporter(func(stage string, _ float64) { stages = append(stages, stage) })

	hdb := &HDBIngestor{DG: dg, Store: s}
	in, n, err := hdb.Run(ctx, store.ModeBackfill, report)
	if err != nil {
		t.Fatal(err)
	}
	if in != 3 || n != 3 {
		t.Fatalf("backfill in=%d upserted=%d, want 3/3 (the price-less row is dropped)", in, n)
	}
	if len(stages) == 0 || stages[0] != "downloading" {
		t.Errorf("stages = %q", stages)
	}
	// Blocks: 123 from property info (with its facts), 301A from the
	// resale fallback; the non-residential row is skipped.
	if got := count(t, pool, `SELECT count(*) FROM hdb_blocks`); got != 2 {
		t.Errorf("hdb_blocks = %d, want 2", got)
	}
	if got := count(t, pool, `SELECT year_completed FROM hdb_blocks WHERE block='123' AND street_name='BISHAN ST 12'`); got != 1987 {
		t.Errorf("year_completed = %d", got)
	}

	// A second run over the same snapshot is a no-op.
	if _, n, err := hdb.Run(ctx, store.ModeBackfill, nil); err != nil || n != 0 {
		t.Fatalf("re-run upserted=%d err=%v, want 0", n, err)
	}

	geo := &GeoIngestor{Store: s, Resolver: &GeoResolver{OneMap: om, GoogleMaps: sources.NewGoogleMaps(sources.GoogleMapsCreds{}), Store: s}}
	in, n, err = geo.Run(ctx, store.ModeIncremental, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in != 2 || n != 1 {
		t.Errorf("geo in=%d upserted=%d, want 2/1", in, n)
	}
	var postal, name string
	if err := pool.QueryRow(ctx, `SELECT postal, name FROM hdb_blocks WHERE block='123'`).Scan(&postal, &name); err != nil {
		t.Fatal(err)
	}
	if postal != "570123" || name != "BISHAN LOFT" {
		t.Errorf("block 123 geocoded to postal=%q name=%q; the matching road must beat the decoy", postal, name)
	}
	if left, _ := s.CountBlocksNeedingGeocode(ctx); left != 1 {
		t.Errorf("blocks still needing geocode = %d, want 1 (301A had no OneMap result)", left)
	}
}

func TestURATransactions_RebuildProjectsAndDiscoverBuildings(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()

	txn := func(date, price, area string) map[string]any {
		return map[string]any{"contractDate": date, "price": price, "area": area, "district": "01",
			"propertyType": "Apartment", "tenure": "99 yrs lease commencing from 2005", "floorRange": "26-30", "typeOfSale": "3"}
	}
	up := &fakeUpstreams{
		uraBatch1: []map[string]any{
			{"project": "THE SAIL @ MARINA BAY", "street": "MARINA BOULEVARD", "marketSegment": "CCR",
				"transaction": []any{txn("0324", "1850000", "92"), txn("0424", "2100000", "105"), txn("0424", "", "105")}},
			{"project": "NO TXNS", "street": "NOWHERE"},
		},
		onemap: map[string][]sources.OneMapResult{
			"THE SAIL @ MARINA BAY MARINA BOULEVARD": {
				{BlkNo: "2", Road: "MARINA BOULEVARD", Postal: "018987", Latitude: "1.2803", Longitude: "103.8527", Building: "THE SAIL @ MARINA BAY"},
				{BlkNo: "4", Road: "MARINA BOULEVARD", Postal: "018986", Latitude: "1.2806", Longitude: "103.8531", Building: "THE SAIL @ MARINA BAY"},
				{BlkNo: "2", Road: "MARINA BOULEVARD", Postal: "018987", Latitude: "1.2803", Longitude: "103.8527", Building: "DUPLICATE ROW"},
				{BlkNo: "8", Road: "MARINA VIEW", Postal: "018960", Latitude: "1.2790", Longitude: "103.8520", Building: "OTHER STREET"},
			},
		},
	}
	httpc := up.client(t)
	ura := sources.NewURA("ak")
	ura.SetHTTPClient(httpc)
	om := sources.NewOneMap(sources.OneMapCreds{})
	om.SetHTTPClient(httpc)

	in, n, err := (&URATxnIngestor{URA: ura, Store: s}).Run(ctx, store.ModeBackfill, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in != 2 || n != 2 {
		t.Fatalf("ura_txn in=%d upserted=%d, want 2/2", in, n)
	}
	if got := count(t, pool, `SELECT count(*) FROM projects`); got != 1 {
		t.Errorf("projects = %d, want 1", got)
	}
	// PSF is derived from sqm → sqft.
	if got := count(t, pool, `SELECT round(psf)::int FROM ura_transactions WHERE price = 1850000`); got != 1868 {
		t.Errorf("psf = %d, want 1868", got)
	}

	geo := &GeoIngestor{Store: s, Resolver: &GeoResolver{OneMap: om, GoogleMaps: sources.NewGoogleMaps(sources.GoogleMapsCreds{}), Store: s}}
	if _, n, err := geo.Run(ctx, store.ModeBackfill, nil); err != nil || n != 1 {
		t.Fatalf("geo upserted=%d err=%v, want 1", n, err)
	}
	if left, _ := s.CountProjectsNeedingGeocode(ctx); left != 0 {
		t.Errorf("projects needing geocode = %d", left)
	}
	// One building per distinct postal on the project's own street.
	if got := count(t, pool, `SELECT count(*) FROM project_buildings`); got != 2 {
		t.Errorf("project_buildings = %d, want 2", got)
	}
}

// The register is a full snapshot keyed on registration_no: a re-run
// refreshes every row in place (agencies and end dates change), so the row
// count is stable while rows_upserted counts the refreshed rows.
func TestCEASalespersons_SnapshotRefreshesInPlace(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()

	const header = "salesperson_name,registration_no,registration_start_date,registration_end_date,estate_agent_name,estate_agent_license_no\n"
	up := &fakeUpstreams{csv: map[string]string{sources.CEASalespersonResourceID: header +
		"TAN AH KOW,R012345A,01/02/2015,31/12/2026,EXAMPLE REALTY PTE LTD,L3000000A\n" +
		"LIM MEI LING,r054321b,2020-06-15,2026-12-31,EXAMPLE REALTY PTE LTD,L3000000A\n" +
		"NO NUMBER,,01/01/2020,31/12/2026,EXAMPLE REALTY PTE LTD,L3000000A\n"}}
	dg := sources.NewDataGov()
	dg.SetHTTPClient(up.client(t))
	cea := &CEAIngestor{DG: dg, Store: s}

	in, n, err := cea.Run(ctx, store.ModeIncremental, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in != 2 || n != 2 {
		t.Fatalf("cea in=%d upserted=%d, want 2/2 (the row without a number is dropped)", in, n)
	}
	// Registration numbers are normalised to upper case.
	if got := count(t, pool, `SELECT count(*) FROM cea_salespersons WHERE registration_no = 'R054321B'`); got != 1 {
		t.Errorf("R054321B missing")
	}

	// Next month's snapshot: one salesperson moved agency.
	up.csv[sources.CEASalespersonResourceID] = header +
		"TAN AH KOW,R012345A,01/02/2015,31/12/2027,OTHER HOMES PTE LTD,L3111111B\n" +
		"LIM MEI LING,R054321B,2020-06-15,2026-12-31,EXAMPLE REALTY PTE LTD,L3000000A\n"
	if _, _, err := cea.Run(ctx, store.ModeIncremental, nil); err != nil {
		t.Fatal(err)
	}
	if got := count(t, pool, `SELECT count(*) FROM cea_salespersons`); got != 2 {
		t.Errorf("rows = %d after refresh, want 2", got)
	}
	var agent string
	if err := pool.QueryRow(ctx, `SELECT estate_agent_license_no FROM cea_salespersons WHERE registration_no='R012345A'`).Scan(&agent); err != nil || agent != "L3111111B" {
		t.Errorf("agency not refreshed: %q %v", agent, err)
	}
}

func TestHDBBackfill_DownloadFailureSurfaces(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	up := &fakeUpstreams{} // no CSV registered → the presigned URL 404s
	dg := sources.NewDataGov()
	dg.SetHTTPClient(up.client(t))

	_, _, err := (&HDBIngestor{DG: dg, Store: s}).Run(context.Background(), store.ModeBackfill, nil)
	if err == nil || !strings.Contains(err.Error(), "hdb backfill download") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
	if got := count(t, pool, `SELECT count(*) FROM hdb_resale`); got != 0 {
		t.Errorf("hdb_resale = %d after a failed download", got)
	}
}
