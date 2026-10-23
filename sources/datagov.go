package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// retryOn429 wraps doFn with a simple retry loop for HTTP 429 responses.
// data.gov.sg asks callers to wait ~10s and suggests signing up for an API key.
// We retry up to maxRetries times with retryDelay between attempts.
const (
	dgMaxRetries = 3
	dgRetryDelay = 12 * time.Second
)

// HDB resale transactions dataset (master, all years).
// https://data.gov.sg/datasets/d_8b84c4ee58e3cfc0ece0d773c8ca6abc/view
const HDBResaleResourceID = "d_8b84c4ee58e3cfc0ece0d773c8ca6abc"

// HDB Property Information — block-level master with year_completed,
// max_floor_lvl, total_dwelling_units. ~13.3k rows total; ~10.7k residential.
// https://data.gov.sg/datasets/d_17f5382f26140b1fdae0ba2ef6239d2f/view
const HDBPropertyInfoResourceID = "d_17f5382f26140b1fdae0ba2ef6239d2f"

// CEA salesperson register — every valid real-estate salesperson licensed
// in Singapore from Jan 2011 onwards. CSV snapshot, ~37k rows.
// https://data.gov.sg/datasets/d_07c63be0f37e6e59c07a4ddc2fd87fcb/view
const CEASalespersonResourceID = "d_07c63be0f37e6e59c07a4ddc2fd87fcb"

// CEA salesperson transaction records (residential) — every HDB resale,
// HDB rental, private resale, private rental, and new sale facilitated
// by a CEA-licensed salesperson from Jan 2017 onwards. CSV snapshot,
// ~1.3M rows, refreshed monthly. Joins to cea_salespersons via
// salesperson_reg_num ↔ registration_no.
// https://data.gov.sg/datasets/d_ee7e46d3c57f7865790704632b0aef71/view
const CEATransactionResourceID = "d_ee7e46d3c57f7865790704632b0aef71"

type DataGov struct {
	http *http.Client
}

func NewDataGov() *DataGov {
	return &DataGov{http: &http.Client{Timeout: 45 * time.Second}}
}

type datastoreResp struct {
	Success bool `json:"success"`
	Result  struct {
		Records []map[string]any `json:"records"`
		Total   int              `json:"total"`
	} `json:"result"`
}

// DatastoreSearch calls data.gov.sg's CKAN datastore_search endpoint.
// filters is converted to a JSON object; datastore_search only supports equality.
func (d *DataGov) DatastoreSearch(ctx context.Context, resourceID string, filters map[string]string, limit int) ([]map[string]any, error) {
	return d.DatastoreSearchOffset(ctx, resourceID, filters, limit, 0)
}

