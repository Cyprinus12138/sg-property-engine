package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/sg-property-engine/client"
)

// fakeEngine answers the query API with canned JSON and records every
// request it saw, so tests can assert what reached the wire.
type fakeEngine struct {
	mu     sync.Mutex
	routes map[string]string // path → JSON body; missing path → 404
	seen   []*url.URL
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.URL)
	body, ok := f.routes[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (f *fakeEngine) query(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.seen) - 1; i >= 0; i-- {
		if f.seen[i].Path == path {
			return f.seen[i].Query()
		}
	}
	return nil
}

func (f *fakeEngine) called(path string) bool { return f.query(path) != nil }

// connect starts the MCP server over an in-memory transport, backed by eng.
func connect(t *testing.T, eng http.Handler) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := New(InProcessClient(eng), "test")
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call invokes a tool and returns its structured output, failing on a
// protocol error. toolErr is the tool-level error text, if any.
func call(t *testing.T, cs *mcp.ClientSession, name string, args any) (out map[string]any, toolErr string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return nil, b.String()
	}
	raw, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(raw, &out)
	return out, ""
}

func TestListTools_AllReadOnlyWithSchemas(t *testing.T) {
	cs := connect(t, &fakeEngine{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 18 {
		t.Errorf("got %d tools, want 18", len(res.Tools))
	}
	for _, tool := range res.Tools {
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s: missing description or schemas", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: must be annotated read-only", tool.Name)
		}
	}
}

func TestHDBTransactions_NormalisesFilters(t *testing.T) {
	eng := &fakeEngine{routes: map[string]string{
		"/hdb/transactions": `{"rows":[],"total":0,"limit":100,"offset":0}`,
	}}
	cs := connect(t, eng)
	_, terr := call(t, cs, "hdb_resale_transactions", map[string]any{
		"town": "  ang mo  kio ", "flat_type": "4-room", "from_month": "2024-03",
		"street": "bishan st 12", "sort": "price_desc", "limit": 1000,
	})
	if terr != "" {
		t.Fatal(terr)
	}
	q := eng.query("/hdb/transactions")
	want := map[string]string{
		"town": "ANG MO KIO", "flat_type": "4 ROOM", "min_month": "2024-03-01",
		"street": "BISHAN ST 12", "sort": "price_desc", "limit": "100",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q (query %v)", k, q.Get(k), v, q)
		}
	}
}

func TestNear_ProjectResolvesToCoordinates(t *testing.T) {
	eng := &fakeEngine{routes: map[string]string{
		"/projects/the sail": `{"name":"THE SAIL @ MARINA BAY","display_name":"The Sail @ Marina Bay","lat":1.2806,"lng":103.8527,"txn_count":3,"nearby_mrt":null}`,
		"/ura/transactions":  `{"rows":[{"project":"THE SAIL @ MARINA BAY","price":1500000,"psf":2100,"distance_m":12.5}],"total":1,"limit":20,"offset":0}`,
	}}
	cs := connect(t, eng)
	out, terr := call(t, cs, "private_transactions", map[string]any{
		"near": map[string]any{"project": "the sail", "radius_m": 300}, "sort": "distance_asc",
	})
	if terr != "" {
		t.Fatal(terr)
	}
	q := eng.query("/ura/transactions")
	if q.Get("near_lat") != "1.280600" || q.Get("near_lng") != "103.852700" || q.Get("radius_m") != "300" {
		t.Errorf("engine query = %v, want resolved coordinates", q)
	}
	if q.Get("near_project") != "" {
		t.Error("names must not reach the engine's own anchor resolution")
	}
	a, _ := out["anchor"].(map[string]any)
	if a["kind"] != "project" || a["name"] != "THE SAIL @ MARINA BAY" {
		t.Errorf("anchor = %v", out["anchor"])
	}
}

