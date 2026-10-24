// Package client is a thin, cached HTTP client for the sg-property-engine
// query API. Downstream services (an LLM agent's tools, a web backend)
// call this instead of the upstream government APIs: one SQL-backed round
// trip per request, with geo filters, paging and stats handled upstream.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when the engine answers 404. Callers use it to
// distinguish "row missing" (a routine outcome) from transient failure.
var ErrNotFound = errors.New("sgpe: not found")

// Client talks to one engine instance. Safe for concurrent use.
type Client struct {
	BaseURL string
	http    *http.Client

	// In-memory GET-response cache. The same SQL-backed query running
	// twice within a minute is a common case for chat-style consumers
	// (a model re-runs a lookup after an unrelated step). TTL is short:
	// long enough to absorb a multi-step loop, short enough that fresh
	// data lands quickly after the nightly ingest.
	cacheMu  sync.RWMutex
	cacheMap map[string]cacheEntry
	cacheTTL time.Duration
}

type cacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// DefaultCacheTTL is the response-cache lifetime used by New.
const DefaultCacheTTL = 5 * time.Minute

// New builds a client for baseURL (default http://localhost:8081).
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: 30 * time.Second},
		cacheMap: make(map[string]cacheEntry),
		cacheTTL: DefaultCacheTTL,
	}
}

// WithHTTPClient swaps the underlying *http.Client (timeouts, transport).
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	if h != nil {
		c.http = h
	}
	return c
}

// WithCacheTTL overrides the response-cache lifetime; 0 disables caching.
func (c *Client) WithCacheTTL(ttl time.Duration) *Client {
	c.cacheTTL = ttl
	return c
}

// cacheKey returns a stable string for params irrespective of insertion
// order, prefixed with the path so endpoints don't collide.
func (c *Client) cacheKey(path string, params url.Values) string {
	if len(params) == 0 {
		return path
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(path)
	b.WriteByte('?')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.Join(params[k], ","))
	}
	return b.String()
}

func (c *Client) cacheGet(key string) ([]byte, bool) {
	if c.cacheTTL <= 0 {
		return nil, false
	}
	c.cacheMu.RLock()
	e, ok := c.cacheMap[key]
	c.cacheMu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.body, true
}