// DatastoreSearchOffset is DatastoreSearch with pagination. The data engine's
// backfill ingestor uses it to page through the full HDB resale dataset.
func (d *DataGov) DatastoreSearchOffset(ctx context.Context, resourceID string, filters map[string]string, limit, offset int) ([]map[string]any, error) {
	u, _ := url.Parse("https://data.gov.sg/api/action/datastore_search")
	q := u.Query()
	q.Set("resource_id", resourceID)
	if limit <= 0 {
		limit = dataGovBatchDefault
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", offset))
	}
	if len(filters) > 0 {
		fb, _ := json.Marshal(filters)
		q.Set("filters", string(fb))
	}
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")

	var (
		resp    *http.Response
		body    []byte
		started time.Time
		err     error
	)
	for attempt := 0; attempt <= dgMaxRetries; attempt++ {
		if attempt > 0 {
			slog.Info("datagov retry after 429", "attempt", attempt, "wait", dgRetryDelay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(dgRetryDelay):
			}
			req, _ = http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
		}
		started = time.Now()
		resp, err = d.http.Do(req)
		if err != nil {
			slog.Warn("datagov request failed", "url", u.String(), "err", err, "duration_ms", time.Since(started).Milliseconds())
			return nil, err
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		slog.Info("datagov request",
			"resource_id", resourceID,
			"filters", filters,
			"limit", limit,
			"attempt", attempt,
			"status", resp.StatusCode,
			"bytes", len(body),
			"duration_ms", time.Since(started).Milliseconds())
		if resp.StatusCode != 429 {
			break
		}
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("datagov %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var dr datastoreResp
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, err
	}
	slog.Debug("datagov records", "resource_id", resourceID, "count", len(dr.Result.Records))
	return dr.Result.Records, nil
}

// downloadResp models the initiate-download / poll-download envelope returned
// by api-open.data.gov.sg. A successful initiate usually includes the presigned
// URL directly; if not, we poll until it becomes available.
type downloadResp struct {
	Code     int    `json:"code"`
	ErrorMsg string `json:"errorMsg"`
	Data     struct {
		Message string `json:"message"`
		URL     string `json:"url"`
	} `json:"data"`
}

// DownloadDataset returns a reader over the dataset file for a data.gov.sg
// dataset ID. It calls initiate-download to obtain a presigned S3 URL (polling
// poll-download if the URL is not ready yet), then streams the file. The format
// matches the dataset's native type (CSV, GeoJSON, etc.). The caller must Close
// the returned reader.
func (d *DataGov) DownloadDataset(ctx context.Context, datasetID string) (io.ReadCloser, error) {
	fileURL, err := d.fetchDownloadURL(ctx, datasetID)
	if err != nil {
		return nil, err
	}

	// The default client has a short timeout; dataset downloads can be tens of MB.
	dl := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := dl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("datagov download fetch: %w", err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("datagov download %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	slog.Info("datagov download started", "dataset_id", datasetID, "duration_ms", time.Since(started).Milliseconds())
	return resp.Body, nil
}

// DownloadDatasetCSV is an alias for DownloadDataset retained for existing call sites.
func (d *DataGov) DownloadDatasetCSV(ctx context.Context, datasetID string) (io.ReadCloser, error) {
	return d.DownloadDataset(ctx, datasetID)
}

// fetchDownloadURL resolves a dataset ID to a presigned S3 download URL. It
// calls initiate-download first and, if the URL isn't ready yet, falls back to
// polling poll-download until a URL is returned or the deadline is hit.
func (d *DataGov) fetchDownloadURL(ctx context.Context, datasetID string) (string, error) {
	initURL := fmt.Sprintf("https://api-open.data.gov.sg/v1/public/api/datasets/%s/initiate-download", datasetID)
	pollURL := fmt.Sprintf("https://api-open.data.gov.sg/v1/public/api/datasets/%s/poll-download", datasetID)

	fileURL, err := d.callDownloadEndpoint(ctx, initURL)
	if err != nil {
		return "", fmt.Errorf("initiate-download: %w", err)
	}
	if fileURL != "" {
		return fileURL, nil
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("datagov dataset %s: download url not ready after polling", datasetID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
		fileURL, err = d.callDownloadEndpoint(ctx, pollURL)
		if err != nil {
			return "", fmt.Errorf("poll-download: %w", err)
		}
		if fileURL != "" {
			return fileURL, nil
		}
	}
}

// callDownloadEndpoint hits an initiate/poll-download URL and returns the
// presigned file URL (or "" when the URL isn't ready yet). data.gov.sg
// rate-limits these endpoints aggressively — a couple of rapid calls and
// it returns 429 for a sustained window. During the nightly batch sync the
// budget is usually already spent by the time the small per-dataset
// downloads run (MRT station codes, schools, …), so without a retry the
// fetch fails outright and its caller silently degrades (e.g. the station
// code lookup returns an empty index, dropping every nearby_mrt line
// badge). Mirror the 429 backoff the datastore endpoints already use.
func (d *DataGov) callDownloadEndpoint(ctx context.Context, u string) (string, error) {
	var (
		resp *http.Response
		body []byte
		err  error
	)
	for attempt := 0; attempt <= dgMaxRetries; attempt++ {
		if attempt > 0 {
			slog.Info("datagov download retry after 429", "attempt", attempt, "wait", dgRetryDelay)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(dgRetryDelay):
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
		resp, err = d.http.Do(req)
		if err != nil {
			return "", err
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 429 {
			break
		}
	}
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var dr downloadResp
	if err := json.Unmarshal(body, &dr); err != nil {
		return "", err
	}
	if dr.Code != 0 {
		return "", fmt.Errorf("code=%d: %s", dr.Code, dr.ErrorMsg)
	}
	return dr.Data.URL, nil
}