func TestNear_UnknownAnchorIsAnErrorNotIslandWide(t *testing.T) {
	eng := &fakeEngine{routes: map[string]string{
		"/hdb/transactions": `{"rows":[],"total":0}`,
		"/pois":             `{"rows":[],"total":0}`,
	}}
	cs := connect(t, eng)
	cases := []struct {
		near map[string]any
		want string
	}{
		{map[string]any{"project": "NO SUCH PLACE"}, "search_properties"},
		{map[string]any{"poi_type": "mrt_exit", "poi_name": "atlantis"}, "no mrt_exit POI"},
		{map[string]any{"block": "999", "street": "nowhere"}, "no HDB block"},
		{map[string]any{"lat": 103.85, "lng": 1.29}, "swapped"},
		{map[string]any{"lat": 1.3}, "lat and lng go together"},
		{map[string]any{"project": "X", "lat": 1.3, "lng": 103.8}, "exactly one"},
		{map[string]any{"poi_name": "BISHAN"}, "needs poi_type"},
	}
	for _, c := range cases {
		eng.seen = nil
		_, terr := call(t, cs, "find_projects", map[string]any{"near": c.near})
		if !strings.Contains(terr, c.want) {
			t.Errorf("near %v: error %q, want it to mention %q", c.near, terr, c.want)
		}
		if eng.called("/projects") {
			t.Errorf("near %v: list endpoint was queried despite an unresolved anchor", c.near)
		}
	}
}

func TestNearestMRT_FromHDBBlock(t *testing.T) {
	eng := &fakeEngine{routes: map[string]string{
		"/hdb/transactions":     `{"rows":[{"block":"123","street_name":"BISHAN ST 12","lat":1.3512,"lng":103.8485}],"total":1}`,
		"/geo/nearest-stations": `{"lat":1.3512,"lng":103.8485,"limit":3,"within_m":1000,"stations":[{"station":"BISHAN","type":"mrt_exit","codes":["NS17","CC15"],"lines":["NSL","CCL"],"distance_m":290,"walk_min":4}]}`,
	}}
	cs := connect(t, eng)
	out, terr := call(t, cs, "nearest_mrt", map[string]any{"near": map[string]any{"block": "123", "street": "Bishan St 12"}})
	if terr != "" {
		t.Fatal(terr)
	}
	if q := eng.query("/hdb/transactions"); q.Get("block") != "123" || q.Get("street") != "BISHAN ST 12" {
		t.Errorf("block lookup query = %v", q)
	}
	st, _ := out["stations"].([]any)
	if len(st) != 1 {
		t.Fatalf("stations = %v", out["stations"])
	}
}

// Engines return null for empty arrays in places; output schemas must still
// validate, and callers should get [] rather than null.
func TestNullResponsesValidate(t *testing.T) {
	eng := &fakeEngine{routes: map[string]string{
		"/hdb/stats":               `{"group_by":"town","metric":"median_psf","buckets":null}`,
		"/ura/rentals/trend":       `{"months":null,"total":0}`,
		"/ura/pipeline":            `{"rows":null,"total":0}`,
		"/search/properties":       `{"rows":null}`,
		"/pois/types":              `{"types":null}`,
		"/explore/hottest":         `{"rows":null}`,
		"/cea/practice-stats":      `{"registration_no":"R1","mode":"sale","window_from":"2025-10-01T00:00:00Z","window_to":"2026-09-30T00:00:00Z","deals":0,"prior_deals":0,"active_months":0,"monthly":null,"towns":null,"representation":null,"tx_types":null}`,
		"/projects/x":              `{"name":"X","display_name":"X","txn_count":0,"nearby_mrt":null}`,
		"/sync/status":             `{"sources":null,"known":["hdb","cea"]}`,
		"/explore/district_median": `{"district":"09","window_days":90,"sample_count":0}`,
	}}
	cs := connect(t, eng)
	for name, args := range map[string]map[string]any{
		"hdb_resale_stats":     {"group_by": "town"},
		"private_rental_trend": {},
		"development_pipeline": {},
		"search_properties":    {"query": "zz"},
		"poi_types":            {},
		"hottest_projects":     {},
		"cea_practice_stats":   {"registration_no": "r1"},
		"get_project":          {"name": "x"},
		"data_freshness":       {},
		"district_median":      {"district": "D9"},
	} {
		out, terr := call(t, cs, name, args)
		if terr != "" {
			t.Errorf("%s: %s", name, terr)
			continue
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "null") {
			t.Errorf("%s: output has null: %s", name, raw)
		}
	}
	if q := eng.query("/explore/district_median"); q.Get("district") != "09" {
		t.Errorf("district = %q, want 09", q.Get("district"))
	}
	out, _ := call(t, cs, "data_freshness", map[string]any{})
	if rows, _ := out["rows"].([]any); len(rows) != 2 || rows[0].(map[string]any)["status"] != "never_run" {
		t.Errorf("freshness = %v", out)
	}
}

