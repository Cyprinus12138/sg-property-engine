package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_ParsesPagesAndCachesGETs(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/hdb/transactions":
			if r.URL.Query().Get("town") != "BISHAN" {
				t.Errorf("town param not forwarded: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rows":[{"month":"2024-03-01","town":"BISHAN","resale_price":680000,"psf":679.2}],"total":1,"limit":50,"offset":0}`))
		case "/projects/THE SAIL": // httptest hands the handler the decoded path
			_, _ = w.Write([]byte(`{"name":"THE SAIL","display_name":"The Sail","txn_count":3,"nearby_mrt":[{"station":"Downtown","type":"mrt_exit","codes":["DT17"],"lines":["DTL"],"distance_m":210,"walk_min":3}]}`))
		case "/projects/NOPE":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	page, err := c.HDBTransactions(ctx, url.Values{"town": {"BISHAN"}})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].ResalePrice != 680000 {
		t.Fatalf("page = %+v", page)
	}
	// Second identical call is served from cache.
	if _, err := c.HDBTransactions(ctx, url.Values{"town": {"BISHAN"}}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit (cache), got %d", hits.Load())
	}

	d, err := c.Project(ctx, "THE SAIL")
	if err != nil {
		t.Fatal(err)
	}
	if d.DisplayName != "The Sail" || len(d.NearbyMRT) != 1 || d.NearbyMRT[0].Codes[0] != "DT17" {
		t.Fatalf("project detail = %+v", d)
	}

	_, err = c.Project(ctx, "NOPE")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 must map to ErrNotFound, got %v", err)
	}
	// Errors are never cached: a retry hits upstream again.
	before := hits.Load()
	_, _ = c.Project(ctx, "NOPE")
	if hits.Load() != before+1 {
		t.Fatal("404 response must not be cached")
	}
}

func TestClient_CacheDisabledWithZeroTTL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"rows":[],"total":0,"limit":50,"offset":0}`))
	}))
	defer srv.Close()
	c := New(srv.URL).WithCacheTTL(0)
	for i := 0; i < 3; i++ {
		if _, err := c.Projects(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("cache disabled: want 3 hits, got %d", hits.Load())
	}
}

func TestCacheKey_OrderIndependent(t *testing.T) {
	c := New("")
	a := c.cacheKey("/p", url.Values{"b": {"2"}, "a": {"1"}})
	b := c.cacheKey("/p", url.Values{"a": {"1"}, "b": {"2"}})
	if a != b {
		t.Fatalf("cache keys differ by insertion order: %q vs %q", a, b)
	}
	if c.cacheKey("/p", nil) != "/p" {
		t.Fatal("no params → bare path")
	}
}

func TestTriggerSyncAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/sync/run":
			if r.URL.Query().Get("source") != "hdb" || r.URL.Query().Get("mode") != "backfill" {
				t.Errorf("bad query: %s", r.URL.RawQuery)
			}
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"run_id":42,"source":"hdb","mode":"backfill"}`))
		case r.URL.Path == "/sync/status":
			_, _ = w.Write([]byte(`{"sources":[{"id":42,"source":"hdb","mode":"backfill","started_at":"2026-01-01T00:00:00Z","status":"running","progress":12.5}],"known":["hdb","geo"]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	id, err := c.TriggerSync(context.Background(), "hdb", "backfill", "")
	if err != nil || id != 42 {
		t.Fatalf("TriggerSync = %d, %v", id, err)
	}
	runs, known, err := c.SyncStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Progress != 12.5 || len(known) != 2 {
		t.Fatalf("status = %+v %v", runs, known)
	}
	if runs[0].StartedAt.IsZero() || runs[0].StartedAt.Year() != 2026 {
		t.Fatalf("started_at not parsed: %v", runs[0].StartedAt)
	}
}

func TestPaginateAll(t *testing.T) {
	calls := 0
	rows, err := PaginateAll(context.Background(), 0, func(_ context.Context, limit, offset int) ([]int, int, error) {
		calls++
		const total = 1200
		n := limit
		if offset+n > total {
			n = total - offset
		}
		out := make([]int, n)
		return out, total, nil
	})
	if err != nil || len(rows) != 1200 || calls != 3 {
		t.Fatalf("len=%d calls=%d err=%v", len(rows), calls, err)
	}
	rows, _ = PaginateAll(context.Background(), 600, func(_ context.Context, limit, offset int) ([]int, int, error) {
		return make([]int, limit), 5000, nil
	})
	if len(rows) != 1000 { // cap is checked after each full page
		t.Fatalf("cap: len=%d", len(rows))
	}
	_ = time.Second
}
