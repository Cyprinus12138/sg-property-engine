//go:build smoke

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/internal/testdb"
	"github.com/Cyprinus12138/sg-property-engine/scheduler"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// End-to-end over a real PostGIS: ingest-shaped upserts through the store,
// then the HTTP handlers with geo filters. Pins the contract the Go client
// and downstream consumers rely on.
func TestHDBAndGeoRoundTrip(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()

	month := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	rows := []store.HDBResaleRow{
		{Month: month, Town: "BISHAN", Block: "123", StreetName: "BISHAN ST 12", FlatType: "4 ROOM", StoreyRange: "07 TO 09", FloorAreaSqm: 93, LeaseCommence: 1987, ResalePrice: 680000},
		{Month: month, Town: "BISHAN", Block: "123", StreetName: "BISHAN ST 12", FlatType: "5 ROOM", StoreyRange: "10 TO 12", FloorAreaSqm: 121, LeaseCommence: 1987, ResalePrice: 850000},
		{Month: month, Town: "PUNGGOL", Block: "301A", StreetName: "PUNGGOL PL", FlatType: "4 ROOM", StoreyRange: "04 TO 06", FloorAreaSqm: 93, LeaseCommence: 2012, ResalePrice: 560000},
	}
	in, up, err := s.UpsertHDBResale(ctx, rows)
	if err != nil || in != 3 || up != 3 {
		t.Fatalf("upsert: in=%d up=%d err=%v", in, up, err)
	}
	// Idempotent: same rows again insert nothing.
	if _, up, err := s.UpsertHDBResale(ctx, rows); err != nil || up != 0 {
		t.Fatalf("re-upsert must be a no-op: up=%d err=%v", up, err)
	}
	if n, err := s.RebuildHDBBlocksFromResale(ctx); err != nil || n != 2 {
		t.Fatalf("blocks rebuild: n=%d err=%v", n, err)
	}
	// Geocode one block; the other stays NULL and must be reported as pending.
	if err := s.SetHDBBlockGeo(ctx, "BISHAN ST 12", "123", "570123", 1.3512, 103.8485, "BISHAN", "BISHAN LOFT"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountBlocksNeedingGeocode(ctx); n != 1 {
		t.Fatalf("blocks needing geocode = %d, want 1", n)
	}

	// An MRT exit ~300 m from the block, and a far one.
	_, _, _, err = s.SyncPOIsBySource(ctx, "test:mrt", []store.POIRow{
		{Type: "mrt_exit", Name: "BISHAN A", Subtype: "A", Lat: 1.3510, Lng: 103.8460, Source: "test:mrt",
			Attrs: map[string]any{"station": "BISHAN", "exit": "A", "codes": []string{"NS17", "CC15"}, "lines": []string{"NSL", "CCL"}}},
		{Type: "mrt_exit", Name: "BISHAN B", Subtype: "B", Lat: 1.3505, Lng: 103.8470, Source: "test:mrt",
			Attrs: map[string]any{"station": "BISHAN", "exit": "B", "codes": []string{"NS17", "CC15"}, "lines": []string{"NSL", "CCL"}}},
		{Type: "mrt_exit", Name: "PUNGGOL A", Subtype: "A", Lat: 1.4053, Lng: 103.9023, Source: "test:mrt",
			Attrs: map[string]any{"station": "PUNGGOL", "exit": "A"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{Store: s, Sched: scheduler.New(s)}
	h := srv.Routes()

	get := func(path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s → %d %s", path, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s: bad json: %v", path, err)
		}
		return out
	}

	// Plain filter + paging.
	out := get("/hdb/transactions?town=BISHAN&limit=1")
	if out["total"].(float64) != 2 || len(out["rows"].([]any)) != 1 {
		t.Fatalf("/hdb/transactions town filter: %v", out)
	}
	row := out["rows"].([]any)[0].(map[string]any)
	if row["psf"].(float64) < 600 || row["lat"] == nil {
		t.Fatalf("psf/lat missing on geocoded row: %v", row)
	}

	// Near-anchor by raw point: only the Bishan block is within 1 km.
	out = get("/hdb/transactions?near_lat=1.3512&near_lng=103.8485&radius_m=1000&sort=distance_asc")
	if out["total"].(float64) != 2 {
		t.Fatalf("near-anchor total = %v, want 2 (Punggol excluded, un-geocoded excluded)", out["total"])
	}
	if d := out["rows"].([]any)[0].(map[string]any)["distance_m"]; d == nil || d.(float64) > 1 {
		t.Fatalf("distance_m should be ~0 for the anchor block, got %v", d)
	}

	// Near-anchor by HDB block name resolves through the store.
	out = get("/hdb/transactions?near_block=123&near_street=BISHAN&radius_m=500")
	if out["total"].(float64) != 2 {
		t.Fatalf("near_block anchor total = %v", out["total"])
	}

	// has_poi: the Bishan block has an MRT exit within 500 m.
	out = get("/hdb/transactions?has_poi_type=mrt_exit&has_poi_within_m=500")
	if out["total"].(float64) != 2 {
		t.Fatalf("has_poi total = %v, want 2", out["total"])
	}

	// Stats aggregation.
	out = get("/hdb/stats?group_by=town&metric=count")
	buckets := out["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("stats buckets = %v", buckets)
	}

	// Nearest stations groups exits per station; the closest exit (B, ~184 m)
	// represents the station, not the first row.
	out = get("/geo/nearest-stations?lat=1.3512&lng=103.8485&limit=1&within_m=100")
	st := out["stations"].([]any)
	if len(st) != 1 {
		t.Fatalf("nearest stations = %v", st)
	}
	s0 := st[0].(map[string]any)
	if s0["station"] != "BISHAN" || s0["nearest_exit"] != "B" || s0["walk_min"].(float64) != 2 {
		t.Fatalf("station row = %v", s0)
	}
	if d := s0["distance_m"].(float64); d < 170 || d > 200 {
		t.Fatalf("distance_m = %v, want ~184", d)
	}
	if codes := s0["codes"].([]any); len(codes) != 2 {
		t.Fatalf("codes = %v", codes)
	}

	// Autocomplete merges HDB blocks (projects table is empty here).
	out = get("/search/properties?q=bish")
	srows := out["rows"].([]any)
	if len(srows) != 1 || srows[0].(map[string]any)["kind"] != "hdb" || srows[0].(map[string]any)["district"] != "20" {
		t.Fatalf("search rows = %v", srows)
	}

	// POI types.
	out = get("/pois/types")
	if types := out["types"].([]any); len(types) != 1 || types[0].(map[string]any)["count"].(float64) != 3 {
		t.Fatalf("poi types = %v", out)
	}
}

func TestURAProjectsRoundTrip(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()

	d := func(y, m int) time.Time { return time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC) }
	txns := []store.URATransactionRow{
		{Project: "THE SAIL @ MARINA BAY", Street: "MARINA BOULEVARD", District: "01", PropertyType: "Apartment", Tenure: "99 yrs", MarketSegment: "CCR", AreaSqm: 100, Price: 2000000, PSF: 1858, ContractDate: d(2024, 3), FloorRange: "41-45", TypeOfSale: "3"},
		{Project: "THE SAIL @ MARINA BAY", Street: "MARINA BOULEVARD", District: "01", PropertyType: "Apartment", Tenure: "99 yrs", MarketSegment: "CCR", AreaSqm: 80, Price: 1700000, PSF: 1974, ContractDate: d(2024, 2), FloorRange: "21-25", TypeOfSale: "3"},
		{Project: "MEYER MANSION", Street: "MEYER ROAD", District: "15", PropertyType: "Condominium", Tenure: "Freehold", MarketSegment: "RCR", AreaSqm: 120, Price: 3000000, PSF: 2322, ContractDate: d(2024, 3), FloorRange: "06-10", TypeOfSale: "1"},
	}
	if _, up, err := s.UpsertURATransactions(ctx, txns); err != nil || up != 3 {
		t.Fatalf("upsert: up=%d err=%v", up, err)
	}
	if n, err := s.RebuildProjectsFromURA(ctx); err != nil || n != 2 {
		t.Fatalf("rebuild projects: n=%d err=%v", n, err)
	}
	// Pipeline merge: a case-twin must update in place, a placeholder is skipped,
	// a genuinely new launch is inserted UPPERCASE.
	ins, upd, skip, err := s.UpsertProjectsFromPipeline(ctx, []store.URAPipelineRow{
		{Project: "Meyer Mansion", Street: "MEYER ROAD", District: "15", MarketSegment: "RCR", Raw: map[string]any{"noOfCondo": float64(200)}},
		{Project: "Residential apartments", Street: "X"},
		{Project: "Newport Residences", Street: "ANSON ROAD", District: "02", MarketSegment: "CCR", Raw: map[string]any{"noOfApartment": float64(246)}},
	})
	if err != nil || ins != 1 || upd != 1 || skip != 1 {
		t.Fatalf("pipeline merge: ins=%d upd=%d skip=%d err=%v", ins, upd, skip, err)
	}
	if err := s.SetProjectGeo(ctx, "MEYER MANSION", "437881", 1.2976, 103.8932, "MARINE PARADE"); err != nil {
		t.Fatal(err)
	}
	// Geo survives a rebuild.
	if _, err := s.RebuildProjectsFromURA(ctx); err != nil {
		t.Fatal(err)
	}

	srv := &Server{Store: s, Sched: scheduler.New(s)}
	h := srv.Routes()
	get := func(path string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := get("/projects?district=15")
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("/projects district filter: %d %v", code, out)
	}
	p := out["rows"].([]any)[0].(map[string]any)
	if p["name"] != "MEYER MANSION" || p["display_name"] != "Meyer Mansion" || p["lat"] == nil || p["txn_count"].(float64) != 1 {
		t.Fatalf("project row = %v", p)
	}

	// Case- and punctuation-insensitive lookup, nearby_mrt always present.
	code, out = get("/projects/the%20sail%20marina%20bay")
	if code != 200 || out["name"] != "THE SAIL @ MARINA BAY" {
		t.Fatalf("/projects/{name}: %d %v", code, out)
	}
	if _, ok := out["nearby_mrt"].([]any); !ok {
		t.Fatalf("nearby_mrt must be an array: %v", out["nearby_mrt"])
	}
	if code, _ = get("/projects/NOPE"); code != 404 {
		t.Fatalf("unknown project → %d, want 404", code)
	}

	// Pipeline-only project is addressable and UPPERCASE.
	if code, out = get("/projects/newport%20residences"); code != 200 || out["name"] != "NEWPORT RESIDENCES" {
		t.Fatalf("pipeline-only project: %d %v", code, out)
	}

	// URA transactions with psf band + district.
	code, out = get("/ura/transactions?district=01&min_psf=1900")
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("/ura/transactions: %d %v", code, out)
	}

	// Hottest uses the most recent contract_date as anchor, not NOW().
	code, out = get("/explore/hottest?limit=5")
	if code != 200 || len(out["rows"].([]any)) != 2 {
		t.Fatalf("/explore/hottest: %d %v", code, out)
	}
	top := out["rows"].([]any)[0].(map[string]any)
	if top["name"] != "THE SAIL @ MARINA BAY" || top["txn_count"].(float64) != 2 {
		t.Fatalf("hottest top = %v", top)
	}

	code, out = get("/explore/district_median?district=15&window_days=90")
	if code != 200 || out["sample_count"].(float64) != 1 || out["median_psf"].(float64) != 2322 {
		t.Fatalf("/explore/district_median: %d %v", code, out)
	}
}

func TestSyncEndpointsAndLock(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	sched := scheduler.New(s)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	if err := sched.Register(scheduler.Job{Source: "slow", Run: func(ctx context.Context, _ string, report store.ProgressReporter) (int, int, error) {
		started <- struct{}{}
		report.Report("working", 50)
		<-release
		return 10, 7, nil
	}}); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Store: s, Sched: sched}).Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/sync/run?source=slow&mode=backfill", nil))
	if rec.Code != 202 {
		t.Fatalf("trigger → %d %s", rec.Code, rec.Body.String())
	}
	var trig struct {
		RunID int64 `json:"run_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &trig)
	<-started

	// Second trigger while running → 409.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/sync/run?source=slow", nil))
	if rec.Code != 409 {
		t.Fatalf("concurrent trigger → %d, want 409", rec.Code)
	}
	// Unknown source → 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/sync/run?source=nope", nil))
	if rec.Code != 400 {
		t.Fatalf("unknown source → %d, want 400", rec.Code)
	}

	close(release)
	sched.WaitDone(context.Background(), "slow")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sync/runs/"+jsonInt(trig.RunID), nil))
	var run store.SyncRun
	_ = json.Unmarshal(rec.Body.Bytes(), &run)
	if rec.Code != 200 || run.Status != store.StatusSuccess || run.RowsIn != 10 || run.RowsUpserted != 7 || run.Progress != 100 {
		t.Fatalf("run row = %d %+v", rec.Code, run)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sync/status", nil))
	var status struct {
		Sources []store.SyncRun `json:"sources"`
		Known   []string        `json:"known"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if len(status.Sources) != 1 || len(status.Known) != 1 || status.Known[0] != "slow" {
		t.Fatalf("status = %+v", status)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
