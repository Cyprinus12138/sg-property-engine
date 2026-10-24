package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// OneMap public search API + authenticated Themes API.
// https://www.onemap.gov.sg/apidocs/
// https://www.onemap.gov.sg/apidocs/authentication
// https://www.onemap.gov.sg/apidocs/themes
type OneMap struct {
	http *http.Client

	email    string
	password string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

type OneMapCreds struct {
	Email    string
	Password string
}

func NewOneMap(creds OneMapCreds) *OneMap {
	return &OneMap{
		http:     &http.Client{Timeout: 20 * time.Second},
		email:    creds.Email,
		password: creds.Password,
	}
}

func (o *OneMap) HasAuth() bool { return o.email != "" && o.password != "" }

// SetHTTPClient replaces the client used for every call (proxy, custom
// timeouts, a test transport).
func (o *OneMap) SetHTTPClient(c *http.Client) { o.http = c }

type OneMapResult struct {
	SearchVal string `json:"SEARCHVAL"`
	BlkNo     string `json:"BLK_NO"`
	Road      string `json:"ROAD_NAME"`
	Building  string `json:"BUILDING"`
	Address   string `json:"ADDRESS"`
	Postal    string `json:"POSTAL"`
	Latitude  string `json:"LATITUDE"`
	Longitude string `json:"LONGITUDE"`
}

func (o *OneMap) Search(ctx context.Context, query string) ([]OneMapResult, error) {
	u := "https://www.onemap.gov.sg/api/common/elastic/search?searchVal=" +
		url.QueryEscape(query) + "&returnGeom=Y&getAddrDetails=Y&pageNum=1"
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := o.http.Do(req)
	if err != nil {
		slog.Warn("onemap request failed", "query", query, "err", err, "duration_ms", time.Since(started).Milliseconds())
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		slog.Warn("onemap non-200", "query", query, "status", resp.StatusCode, "duration_ms", time.Since(started).Milliseconds())
		return nil, fmt.Errorf("onemap %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Results []OneMapResult `json:"results"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	slog.Info("onemap search",
		"query", query,
		"status", resp.StatusCode,
		"results", len(r.Results),
		"bytes", len(body),
		"duration_ms", time.Since(started).Milliseconds())
	return r.Results, nil
}

// ensureToken returns a cached OneMap API token, refreshing it if the
// cached copy is missing or within 5 minutes of expiry. Tokens are
// typically valid for ~3 days.
func (o *OneMap) ensureToken(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" && time.Now().Add(5*time.Minute).Before(o.tokenExpiry) {
		return o.token, nil
	}
	if o.email == "" || o.password == "" {
		return "", fmt.Errorf("onemap auth not configured (set ONEMAP_EMAIL and ONEMAP_PASSWORD)")
	}
	payload, _ := json.Marshal(map[string]string{"email": o.email, "password": o.password})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://www.onemap.gov.sg/api/auth/post/getToken", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("onemap token %d: %s", resp.StatusCode, string(raw))
	}
	var r struct {
		AccessToken     string `json:"access_token"`
		ExpiryTimestamp string `json:"expiry_timestamp"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("onemap token: empty access_token in response: %s", string(raw))
	}
	o.token = r.AccessToken
	if secs, perr := strconv.ParseInt(r.ExpiryTimestamp, 10, 64); perr == nil && secs > 0 {
		o.tokenExpiry = time.Unix(secs, 0)
	} else {
		o.tokenExpiry = time.Now().Add(24 * time.Hour)
	}
	slog.Info("onemap token refreshed",
		"expires_at", o.tokenExpiry.Format(time.RFC3339),
		"duration_ms", time.Since(started).Milliseconds())
	return o.token, nil
}

type OneMapThemeResult struct {
	Name                string `json:"NAME"`
	Description         string `json:"DESCRIPTION"`
	AddressPostalCode   string `json:"ADDRESSPOSTALCODE"`
	AddressStreetName   string `json:"ADDRESSSTREETNAME"`
	AddressBlockHouse   string `json:"ADDRESSBLOCKHOUSENUMBER"`
	AddressBuildingName string `json:"ADDRESSBUILDINGNAME"`
	AddressType         string `json:"ADDRESSTYPE"`
	AddressUnitNumber   string `json:"ADDRESSUNITNUMBER"`
	AddressFloorNumber  string `json:"ADDRESSFLOORNUMBER"`
	HyperLink           string `json:"HYPERLINK"`
	IconName            string `json:"ICON_NAME"`
	LatLng              string `json:"LatLng"`
}

// RetrieveTheme calls OneMap's authenticated Themes API for a given
// queryName (e.g. "hawkercentre", "moe_schools_list", "kindergartens").
// The first element in SrchResults is a header containing FeatCount /
// Theme_Name — we skip it and return only the feature rows.
func (o *OneMap) RetrieveTheme(ctx context.Context, queryName string) ([]OneMapThemeResult, error) {
	tok, err := o.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	u := "https://www.onemap.gov.sg/api/public/themesvc/retrieveTheme?queryName=" + url.QueryEscape(queryName)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("onemap theme %s %d: %s", queryName, resp.StatusCode, string(body))
	}
	var r struct {
		SrchResults []json.RawMessage `json:"SrchResults"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]OneMapThemeResult, 0, len(r.SrchResults))
	for i, raw := range r.SrchResults {
		if i == 0 {
			continue
		}
		var it OneMapThemeResult
		if err := json.Unmarshal(raw, &it); err != nil {
			continue
		}
		out = append(out, it)
	}
	slog.Info("onemap theme",
		"queryName", queryName,
		"status", resp.StatusCode,
		"results", len(out),
		"bytes", len(body),
		"duration_ms", time.Since(started).Milliseconds())
	return out, nil
}
