//go:build smoke

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/internal/testdb"
	"github.com/Cyprinus12138/sg-property-engine/scheduler"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// The endpoints behind the ops UI: run history, table stats, table preview.
func TestOpsEndpoints(t *testing.T) {
	pool := testdb.Open(t)
	s := &store.Store{Pool: pool}
	ctx := context.Background()

	if _, _, err := s.UpsertHDBResale(ctx, []store.HDBResaleRow{
		{Month: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), Town: "BISHAN", Block: "123", StreetName: "BISHAN ST 12",
			FlatType: "4 ROOM", StoreyRange: "07 TO 09", FloorAreaSqm: 93, LeaseCommence: 1987, ResalePrice: 680000},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.SyncPOIsBySource(ctx, "test:mrt", []store.POIRow{
		{Type: "mrt_exit", Name: "BISHAN A", Subtype: "A", Lat: 1.351, Lng: 103.846, Source: "test:mrt",
			Attrs: map[string]any{"station": "BISHAN"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Three runs: hdb ok, hdb failed, cea ok — oldest first.
	for _, r := range []struct {
		source string
		err    error
	}{{"hdb", nil}, {"hdb", errors.New("upstream 503")}, {"cea", nil}} {
		id, err := s.StartRun(ctx, r.source, store.ModeIncremental)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, id, 10, 4, r.err); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // distinct started_at
	}

	h := (&Server{Store: s, Sched: scheduler.New(s)}).Routes()
	get := func(path string, out any) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s → %d %s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}

	t.Run("runs", func(t *testing.T) {
		var page struct {
			Rows  []store.SyncRun `json:"rows"`
			Total int             `json:"total"`
		}
		get("/sync/runs", &page)
		if page.Total != 3 || len(page.Rows) != 3 || page.Rows[0].Source != "cea" {
			t.Fatalf("all runs = %d/%d, first %+v; want 3, newest (cea) first", len(page.Rows), page.Total, page.Rows)
		}
		get("/sync/runs?source=hdb&limit=1", &page)
		if page.Total != 2 || len(page.Rows) != 1 || page.Rows[0].Status != store.StatusError || page.Rows[0].Error != "upstream 503" {
			t.Fatalf("hdb page 1 = %+v total %d; want the failed run, total 2", page.Rows, page.Total)
		}
		get("/sync/runs?source=hdb&limit=1&offset=1", &page)
		if len(page.Rows) != 1 || page.Rows[0].Status != store.StatusSuccess {
			t.Fatalf("hdb page 2 = %+v", page.Rows)
		}
		get("/sync/runs?source=nope", &page)
		if page.Total != 0 || page.Rows == nil {
			t.Fatalf("unknown source must give an empty list, not null: %+v", page)
		}
	})

	t.Run("tables", func(t *testing.T) {
		var out struct {
			Tables []store.TableStat `json:"tables"`
		}
		get("/db/tables", &out)
		if len(out.Tables) != len(store.OpsTableNames()) {
			t.Fatalf("got %d tables, want %d", len(out.Tables), len(store.OpsTableNames()))
		}
		by := map[string]store.TableStat{}
		for _, ts := range out.Tables {
			by[ts.Table] = ts
		}
		if hr := by["hdb_resale"]; hr.RowCount != 1 || hr.SizeBytes <= 0 || hr.LastUpdate == nil {
			t.Fatalf("hdb_resale stat = %+v", hr)
		}
		if e := by["ura_rentals"]; e.RowCount != 0 || e.LastUpdate != nil {
			t.Fatalf("empty table must have no last_update: %+v", e)
		}
	})

	t.Run("preview every table", func(t *testing.T) {
		// Each allowlisted table's ORDER BY must be valid against the schema.
		for _, name := range store.OpsTableNames() {
			var p store.TablePreview
			get("/db/tables/"+name+"/rows", &p)
			if len(p.Columns) == 0 || p.Rows == nil || p.Limit != previewPageDefault {
				t.Errorf("%s: columns=%v rows=%v limit=%d", name, p.Columns, p.Rows, p.Limit)
			}
		}
	})

	t.Run("preview renders geo and hashes", func(t *testing.T) {
		var p store.TablePreview
		get("/db/tables/pois/rows?limit=9999", &p)
		if p.Limit != previewPageMax {
			t.Fatalf("limit not clamped: %d", p.Limit)
		}
		if p.Total != 1 || len(p.Rows) != 1 {
			t.Fatalf("pois preview = %+v", p)
		}
		if p.Columns[0] != "id" || p.Columns[1] != "type" {
			t.Fatalf("columns must keep schema order, got %v", p.Columns)
		}
		row := p.Rows[0]
		if g, _ := row["geom"].(string); !strings.HasPrefix(g, "POINT(103.846 1.351") {
			t.Errorf("geom = %#v, want WKT", row["geom"])
		}
		if h, _ := row["source_hash"].(string); !regexp.MustCompile(`^[0-9a-f]{32,}$`).MatchString(h) {
			t.Errorf("source_hash = %#v, want hex", row["source_hash"])
		}
		if a, _ := row["attrs"].(map[string]any); a["station"] != "BISHAN" {
			t.Errorf("attrs = %#v, want decoded jsonb", row["attrs"])
		}
	})
}
