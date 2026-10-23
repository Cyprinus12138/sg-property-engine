package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// LTA DataMall client. https://datamall.lta.gov.sg/content/datamall/en/dynamic-data.html
// All endpoints require the AccountKey header and return OData-shaped JSON
// with a top-level "value" array, paginated at 500 rows per call via $skip.
type LTA struct {
	http       *http.Client
	accountKey string
}

type LTACreds struct {
	AccountKey string
}

func NewLTA(creds LTACreds) *LTA {
	return &LTA{
		http:       &http.Client{Timeout: 20 * time.Second},
		accountKey: creds.AccountKey,
	}
}

func (l *LTA) HasKey() bool { return l.accountKey != "" }

type LTABusStop struct {
	BusStopCode string  `json:"BusStopCode"`
	RoadName    string  `json:"RoadName"`
	Description string  `json:"Description"`
	Latitude    float64 `json:"Latitude"`
	Longitude   float64 `json:"Longitude"`
}

// BusStops fetches the full bus stop directory, paging 500 rows at a time
// until the server returns a short page.
func (l *LTA) BusStops(ctx context.Context) ([]LTABusStop, error) {
	if !l.HasKey() {
		return nil, fmt.Errorf("lta datamall not configured (set LTA_ACCOUNT_KEY)")
	}
	const pageSize = ltaBusStopPageSize
	var all []LTABusStop
	for skip := 0; ; skip += pageSize {
		page, err := l.fetchBusStopPage(ctx, skip)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
	}
	slog.Info("lta bus stops fetched", "count", len(all))
	return all, nil
}

// (Removed LTATrainStation / TrainStationCodes — the /TrainStationCodes
// endpoint on LTA Datamall does not exist; station codes are sourced
// from data.gov.sg dataset d_d312a5b127e1ae74299b8ae664cedd4e via the
// pois ingest instead.)

func (l *LTA) fetchBusStopPage(ctx context.Context, skip int) ([]LTABusStop, error) {
	u := fmt.Sprintf("https://datamall2.mytransport.sg/ltaodataservice/BusStops?$skip=%d", skip)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("AccountKey", l.accountKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("lta busstops %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Value []LTABusStop `json:"value"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	slog.Debug("lta busstops page",
		"skip", skip,
		"rows", len(r.Value),
		"duration_ms", time.Since(started).Milliseconds())
	return r.Value, nil
}
