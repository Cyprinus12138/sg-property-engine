package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeURA serves the token endpoint and invokeUraDS; handler gets the
// service query and returns the Result JSON array for it.
func fakeURA(t *testing.T, tokenCalls, invokeCalls *atomic.Int32, result func(r *http.Request) string) *URA {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/uraDataService/insertNewToken/v1", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if r.Header.Get("AccessKey") != "ak" {
			_, _ = io.WriteString(w, `{"Status":"Failed","Message":"bad key"}`)
			return
		}
		_, _ = io.WriteString(w, `{"Status":"Success","Result":"tok-1"}`)
	})
	mux.HandleFunc("/uraDataService/invokeUraDS/v1", func(w http.ResponseWriter, r *http.Request) {
		invokeCalls.Add(1)
		if r.Header.Get("AccessKey") != "ak" || r.Header.Get("Token") != "tok-1" {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"Status":"Success","Result":%s}`, result(r))
	})
	u := NewURA("ak")
	u.SetHTTPClient(fakeServer(t, mux))
	return u
}

func TestURA_NoKeyFailsFast(t *testing.T) {
	u := NewURA("")
	if u.HasKey() {
		t.Fatal("HasKey with empty key")
	}
	if _, err := u.Invoke(context.Background(), ServiceURAResiPipeline, nil); err == nil || !strings.Contains(err.Error(), "URA_ACCESS_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestURA_TokenFetchedOnceAndResponsesCached(t *testing.T) {
	var tokens, invokes atomic.Int32
	u := fakeURA(t, &tokens, &invokes, func(r *http.Request) string {
		return fmt.Sprintf(`[{"service":%q,"batch":%q}]`, r.URL.Query().Get("service"), r.URL.Query().Get("batch"))
	})
	ctx := context.Background()

	a, err := u.Invoke(ctx, ServiceURAResiTransaction, map[string]string{"batch": "1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := u.Invoke(ctx, ServiceURAResiTransaction, map[string]string{"batch": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("cached body differs")
	}
	if _, err := u.Invoke(ctx, ServiceURAResiTransaction, map[string]string{"batch": "2"}); err != nil {
		t.Fatal(err)
	}
	if tokens.Load() != 1 {
		t.Errorf("token calls = %d, want 1", tokens.Load())
	}
	if invokes.Load() != 2 {
		t.Errorf("invoke calls = %d, want 2 (second batch-1 call served from cache)", invokes.Load())
	}
}

func TestURA_TokenFailure(t *testing.T) {
	var tokens, invokes atomic.Int32
	u := fakeURA(t, &tokens, &invokes, func(*http.Request) string { return "[]" })
	u.accessKey = "wrong"
	if _, err := u.Invoke(context.Background(), ServiceURAResiPipeline, nil); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err = %v", err)
	}
	if invokes.Load() != 0 {
		t.Error("must not invoke without a token")
	}
}

func TestURA_InvokeAllBatchesMergesFourBatches(t *testing.T) {
	var tokens, invokes atomic.Int32
	u := fakeURA(t, &tokens, &invokes, func(r *http.Request) string {
		return fmt.Sprintf(`[{"batch":%q}]`, r.URL.Query().Get("batch"))
	})
	rows, err := u.InvokeAllBatches(context.Background(), ServiceURAResiTransaction)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r["batch"].(string))
	}
	if !reflect.DeepEqual(got, []string{"1", "2", "3", "4"}) {
		t.Errorf("batches = %v", got)
	}
}

func TestURA_NonSuccessEnvelopeIsAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/uraDataService/insertNewToken/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Status":"Success","Result":"tok-1"}`)
	})
	mux.HandleFunc("/uraDataService/invokeUraDS/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Status":"Failed","Message":"Invalid refPeriod"}`)
	})
	u := NewURA("ak")
	u.SetHTTPClient(fakeServer(t, mux))

	if _, err := u.InvokeQuarters(context.Background(), ServiceURAResiRental, 1); err == nil || !strings.Contains(err.Error(), "Invalid refPeriod") {
		t.Fatalf("err = %v", err)
	}
}

func TestURA_InvokeQuartersAndMonthsSendRefPeriods(t *testing.T) {
	pinClock(t, time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC))
	var tokens, invokes atomic.Int32
	u := fakeURA(t, &tokens, &invokes, func(r *http.Request) string {
		return fmt.Sprintf(`[{"refPeriod":%q}]`, r.URL.Query().Get("refPeriod"))
	})
	ctx := context.Background()

	periods := func(rows []map[string]any) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r["refPeriod"].(string))
		}
		return out
	}
	q, err := u.InvokeQuarters(ctx, ServiceURAResiRental, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := periods(q), []string{"25q4", "25q3", "25q2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("quarters = %v, want %v", got, want)
	}
	m, err := u.InvokeMonths(ctx, ServiceURAResiDeveloperSale, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := periods(m), []string{"0126", "1225"}; !reflect.DeepEqual(got, want) {
		t.Errorf("months = %v, want %v", got, want)
	}
}

func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	old := sgNow
	sgNow = func() time.Time { return at }
	t.Cleanup(func() { sgNow = old })
}

func TestRecentRefPeriods_YearBoundaries(t *testing.T) {
	cases := []struct {
		at        time.Time
		quarters  []string
		monthlies []string
	}{
		// Q1: the previous completed quarter is Q4 of last year.
		{time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), []string{"25q4", "25q3"}, []string{"1225", "1125"}},
		// Mid-year.
		{time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), []string{"26q2", "26q1"}, []string{"0626", "0526"}},
		// Crossing two year boundaries in one window.
		{time.Date(2001, 4, 1, 0, 0, 0, 0, time.UTC), []string{"01q1", "00q4"}, []string{"0301", "0201"}},
	}
	for _, c := range cases {
		pinClock(t, c.at)
		if got := recentRefPeriods(2); !reflect.DeepEqual(got, c.quarters) {
			t.Errorf("%s quarters = %v, want %v", c.at.Format("2006-01-02"), got, c.quarters)
		}
		if got := recentRefPeriodsMonthly(2); !reflect.DeepEqual(got, c.monthlies) {
			t.Errorf("%s months = %v, want %v", c.at.Format("2006-01-02"), got, c.monthlies)
		}
	}
	pinClock(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if got := recentRefPeriods(5); len(got) != 5 || got[4] != "24q4" {
		t.Errorf("5 quarters = %v", got)
	}
}
