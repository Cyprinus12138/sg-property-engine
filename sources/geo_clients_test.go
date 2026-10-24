package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOneMapSearch_ParsesResults(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/common/elastic/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("searchVal") != "123 BISHAN ST 12" || q.Get("returnGeom") != "Y" || q.Get("getAddrDetails") != "Y" {
			t.Errorf("query = %v", q)
		}
		_, _ = io.WriteString(w, `{"found":1,"results":[{"SEARCHVAL":"BISHAN LOFT","BLK_NO":"123","ROAD_NAME":"BISHAN STREET 12","POSTAL":"570123","LATITUDE":"1.3512","LONGITUDE":"103.8485"}]}`)
	})
	om := NewOneMap(OneMapCreds{})
	om.SetHTTPClient(fakeServer(t, mux))

	res, err := om.Search(context.Background(), "123 BISHAN ST 12")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Postal != "570123" || res[0].BlkNo != "123" || res[0].Latitude != "1.3512" {
		t.Fatalf("results = %+v", res)
	}
}

func TestOneMapSearch_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/common/elastic/search", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	om := NewOneMap(OneMapCreds{})
	om.SetHTTPClient(fakeServer(t, mux))
	if _, err := om.Search(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
}

func TestOneMapTheme_RequiresCreds(t *testing.T) {
	om := NewOneMap(OneMapCreds{Email: "a@example.com"})
	if om.HasAuth() {
		t.Fatal("HasAuth with no password")
	}
	if _, err := om.RetrieveTheme(context.Background(), "hawkercentre"); err == nil || !strings.Contains(err.Error(), "ONEMAP_EMAIL") {
		t.Fatalf("err = %v", err)
	}
}