func (c *Client) cachePut(key string, body []byte) {
	if c.cacheTTL <= 0 {
		return
	}
	c.cacheMu.Lock()
	c.cacheMap[key] = cacheEntry{body: body, expiresAt: time.Now().Add(c.cacheTTL)}
	// Bound growth: sweep expired entries once the map gets large.
	if len(c.cacheMap) > 256 {
		now := time.Now()
		for k, v := range c.cacheMap {
			if now.After(v.expiresAt) {
				delete(c.cacheMap, k)
			}
		}
	}
	c.cacheMu.Unlock()
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	key := c.cacheKey(path, params)
	if cached, ok := c.cacheGet(key); ok {
		slog.Debug("sgpe client cache hit", "path", path, "bytes", len(cached))
		return json.Unmarshal(cached, out)
	}
	u := c.BaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return fmt.Errorf("%w: %s", ErrNotFound, truncate(string(body), 200))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("sgpe %s: %d %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	// Cache only successful responses — 4xx/5xx are transient or indicate
	// a bad query, neither of which we want to memoise.
	c.cachePut(key, body)
	return json.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------- HDB ----------------

type HDBTxn struct {
	Month          string   `json:"month"`
	Town           string   `json:"town"`
	Block          string   `json:"block"`
	Street         string   `json:"street_name"`
	FlatType       string   `json:"flat_type"`
	StoreyRange    string   `json:"storey_range"`
	FloorAreaSqm   float64  `json:"floor_area_sqm"`
	FlatModel      string   `json:"flat_model"`
	LeaseCommence  int      `json:"lease_commence"`
	RemainingLease string   `json:"remaining_lease"`
	ResalePrice    float64  `json:"resale_price"`
	PSF            float64  `json:"psf"`
	Lat            *float64 `json:"lat,omitempty"`
	Lng            *float64 `json:"lng,omitempty"`
	DistanceM      *float64 `json:"distance_m,omitempty"`
}

type HDBTxnPage struct {
	Rows   []HDBTxn `json:"rows"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

// HDBTransactions — GET /hdb/transactions.
func (c *Client) HDBTransactions(ctx context.Context, params url.Values) (*HDBTxnPage, error) {
	var p HDBTxnPage
	if err := c.get(ctx, "/hdb/transactions", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type StatsBucket struct {
	Key   string   `json:"key"`
	Value *float64 `json:"value"`
	Count int      `json:"count"`
}

type StatsResult struct {
	GroupBy string        `json:"group_by"`
	Metric  string        `json:"metric"`
	Buckets []StatsBucket `json:"buckets"`
}

// HDBStats — GET /hdb/stats.
func (c *Client) HDBStats(ctx context.Context, params url.Values) (*StatsResult, error) {
	var s StatsResult
	if err := c.get(ctx, "/hdb/stats", params, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ---------------- URA ----------------

type URATxn struct {
	Project       string   `json:"project"`
	Street        string   `json:"street"`
	District      string   `json:"district"`
	PropertyType  string   `json:"property_type"`
	Tenure        string   `json:"tenure"`
	MarketSegment string   `json:"market_segment"`
	AreaSqm       float64  `json:"area_sqm"`
	Price         float64  `json:"price"`
	PSF           float64  `json:"psf"`
	ContractDate  string   `json:"contract_date"`
	FloorRange    string   `json:"floor_range"`
	TypeOfSale    string   `json:"type_of_sale"`
	Lat           *float64 `json:"lat,omitempty"`
	Lng           *float64 `json:"lng,omitempty"`
	DistanceM     *float64 `json:"distance_m,omitempty"`
}

type URATxnPage struct {
	Rows   []URATxn `json:"rows"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

// URATransactions — GET /ura/transactions.
func (c *Client) URATransactions(ctx context.Context, params url.Values) (*URATxnPage, error) {
	var p URATxnPage
	if err := c.get(ctx, "/ura/transactions", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type URARental struct {
	Project      string   `json:"project"`
	Street       string   `json:"street"`
	District     string   `json:"district"`
	PropertyType string   `json:"property_type"`
	AreaSqmMin   *float64 `json:"area_sqm_min,omitempty"`
	AreaSqmMax   *float64 `json:"area_sqm_max,omitempty"`
	BedroomNum   *float64 `json:"bedroom_num,omitempty"`
	Rent         float64  `json:"rent"`
	LeaseDate    string   `json:"lease_date"`
	Lat          *float64 `json:"lat,omitempty"`
	Lng          *float64 `json:"lng,omitempty"`
	DistanceM    *float64 `json:"distance_m,omitempty"`
}

type URARentalPage struct {
	Rows   []URARental `json:"rows"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// URARentals — GET /ura/rentals.
func (c *Client) URARentals(ctx context.Context, params url.Values) (*URARentalPage, error) {
	var p URARentalPage
	if err := c.get(ctx, "/ura/rentals", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type RentalTrendMonth struct {
	Month      string `json:"month"`
	Txns       int64  `json:"txns"`
	P25        int64  `json:"p25"`
	MedianRent int64  `json:"median_rent"`
	P75        int64  `json:"p75"`
}

type RentalTrend struct {
	Months []RentalTrendMonth `json:"months"`
	Total  int                `json:"total"`
}

// URARentalsTrend — GET /ura/rentals/trend (monthly P25/median/P75).
func (c *Client) URARentalsTrend(ctx context.Context, params url.Values) (*RentalTrend, error) {
	var t RentalTrend
	if err := c.get(ctx, "/ura/rentals/trend", params, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

type URAPipelineRow struct {
	Project       string `json:"project"`
	Developer     string `json:"developer"`
	Street        string `json:"street"`
	District      string `json:"district"`
	MarketSegment string `json:"market_segment"`
	TotalUnits    int    `json:"total_units"`
	UnitsSold     int    `json:"units_sold"`
	ExpectedTop   string `json:"expected_top"`
	Status        string `json:"status"`
}

type URAPipelinePage struct {
	Rows  []URAPipelineRow `json:"rows"`
	Total int              `json:"total"`
}

// URAPipeline — GET /ura/pipeline.
func (c *Client) URAPipeline(ctx context.Context, params url.Values) (*URAPipelinePage, error) {
	var p URAPipelinePage
	if err := c.get(ctx, "/ura/pipeline", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type URADeveloperSale struct {
	Project         string   `json:"project"`
	Developer       string   `json:"developer"`
	Street          string   `json:"street"`
	District        string   `json:"district"`
	MarketSegment   string   `json:"market_segment"`
	PropertyType    string   `json:"property_type"`
	RefPeriod       string   `json:"ref_period"`
	UnitsAvail      int      `json:"units_avail"`
	LaunchedToDate  int      `json:"launched_to_date"`
	SoldToDate      int      `json:"sold_to_date"`
	LaunchedInMonth int      `json:"launched_in_month"`
	SoldInMonth     int      `json:"sold_in_month"`
	LowestPrice     float64  `json:"lowest_price"`
	MedianPrice     float64  `json:"median_price"`
	HighestPrice    float64  `json:"highest_price"`
	Lat             *float64 `json:"lat,omitempty"`
	Lng             *float64 `json:"lng,omitempty"`
}

type URADeveloperSalesPage struct {
	Rows   []URADeveloperSale `json:"rows"`
	Total  int                `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

// URADeveloperSales — GET /ura/developer-sales.
func (c *Client) URADeveloperSales(ctx context.Context, params url.Values) (*URADeveloperSalesPage, error) {
	var p URADeveloperSalesPage
	if err := c.get(ctx, "/ura/developer-sales", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---------------- Projects ----------------

type Project struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"display_name"`
	Street        *string  `json:"street,omitempty"`
	District      *string  `json:"district,omitempty"`
	PropertyTypes []string `json:"property_types,omitempty"`
	Tenure        *string  `json:"tenure,omitempty"`
	MarketSegment *string  `json:"market_segment,omitempty"`
	Postal        *string  `json:"postal,omitempty"`
	Lat           *float64 `json:"lat,omitempty"`
	Lng           *float64 `json:"lng,omitempty"`
	PlanningArea  *string  `json:"planning_area,omitempty"`
	FirstTxnDate  *string  `json:"first_txn_date,omitempty"`
	LastTxnDate   *string  `json:"last_txn_date,omitempty"`
	TxnCount      int      `json:"txn_count"`
	DistanceM     *float64 `json:"distance_m,omitempty"`
}

type ProjectPage struct {
	Rows   []Project `json:"rows"`
	Total  int       `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// Projects — GET /projects.
func (c *Client) Projects(ctx context.Context, params url.Values) (*ProjectPage, error) {
	var p ProjectPage
	if err := c.get(ctx, "/projects", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ProjectDetail mirrors GET /projects/{name}: the master row plus the
// nearby_mrt enrichment.
type ProjectDetail struct {
	Project
	NearbyMRT []MRTStation `json:"nearby_mrt"`
}

// MRTStation mirrors store.Station — duplicated here so client callers
// don't import the store package. Keep field tags in sync.
type MRTStation struct {
	Name        string   `json:"station"`
	Type        string   `json:"type"`  // "mrt_exit" | "lrt_exit"
	Codes       []string `json:"codes"` // ["EW16","NE3","TE17"] for Outram Park
	Lines       []string `json:"lines"` // ["EWL","NEL","TEL"]
	DistanceM   int      `json:"distance_m"`
	WalkMin     int      `json:"walk_min"`
	NearestExit string   `json:"nearest_exit,omitempty"`
}

// Project — GET /projects/{name}. Returns ErrNotFound for unknown names.
func (c *Client) Project(ctx context.Context, name string) (*ProjectDetail, error) {
	var d ProjectDetail
	if err := c.get(ctx, "/projects/"+url.PathEscape(name), nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ---------------- POIs & geo ----------------

type POI struct {
	ID        int64    `json:"id"`
	Type      string   `json:"type"`
	Name      string   `json:"name"`
	Subtype   *string  `json:"subtype,omitempty"`
	Lat       float64  `json:"lat"`
	Lng       float64  `json:"lng"`
	DistanceM *float64 `json:"distance_m,omitempty"`
}

type POIPage struct {
	Rows   []POI `json:"rows"`
	Total  int   `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

// POIs — GET /pois.
func (c *Client) POIs(ctx context.Context, params url.Values) (*POIPage, error) {
	var p POIPage
	if err := c.get(ctx, "/pois", params, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// POITypeCount mirrors one row of GET /pois/types.
type POITypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// POITypes — GET /pois/types: every ingested POI type with its row count.
func (c *Client) POITypes(ctx context.Context) ([]POITypeCount, error) {
	var out struct {
		Types []POITypeCount `json:"types"`
	}
	if err := c.get(ctx, "/pois/types", nil, &out); err != nil {
		return nil, err
	}
	return out.Types, nil
}

type NearestStations struct {
	Lat      float64      `json:"lat"`
	Lng      float64      `json:"lng"`
	Limit    int          `json:"limit"`
	WithinM  int          `json:"within_m"`
	Stations []MRTStation `json:"stations"`
}

// NearestStations — GET /geo/nearest-stations for an arbitrary point.
func (c *Client) NearestStations(ctx context.Context, lat, lng float64, limit, withinM int) (*NearestStations, error) {
	q := url.Values{
		"lat": {fmt.Sprintf("%.6f", lat)},
		"lng": {fmt.Sprintf("%.6f", lng)},
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if withinM > 0 {
		q.Set("within_m", fmt.Sprint(withinM))
	}
	var out NearestStations
	if err := c.get(ctx, "/geo/nearest-stations", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------- Search ----------------

type SearchRow struct {
	Kind          string   `json:"kind"` // "project" | "hdb"
	Name          string   `json:"name"`
	Subtitle      string   `json:"subtitle,omitempty"`
	District      *string  `json:"district,omitempty"`
	Tenure        *string  `json:"tenure,omitempty"`
	PropertyTypes []string `json:"property_types,omitempty"`
	MarketSegment *string  `json:"market_segment,omitempty"`
	Block         *string  `json:"block,omitempty"`
	StreetName    *string  `json:"street_name,omitempty"`
	EstateName    *string  `json:"estate_name,omitempty"`
	YearCompleted *int     `json:"year_completed,omitempty"`
	Street        *string  `json:"street,omitempty"`
	Postal        *string  `json:"postal,omitempty"`
	Lat           *float64 `json:"lat,omitempty"`
	Lng           *float64 `json:"lng,omitempty"`
	PlanningArea  *string  `json:"planning_area,omitempty"`
}

// SearchProperties — GET /search/properties?q=…: projects + HDB blocks.
func (c *Client) SearchProperties(ctx context.Context, q string, limit int) ([]SearchRow, error) {
	params := url.Values{"q": {q}}
	if limit > 0 {
		params.Set("limit", fmt.Sprint(limit))
	}
	var out struct {
		Rows []SearchRow `json:"rows"`
	}
	if err := c.get(ctx, "/search/properties", params, &out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}

// ---------------- Explore aggregations ----------------

type HottestProject struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	District     *string  `json:"district,omitempty"`
	PropertyType *string  `json:"property_type,omitempty"`
	TxnCount     int      `json:"txn_count"`
	MedianPrice  *float64 `json:"median_price,omitempty"`
	MedianPSF    *float64 `json:"median_psf,omitempty"`
	PSFChangePct *float64 `json:"psf_change_pct,omitempty"`
	Direction    string   `json:"direction"`
}

// Hottest — GET /explore/hottest.
func (c *Client) Hottest(ctx context.Context, limit int) ([]HottestProject, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", fmt.Sprint(limit))
	}
	var out struct {
		Rows []HottestProject `json:"rows"`
	}
	if err := c.get(ctx, "/explore/hottest", params, &out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}

// DistrictMedianResp mirrors GET /explore/district_median. Zero samples
// means "no signal", not "S$0" — callers must check SampleCount.
type DistrictMedianResp struct {
	District    string   `json:"district"`
	WindowDays  int      `json:"window_days"`
	SampleCount int      `json:"sample_count"`
	AnchorDate  *string  `json:"anchor_date,omitempty"`
	MedianPrice *float64 `json:"median_price,omitempty"`
	MedianPSF   *float64 `json:"median_psf,omitempty"`
	MinAreaSqm  *float64 `json:"min_area_sqm,omitempty"`
	MaxAreaSqm  *float64 `json:"max_area_sqm,omitempty"`
}

// DistrictMedian — GET /explore/district_median.
func (c *Client) DistrictMedian(ctx context.Context, params url.Values) (*DistrictMedianResp, error) {
	var out DistrictMedianResp
	if err := c.get(ctx, "/explore/district_median", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------- CEA salesperson register ----------------

type CEASalesperson struct {
	SalespersonName       string    `json:"salesperson_name"`
	RegistrationNo        string    `json:"registration_no"`
	RegistrationStartDate time.Time `json:"registration_start_date"`
	RegistrationEndDate   time.Time `json:"registration_end_date"`
	EstateAgentName       string    `json:"estate_agent_name"`
	EstateAgentLicenseNo  string    `json:"estate_agent_license_no"`
	Active                bool      `json:"active"`
}

// CEALookup — GET /cea/lookup. ErrNotFound when the number is absent.
func (c *Client) CEALookup(ctx context.Context, registrationNo string) (*CEASalesperson, error) {
	q := url.Values{"registration_no": {registrationNo}}
	var out CEASalesperson
	if err := c.get(ctx, "/cea/lookup", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CEAPracticeStats mirrors store.CEAPracticeStats.
type CEAPracticeStats struct {
	RegistrationNo string             `json:"registration_no"`
	Mode           string             `json:"mode"`
	WindowFrom     time.Time          `json:"window_from"`
	WindowTo       time.Time          `json:"window_to"`
	Deals          int                `json:"deals"`
	PriorDeals     int                `json:"prior_deals"`
	ActiveMonths   int                `json:"active_months"`
	LicensedSince  *time.Time         `json:"licensed_since,omitempty"`
	Monthly        []CEAStatsMonthly  `json:"monthly"`
	Towns          []CEAStatsTown     `json:"towns"`
	Representation []CEAStatsCategory `json:"representation"`
	TxTypes        []CEAStatsCategory `json:"tx_types"`
}

type CEAStatsMonthly struct {
	Month time.Time `json:"month"`
	Count int       `json:"count"`
}

type CEAStatsTown struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type CEAStatsCategory struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// CEAPracticeStats — GET /cea/practice-stats. mode is "sale" or "rent".
func (c *Client) CEAPracticeStats(ctx context.Context, registrationNo, mode string) (*CEAPracticeStats, error) {
	q := url.Values{
		"reg_no": {registrationNo},
		"mode":   {mode},
	}
	var out CEAPracticeStats
	if err := c.get(ctx, "/cea/practice-stats", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------- Ops ----------------

// SyncRun mirrors a sync_runs row as served by /sync/runs/{id} and
// /sync/status.
type SyncRun struct {
	ID           int64      `json:"id"`
	Source       string     `json:"source"`
	Mode         string     `json:"mode"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Status       string     `json:"status"`
	RowsIn       int        `json:"rows_in"`
	RowsUpserted int        `json:"rows_upserted"`
	Stage        string     `json:"stage,omitempty"`
	Progress     float64    `json:"progress"`
	Error        string     `json:"error,omitempty"`
}

// SyncStatus — GET /sync/status: latest run per source plus known sources.
func (c *Client) SyncStatus(ctx context.Context) (runs []SyncRun, known []string, err error) {
	var out struct {
		Sources []SyncRun `json:"sources"`
		Known   []string  `json:"known"`
	}
	// Bypass the cache: ops status must be live.
	ttl := c.cacheTTL
	c.cacheTTL = 0
	err = c.get(ctx, "/sync/status", nil, &out)
	c.cacheTTL = ttl
	return out.Sources, out.Known, err
}

// TriggerSync — POST /sync/run. Returns the new run id; a 409 (source
// already running) surfaces as an error whose text contains "409".
func (c *Client) TriggerSync(ctx context.Context, source, mode, arg string) (int64, error) {
	q := url.Values{"source": {source}}
	if mode != "" {
		q.Set("mode", mode)
	}
	if arg != "" {
		q.Set("arg", arg)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/sync/run?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 202 {
		return 0, fmt.Errorf("sgpe /sync/run: %d %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	return out.RunID, nil
}

// ---------------- helpers ----------------

// PaginateAll keeps calling fetch until every row is collected or cap is
// hit. cap=0 means no cap.
func PaginateAll[T any](
	ctx context.Context,
	cap int,
	fetch func(ctx context.Context, limit, offset int) (rows []T, total int, err error),
) ([]T, error) {
	const pageSize = 500
	var out []T
	offset := 0
	for {
		rows, total, err := fetch(ctx, pageSize, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		offset += len(rows)
		if len(rows) < pageSize || offset >= total {
			break
		}
		if cap > 0 && len(out) >= cap {
			break
		}
	}
	return out, nil
}
