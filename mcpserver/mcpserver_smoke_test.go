//go:build smoke

package mcpserver

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/sg-property-engine/httpapi"
	"github.com/Cyprinus12138/sg-property-engine/internal/testdb"
	"github.com/Cyprinus12138/sg-property-engine/scheduler"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// The tools end to end: MCP over streamable HTTP at /mcp on the real
// router, in-process calls into the real handlers, real PostGIS. Real
// responses (timestamps, numerics, distances) must pass the output schemas.
func TestToolsOverRealEngine(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()
	month := func(y, m int) time.Time { return time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC) }

	if _, _, err := s.UpsertHDBResale(ctx, []store.HDBResaleRow{
		{Month: month(2024, 3), Town: "BISHAN", Block: "123", StreetName: "BISHAN ST 12", FlatType: "4 ROOM", StoreyRange: "07 TO 09", FloorAreaSqm: 93, LeaseCommence: 1987, ResalePrice: 680000},
		{Month: month(2024, 2), Town: "BISHAN", Block: "123", StreetName: "BISHAN ST 12", FlatType: "5 ROOM", StoreyRange: "10 TO 12", FloorAreaSqm: 121, LeaseCommence: 1987, ResalePrice: 850000},
		{Month: month(2024, 3), Town: "PUNGGOL", Block: "301A", StreetName: "PUNGGOL PL", FlatType: "4 ROOM", StoreyRange: "04 TO 06", FloorAreaSqm: 93, LeaseCommence: 2012, ResalePrice: 560000},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RebuildHDBBlocksFromResale(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHDBBlockGeo(ctx, "BISHAN ST 12", "123", "570123", 1.3512, 103.8485, "BISHAN", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SyncPOIsBySource(ctx, "test:mrt", []store.POIRow{
		{Type: "mrt_exit", Name: "BISHAN A", Subtype: "A", Lat: 1.3510, Lng: 103.8460, Source: "test:mrt",
			Attrs: map[string]any{"station": "BISHAN", "exit": "A", "codes": []string{"NS17", "CC15"}, "lines": []string{"NSL", "CCL"}}},
		{Type: "mrt_exit", Name: "DAKOTA A", Subtype: "A", Lat: 1.3083, Lng: 103.8885, Source: "test:mrt",
			Attrs: map[string]any{"station": "DAKOTA", "exit": "A", "codes": []string{"CC8"}, "lines": []string{"CCL"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertURATransactions(ctx, []store.URATransactionRow{
		{Project: "MEYER MANSION", Street: "MEYER ROAD", District: "15", PropertyType: "Condominium", Tenure: "Freehold", MarketSegment: "RCR", AreaSqm: 120, Price: 3000000, PSF: 2322, ContractDate: month(2024, 3), FloorRange: "06-10", TypeOfSale: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RebuildProjectsFromURA(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectGeo(ctx, "MEYER MANSION", "437881", 1.2976, 103.8932, "MARINE PARADE"); err != nil {
		t.Fatal(err)
	}
	id, _ := s.StartRun(ctx, "hdb", store.ModeBackfill)
	_ = s.FinishRun(ctx, id, 3, 3, nil)

	srv := &httpapi.Server{Store: s, Sched: scheduler.New(s)}
	srv.MCP = HTTPHandler(New(InProcessClient(srv.Routes()), "smoke"))
	hs := httptest.NewServer(srv.Routes())
	defer hs.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ok := func(name string, args map[string]any) map[string]any {
		t.Helper()
		out, terr := call(t, cs, name, args)
		if terr != "" {
			t.Fatalf("%s: %s", name, terr)
		}
		return out
	}
	rows := func(out map[string]any) []map[string]any {
		var r []map[string]any
		for _, x := range out["rows"].([]any) {
			r = append(r, x.(map[string]any))
		}
		return r
	}

	if r := rows(ok("search_properties", map[string]any{"query": "meyer"})); len(r) == 0 || r[0]["name"] != "MEYER MANSION" {
		t.Errorf("search meyer = %v", r)
	}
	if p := ok("get_project", map[string]any{"name": "meyer mansion"}); p["district"] != "15" || p["postal"] != "437881" {
		t.Errorf("get_project = %v", p)
	}

	// Informal inputs all the way down: town and flat type in the wrong
	// case, an MRT station as anchor.
	out := ok("hdb_resale_transactions", map[string]any{
		"town": "bishan", "flat_type": "4-room",
		"near": map[string]any{"poi_type": "mrt_exit", "poi_name": "bishan", "radius_m": 800}, "sort": "distance_asc",
	})
	if r := rows(out); len(r) != 1 || r[0]["block"] != "123" || r[0]["distance_m"] == nil {
		t.Errorf("hdb near Bishan MRT = %v", r)
	}
	if a := out["anchor"].(map[string]any); a["name"] != "BISHAN A" {
		t.Errorf("anchor = %v", a)
	}

	out = ok("private_transactions", map[string]any{"near": map[string]any{"project": "Meyer Mansion", "radius_m": 200}})
	if r := rows(out); len(r) != 1 || r[0]["psf"].(float64) != 2322 {
		t.Errorf("ura near project = %v", r)
	}

	out = ok("nearest_mrt", map[string]any{"near": map[string]any{"block": "123", "street": "bishan st 12"}, "limit": 1})
	if st := out["stations"].([]any); len(st) == 0 || st[0].(map[string]any)["station"] != "BISHAN" {
		t.Errorf("nearest_mrt = %v", out)
	}

	out = ok("hdb_resale_stats", map[string]any{"group_by": "town", "metric": "count"})
	if b := out["buckets"].([]any); len(b) != 2 {
		t.Errorf("stats buckets = %v", b)
	}

	if r := rows(ok("find_pois", map[string]any{"type": "mrt_exit", "near": map[string]any{"project": "meyer mansion", "radius_m": 3000}})); len(r) != 1 || r[0]["name"] != "DAKOTA A" {
		t.Errorf("pois near Meyer = %v", r)
	}

	fresh := rows(ok("data_freshness", map[string]any{}))
	if len(fresh) != 1 || fresh[0]["source"] != "hdb" || fresh[0]["status"] != "success" || fresh[0]["last_run_at"] == nil {
		t.Errorf("freshness = %v", fresh)
	}

	if m := ok("district_median", map[string]any{"district": "D15", "window_days": 365}); m["sample_count"].(float64) != 1 {
		t.Errorf("district median = %v", m)
	}

	// A typo'd anchor must fail loudly, not return Punggol.
	if _, terr := call(t, cs, "hdb_resale_transactions", map[string]any{"near": map[string]any{"project": "meyer mansionz"}}); !strings.Contains(terr, "search_properties") {
		t.Errorf("typo anchor error = %q", terr)
	}
	if _, terr := call(t, cs, "cea_salesperson", map[string]any{"registration_no": "r000000z"}); !strings.Contains(terr, "R000000Z is not in the CEA register") {
		t.Errorf("cea miss = %q", terr)
	}
}