func TestOneMapTheme_TokenCachedAndHeaderRowSkipped(t *testing.T) {
	var tokenCalls atomic.Int32
	expiry := time.Now().Add(72 * time.Hour).Unix()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/post/getToken", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPost || body["email"] != "a@example.com" || body["password"] != "pw" {
			http.Error(w, "bad creds", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"jwt-1","expiry_timestamp":"%d"}`, expiry)
	})
	mux.HandleFunc("/api/public/themesvc/retrieveTheme", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-1" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("queryName") != "hawkercentre" {
			t.Errorf("queryName = %q", r.URL.Query().Get("queryName"))
		}
		_, _ = io.WriteString(w, `{"SrchResults":[{"FeatCount":2,"Theme_Name":"Hawker Centres"},
			{"NAME":"Bishan North","LatLng":"1.35,103.84","ADDRESSPOSTALCODE":"570287"},
			{"NAME":"Old Airport Road","LatLng":"1.30,103.88"}]}`)
	})
	om := NewOneMap(OneMapCreds{Email: "a@example.com", Password: "pw"})
	om.SetHTTPClient(fakeServer(t, mux))

	for i := 0; i < 2; i++ {
		res, err := om.RetrieveTheme(context.Background(), "hawkercentre")
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 || res[0].Name != "Bishan North" || res[0].AddressPostalCode != "570287" {
			t.Fatalf("results = %+v", res)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Errorf("token calls = %d, want 1", tokenCalls.Load())
	}
	if om.tokenExpiry.Unix() != expiry {
		t.Errorf("token expiry = %v, want unix %d", om.tokenExpiry, expiry)
	}
}

func TestOneMapToken_RefreshedNearExpiry(t *testing.T) {
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/post/getToken", func(w http.ResponseWriter, r *http.Request) {
		n := tokenCalls.Add(1)
		_, _ = fmt.Fprintf(w, `{"access_token":"jwt-%d","expiry_timestamp":"%d"}`, n, time.Now().Add(time.Minute).Unix())
	})
	om := NewOneMap(OneMapCreds{Email: "a@example.com", Password: "pw"})
	om.SetHTTPClient(fakeServer(t, mux))

	// Expiry inside the 5-minute safety margin forces a refresh every time.
	a, _ := om.ensureToken(context.Background())
	b, _ := om.ensureToken(context.Background())
	if a == b || tokenCalls.Load() != 2 {
		t.Errorf("tokens %q,%q calls=%d: a token expiring within 5m must be refreshed", a, b, tokenCalls.Load())
	}
}

func TestLTABusStops_PagesUntilShortPage(t *testing.T) {
	var skips []int
	mux := http.NewServeMux()
	mux.HandleFunc("/ltaodataservice/BusStops", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("AccountKey") != "lk" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		skips = append(skips, skip)
		n := ltaBusStopPageSize
		if skip >= 2*ltaBusStopPageSize {
			n = 3
		}
		rows := make([]LTABusStop, n)
		for i := range rows {
			rows[i] = LTABusStop{BusStopCode: fmt.Sprintf("%05d", skip+i), Latitude: 1.3, Longitude: 103.8}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": rows})
	})
	l := NewLTA(LTACreds{AccountKey: "lk"})
	l.SetHTTPClient(fakeServer(t, mux))

	stops, err := l.BusStops(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 2*ltaBusStopPageSize+3 {
		t.Errorf("stops = %d", len(stops))
	}
	if fmt.Sprint(skips) != fmt.Sprintf("[0 %d %d]", ltaBusStopPageSize, 2*ltaBusStopPageSize) {
		t.Errorf("skips = %v", skips)
	}
}

func TestLTABusStops_NoKeyAndHTTPError(t *testing.T) {
	if _, err := NewLTA(LTACreds{}).BusStops(context.Background()); err == nil || !strings.Contains(err.Error(), "LTA_ACCOUNT_KEY") {
		t.Fatalf("no key err = %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ltaodataservice/BusStops", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "quota", http.StatusTooManyRequests)
	})
	l := NewLTA(LTACreds{AccountKey: "lk"})
	l.SetHTTPClient(fakeServer(t, mux))
	if _, err := l.BusStops(context.Background()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("http err = %v", err)
	}
}

func TestGoogleMapsSearchText_HeadersAndMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/places:searchText", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "gk" || !strings.Contains(r.Header.Get("X-Goog-FieldMask"), "places.location") {
			http.Error(w, "headers", http.StatusForbidden)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["textQuery"] != "The Sail @ Marina Bay" {
			t.Errorf("textQuery = %q", body["textQuery"])
		}
		_, _ = io.WriteString(w, `{"places":[{"name":"places/abc","formattedAddress":"2 Marina Blvd","displayName":{"text":"The Sail"},
			"location":{"latitude":1.2803,"longitude":103.8527},"postalAddress":{"postalCode":"018987"}}]}`)
	})
	g := NewGoogleMaps(GoogleMapsCreds{APIKey: "gk"})
	g.SetHTTPClient(fakeServer(t, mux))

	res, err := g.SearchText(context.Background(), "The Sail @ Marina Bay")
	if err != nil {
		t.Fatal(err)
	}
	want := GoogleMapsPlace{Name: "places/abc", DisplayName: "The Sail", FormattedAddress: "2 Marina Blvd", PostalCode: "018987", Latitude: 1.2803, Longitude: 103.8527}
	if len(res) != 1 || res[0] != want {
		t.Fatalf("res = %+v", res)
	}
}

func TestGoogleMaps_NoKey(t *testing.T) {
	g := NewGoogleMaps(GoogleMapsCreds{})
	if g.HasAuth() {
		t.Fatal("HasAuth with no key")
	}
	if _, err := g.SearchText(context.Background(), "x"); err == nil {
		t.Error("SearchText without key must fail")
	}
	if _, err := g.DirectionsTransit(context.Background(), 1, 2, 3, 4, false, 0); err == nil {
		t.Error("DirectionsTransit without key must fail")
	}
}

func TestGoogleMapsDirections_SummarisesBestRoute(t *testing.T) {
	var q map[string][]string
	mux := http.NewServeMux()
	mux.HandleFunc("/maps/api/directions/json", func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		_, _ = io.WriteString(w, `{"status":"OK","routes":[{"summary":"NSL","legs":[{
			"duration":{"value":30,"text":"1 min"},"distance":{"value":420,"text":"0.4 km"},
			"start_address":"A","end_address":"B","steps":[
				{"travel_mode":"WALKING"},
				{"travel_mode":"TRANSIT","transit_details":{"line":{"vehicle":{"type":"SUBWAY"}}}},
				{"travel_mode":"TRANSIT","transit_details":{"line":{"vehicle":{"type":"BUS"}}}},
				{"travel_mode":"TRANSIT","transit_details":{"line":{"vehicle":{"type":"SUBWAY"}}}}]}]}]}`)
	})
	g := NewGoogleMaps(GoogleMapsCreds{APIKey: "gk"})
	g.SetHTTPClient(fakeServer(t, mux))

	d, err := g.DirectionsTransit(context.Background(), 1.35, 103.84, 1.28, 103.85, true, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if d.DurationMinutes != 1 {
		t.Errorf("30 s must round up to 1 minute, got %d", d.DurationMinutes)
	}
	sort.Strings(d.TransitVehicles)
	if fmt.Sprint(d.TransitVehicles) != "[BUS SUBWAY]" || d.DistanceMeters != 420 || d.Summary != "NSL" {
		t.Errorf("directions = %+v", d)
	}
	if q["mode"][0] != "transit" || q["transit_mode"][0] != "bus" || q["departure_time"][0] != "1700000000" || q["origin"][0] != "1.350000,103.840000" {
		t.Errorf("query = %v", q)
	}
}

func TestGoogleMapsDirections_NotOKStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/maps/api/directions/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ZERO_RESULTS","routes":[]}`)
	})
	g := NewGoogleMaps(GoogleMapsCreds{APIKey: "gk"})
	g.SetHTTPClient(fakeServer(t, mux))
	if _, err := g.DirectionsTransit(context.Background(), 1, 2, 3, 4, false, 1); err == nil || !strings.Contains(err.Error(), "ZERO_RESULTS") {
		t.Fatalf("err = %v", err)
	}
}