func TestInputErrors(t *testing.T) {
	cs := connect(t, &fakeEngine{})
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"find_projects", map[string]any{"district": "D31"}, "1-28"},
		{"private_transactions", map[string]any{"market_segment": "core"}, "CCR, RCR or OCR"},
		{"hdb_resale_transactions", map[string]any{"from_month": "March 2024"}, "YYYY-MM"},
		{"hdb_resale_transactions", map[string]any{"sort": "distance_asc"}, "needs near"},
		{"hdb_resale_stats", map[string]any{"group_by": "street"}, "want one of"},
		{"cea_practice_stats", map[string]any{"registration_no": "R1", "mode": "lease"}, "want one of"},
		{"search_properties", map[string]any{"query": "a"}, "at least 2"},
	} {
		if _, terr := call(t, cs, c.tool, c.args); !strings.Contains(terr, c.want) {
			t.Errorf("%s %v: error %q, want %q", c.tool, c.args, terr, c.want)
		}
	}
}

func TestStreamableHTTP(t *testing.T) {
	eng := &fakeEngine{routes: map[string]string{"/pois/types": `{"types":[{"type":"mrt_exit","count":3}]}`}}
	srv := httptest.NewServer(HTTPHandler(New(InProcessClient(eng), "test")))
	defer srv.Close()
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	out, terr := call(t, cs, "poi_types", map[string]any{})
	if terr != "" {
		t.Fatal(terr)
	}
	if rows, _ := out["rows"].([]any); len(rows) != 1 {
		t.Fatalf("poi_types over HTTP = %v", out)
	}

	// A browser on another origin must not be able to drive the tools.
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST → %d, want 403", resp.StatusCode)
	}
}

func TestNormalisers(t *testing.T) {
	for in, want := range map[string]string{"15": "15", "D9": "09", "d 05": "05", "District 28": "28", "1": "01"} {
		if got, err := normDistrict(in); err != nil || got != want {
			t.Errorf("normDistrict(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "29", "D", "Bishan"} {
		if _, err := normDistrict(bad); err == nil {
			t.Errorf("normDistrict(%q) should fail", bad)
		}
	}
	for in, want := range map[string]string{
		"4-room": "4 ROOM", "4rm": "4 ROOM", "3 Room": "3 ROOM", "5_ROOM": "5 ROOM",
		"executive": "EXECUTIVE", "Multi-Generation": "MULTI-GENERATION", "": "",
	} {
		if got := normFlatType(in); got != want {
			t.Errorf("normFlatType(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"condominium": "Condominium", "EC": "Executive Condominium", "condo": "Condominium",
		"semi-detached": "Semi-detached", "non-landed properties": "Non-landed Properties", "Shophouse": "Shophouse",
	} {
		if got := normPropertyType(in); got != want {
			t.Errorf("normPropertyType(%q) = %q, want %q", in, got, want)
		}
	}
	if got, _ := normDate("x", "2024-03"); got != "2024-03-01" {
		t.Errorf("normDate month = %q", got)
	}
	if _, err := normDate("x", "2024-13"); err == nil {
		t.Error("month 13 should fail")
	}
}

// The handler transport must surface engine errors like a real network
// client would.
func TestInProcessClient_PropagatesStatus(t *testing.T) {
	c := InProcessClient(&fakeEngine{})
	if _, err := c.Project(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not found", err)
	}
	var _ = client.ErrNotFound
}
