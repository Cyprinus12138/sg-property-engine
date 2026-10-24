package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/sources/sourcestest"
)

// fakeServer wires mux behind an httptest server and returns a client
// whose every request lands there.
func fakeServer(t *testing.T, mux http.Handler) *http.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sourcestest.Client(srv)
}

// fastRetries shrinks the data.gov.sg back-off for the duration of a test.
func fastRetries(t *testing.T) {
	t.Helper()
	oldRetry, oldPoll := dgRetryDelay, dgPollInterval
	dgRetryDelay, dgPollInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { dgRetryDelay, dgPollInterval = oldRetry, oldPoll })
}

func TestDatastoreSearch_BuildsQueryAndParsesRecords(t *testing.T) {
	var got *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("/api/action/datastore_search", func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = io.WriteString(w, `{"success":true,"result":{"total":2,"records":[{"town":"BISHAN"},{"town":"PUNGGOL"}]}}`)
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	recs, err := dg.DatastoreSearchOffset(context.Background(), HDBResaleResourceID,
		map[string]string{"month": "2024-03"}, 50, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1]["town"] != "PUNGGOL" {
		t.Fatalf("records = %v", recs)
	}
	if h := got.Header.Get(sourcestest.UpstreamHostHeader); h != "data.gov.sg" {
		t.Errorf("upstream host = %q", h)
	}
	q := got.URL.Query()
	if q.Get("resource_id") != HDBResaleResourceID || q.Get("limit") != "50" || q.Get("offset") != "100" {
		t.Errorf("query = %v", q)
	}
	var filters map[string]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &filters); err != nil || filters["month"] != "2024-03" {
		t.Errorf("filters = %q (%v)", q.Get("filters"), err)
	}
	if !strings.HasPrefix(got.Header.Get("User-Agent"), "sg-property-engine/") {
		t.Errorf("user agent = %q", got.Header.Get("User-Agent"))
	}
}

func TestDatastoreSearch_DefaultsAndOmitsEmptyParams(t *testing.T) {
	var q map[string][]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/action/datastore_search", func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		_, _ = io.WriteString(w, `{"success":true,"result":{"records":[]}}`)
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	if _, err := dg.DatastoreSearch(context.Background(), "d_x", nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := q["limit"]; len(got) != 1 || got[0] != "100" {
		t.Errorf("limit = %v, want default 100", got)
	}
	if _, ok := q["offset"]; ok {
		t.Error("offset 0 must be omitted")
	}
	if _, ok := q["filters"]; ok {
		t.Error("nil filters must be omitted")
	}
}

func TestDatastoreSearch_RetriesOn429ThenSucceeds(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/action/datastore_search", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"result":{"records":[{"a":1}]}}`)
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	recs, err := dg.DatastoreSearch(context.Background(), "d_x", nil, 1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestDatastoreSearch_GivesUpAfterMaxRetries(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/action/datastore_search", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "slow down")
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	_, err := dg.DatastoreSearch(context.Background(), "d_x", nil, 1)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want a 429 error", err)
	}
	if calls.Load() != dgMaxRetries+1 {
		t.Errorf("calls = %d, want %d", calls.Load(), dgMaxRetries+1)
	}
}

func TestDatastoreSearch_Non200IsAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/action/datastore_search", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	if _, err := dg.DatastoreSearch(context.Background(), "d_x", nil, 1); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadDataset_InitiateThenPollThenStream(t *testing.T) {
	fastRetries(t)
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/public/api/datasets/d_x/initiate-download", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"code":0,"data":{"message":"queued"}}`)
	})
	mux.HandleFunc("/v1/public/api/datasets/d_x/poll-download", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if polls.Add(1) < 2 {
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"url":"https://s3.example.test/bucket/d_x.csv"}}`)
	})
	mux.HandleFunc("/bucket/d_x.csv", func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get(sourcestest.UpstreamHostHeader); h != "s3.example.test" {
			t.Errorf("download host = %q", h)
		}
		_, _ = io.WriteString(w, "month,town\n2024-03,BISHAN\n")
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	rc, err := dg.DownloadDataset(context.Background(), "d_x")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "month,town\n2024-03,BISHAN\n" {
		t.Errorf("body = %q", body)
	}
	if polls.Load() != 2 {
		t.Errorf("polls = %d, want 2", polls.Load())
	}
}

func TestDownloadDataset_ErrorCodeSurfaces(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/public/api/datasets/d_x/initiate-download", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"code":17,"errorMsg":"dataset not found"}`)
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	if _, err := dg.DownloadDataset(context.Background(), "d_x"); err == nil || !strings.Contains(err.Error(), "dataset not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadDataset_FileNon200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/public/api/datasets/d_x/initiate-download", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"code":0,"data":{"url":"https://s3.example.test/gone.csv"}}`)
	})
	mux.HandleFunc("/gone.csv", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	})
	dg := NewDataGov()
	dg.SetHTTPClient(fakeServer(t, mux))

	if _, err := dg.DownloadDataset(context.Background(), "d_x"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}
