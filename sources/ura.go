package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// URA Data Service API client.
// https://www.ura.gov.sg/maps/api/
//
// Flow: obtain a daily Token using the AccessKey, then call invokeUraDS
// endpoints with both AccessKey and Token headers.
type URA struct {
	accessKey string
	http      *http.Client

	mu             sync.Mutex
	token          string
	tokenExpiresAt time.Time

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	body     json.RawMessage
	expireAt time.Time
}

func NewURA(accessKey string) *URA {
	return &URA{
		accessKey: accessKey,
		http:      &http.Client{Timeout: 60 * time.Second},
		cache:     map[string]cacheEntry{},
	}
}

func (u *URA) HasKey() bool { return u.accessKey != "" }

// SetHTTPClient replaces the client used for every call (proxy, custom
// timeouts, a test transport).
func (u *URA) SetHTTPClient(c *http.Client) { u.http = c }

func (u *URA) token_(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.token != "" && time.Now().Before(u.tokenExpiresAt) {
		return u.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://eservice.ura.gov.sg/uraDataService/insertNewToken/v1", nil)
	req.Header.Set("AccessKey", u.accessKey)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r URATokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Status != "Success" || r.Result == "" {
		return "", fmt.Errorf("ura token: %s", r.Message)
	}
	u.token = r.Result
	u.tokenExpiresAt = time.Now().Add(23 * time.Hour)
	return u.token, nil
}

// Invoke calls a URA Data Service endpoint. Response body is returned raw
// because different services have different payload shapes.
func (u *URA) Invoke(ctx context.Context, service string, extra map[string]string) (json.RawMessage, error) {
	if u.accessKey == "" {
		return nil, fmt.Errorf("URA_ACCESS_KEY not configured")
	}

	// 6h in-memory cache, URA data only updates monthly.
	cacheKey := service
	for k, v := range extra {
		cacheKey += "|" + k + "=" + v
	}
	u.cacheMu.Lock()
	if e, ok := u.cache[cacheKey]; ok && time.Now().Before(e.expireAt) {
		u.cacheMu.Unlock()
		slog.Debug("ura cache hit", "service", service, "params", extra)
		return e.body, nil
	}
	u.cacheMu.Unlock()

	tok, err := u.token_(ctx)
	if err != nil {
		return nil, err
	}
	urlStr := "https://eservice.ura.gov.sg/uraDataService/invokeUraDS/v1?service=" + service
	for k, v := range extra {
		urlStr += "&" + k + "=" + v
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	req.Header.Set("AccessKey", u.accessKey)
	req.Header.Set("Token", tok)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := u.http.Do(req)
	if err != nil {
		slog.Warn("ura request failed", "service", service, "params", extra, "err", err, "duration_ms", time.Since(started).Milliseconds())
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	slog.Info("ura request",
		"service", service,
		"params", extra,
		"status", resp.StatusCode,
		"bytes", len(body),
		"duration_ms", time.Since(started).Milliseconds())
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ura %s: http %d: %s", service, resp.StatusCode, string(body))
	}

	u.cacheMu.Lock()
	u.cache[cacheKey] = cacheEntry{body: body, expireAt: time.Now().Add(6 * time.Hour)}
	u.cacheMu.Unlock()

	return body, nil
}

// InvokeAllBatches fetches batches 1..4 (quarters) for a service and merges
// the Result arrays. Used by PMI_Resi_Transaction and PMI_Resi_Rental.
func (u *URA) InvokeAllBatches(ctx context.Context, service string) ([]map[string]any, error) {
	var all []map[string]any
	for b := 1; b <= 4; b++ {
		body, err := u.Invoke(ctx, service, map[string]string{"batch": fmt.Sprintf("%d", b)})
		if err != nil {
			return nil, err
		}
		var parsed URAGenericEnvelope
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		if parsed.Status != "Success" {
			return nil, fmt.Errorf("ura %s batch %d not success: %s", service, b, parsed.Message)
		}
		all = append(all, parsed.Result...)
	}
	return all, nil
}

func (u *URA) InvokeQuarters(ctx context.Context, service string, quarterNum int) ([]map[string]any, error) {
	var all []map[string]any
	for _, q := range recentRefPeriods(quarterNum) {
		body, err := u.Invoke(ctx, service, map[string]string{"refPeriod": q})
		if err != nil {
			return nil, err
		}
		var parsed URAGenericEnvelope
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		if parsed.Status != "Success" {
			return nil, fmt.Errorf("ura %s quarter %s not success: %s", service, q, parsed.Message)
		}
		all = append(all, parsed.Result...)
	}
	return all, nil
}

// InvokeMonths fetches `monthNum` consecutive monthly ref periods (newest first)
// for services that take a monthly mmyy refPeriod — currently only
// PMI_Resi_Developer_Sales. Each returned record carries its ref period
// inside its developerSales[] entry, so the caller has to thread the period
// through when flattening.
func (u *URA) InvokeMonths(ctx context.Context, service string, monthNum int) ([]map[string]any, error) {
	var all []map[string]any
	for _, m := range recentRefPeriodsMonthly(monthNum) {
		body, err := u.Invoke(ctx, service, map[string]string{"refPeriod": m})
		if err != nil {
			return nil, err
		}
		var parsed URAGenericEnvelope
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		if parsed.Status != "Success" {
			return nil, fmt.Errorf("ura %s month %s not success: %s", service, m, parsed.Message)
		}
		all = append(all, parsed.Result...)
	}
	return all, nil
}

// recentRefPeriods returns the last n URA quarterly ref periods in yyqn
// format (e.g. "14q1"), newest first. URA docs specify lowercase `q`:
// "Field is in format of yyqq e.g. 14q1 represents 2014 1st quarter."
// URA data typically lags by ~1 quarter, so we start from the previous
// completed quarter rather than the in-progress one.
func recentRefPeriods(n int) []string {
	t := sgNow()
	q := (int(t.Month())-1)/3 + 1
	year := t.Year()
	q--
	if q == 0 {
		q = 4
		year--
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%02dq%d", year%100, q))
		q--
		if q == 0 {
			q = 4
			year--
		}
	}
	return out
}

// recentRefPeriodsMonthly returns the last n URA monthly ref periods in mmyy
// format (e.g. "0913" for Sep 2013), newest first. URA's developer-sales
// dataset refreshes "end of day on the 15th" for the *previous* month, so we
// always start from the prior calendar month rather than the in-progress one.
func recentRefPeriodsMonthly(n int) []string {
	t := sgNow()
	year := t.Year()
	month := int(t.Month()) - 1
	if month == 0 {
		month = 12
		year--
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%02d%02d", month, year%100))
		month--
		if month == 0 {
			month = 12
			year--
		}
	}
	return out
}

// sgNow is a var so tests can pin the clock the ref periods are derived from.
var sgNow = func() time.Time {
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		return time.Now().UTC().Add(8 * time.Hour)
	}
	return time.Now().In(loc)
}
