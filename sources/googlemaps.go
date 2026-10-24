package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// GoogleMaps wraps the Places API (New) Text Search endpoint.
// https://developers.google.com/maps/documentation/places/web-service/text-search
// Used as a fallback when OneMap search misses (e.g. uncommon building
// names, newly built blocks not yet in OneMap's index).
type GoogleMaps struct {
	http   *http.Client
	apiKey string
}

type GoogleMapsCreds struct {
	APIKey string
}

func NewGoogleMaps(creds GoogleMapsCreds) *GoogleMaps {
	return &GoogleMaps{
		http:   &http.Client{Timeout: 20 * time.Second},
		apiKey: creds.APIKey,
	}
}

func (g *GoogleMaps) HasAuth() bool { return g.apiKey != "" }

// SetHTTPClient replaces the client used for every call (proxy, custom
// timeouts, a test transport).
func (g *GoogleMaps) SetHTTPClient(c *http.Client) { g.http = c }

type GoogleMapsPlace struct {
	Name             string
	DisplayName      string
	FormattedAddress string
	PostalCode       string
	Latitude         float64
	Longitude        float64
}

type gmPlaceRaw struct {
	Name             string `json:"name"`
	FormattedAddress string `json:"formattedAddress"`
	DisplayName      struct {
		Text string `json:"text"`
	} `json:"displayName"`
	Location struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
	PostalAddress struct {
		PostalCode string `json:"postalCode"`
	} `json:"postalAddress"`
}

// SearchText calls Places API (New) searchText and returns the parsed
// results. Returns an error if the API key is not configured or the call
// fails — callers should treat that as "no fallback available" and move
// on.
func (g *GoogleMaps) SearchText(ctx context.Context, query string) ([]GoogleMapsPlace, error) {
	if g.apiKey == "" {
		return nil, fmt.Errorf("google maps api key not configured (set GOOGLE_MAPS_API_KEY)")
	}
	payload, _ := json.Marshal(map[string]string{"textQuery": query})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://places.googleapis.com/v1/places:searchText", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", g.apiKey)
	req.Header.Set("X-Goog-FieldMask", "places.name,places.location,places.displayName,places.formattedAddress,places.postalAddress")
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := g.http.Do(req)
	if err != nil {
		slog.Warn("googlemaps request failed", "query", query, "err", err, "duration_ms", time.Since(started).Milliseconds())
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		slog.Warn("googlemaps non-200", "query", query, "status", resp.StatusCode, "duration_ms", time.Since(started).Milliseconds(), "body", string(body))
		return nil, fmt.Errorf("googlemaps %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Places []gmPlaceRaw `json:"places"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]GoogleMapsPlace, 0, len(r.Places))
	for _, p := range r.Places {
		out = append(out, GoogleMapsPlace{
			Name:             p.Name,
			DisplayName:      p.DisplayName.Text,
			FormattedAddress: p.FormattedAddress,
			PostalCode:       p.PostalAddress.PostalCode,
			Latitude:         p.Location.Latitude,
			Longitude:        p.Location.Longitude,
		})
	}
	slog.Info("googlemaps searchText",
		"query", query,
		"status", resp.StatusCode,
		"results", len(out),
		"bytes", len(body),
		"duration_ms", time.Since(started).Milliseconds())
	return out, nil
}

// DirectionsTransit summarises a single transit route between two points.
// We only carry the headline figures the model needs to answer "how long
// does this commute take" — full step-by-step directions would balloon
// the LLM context for no advisory benefit.
type DirectionsTransit struct {
	DurationMinutes int      `json:"duration_minutes"`
	DurationText    string   `json:"duration_text"`
	DistanceMeters  int      `json:"distance_meters"`
	StartAddress    string   `json:"start_address"`
	EndAddress      string   `json:"end_address"`
	Summary         string   `json:"summary"`
	TransitVehicles []string `json:"transit_vehicles"` // e.g. ["BUS","SUBWAY"]
}

type gmDirectionsResp struct {
	Status string `json:"status"`
	Routes []struct {
		Summary string `json:"summary"`
		Legs    []struct {
			Duration struct {
				Value int    `json:"value"`
				Text  string `json:"text"`
			} `json:"duration"`
			Distance struct {
				Value int    `json:"value"`
				Text  string `json:"text"`
			} `json:"distance"`
			StartAddress string `json:"start_address"`
			EndAddress   string `json:"end_address"`
			Steps        []struct {
				TravelMode     string `json:"travel_mode"`
				TransitDetails *struct {
					Line struct {
						Vehicle struct {
							Type string `json:"type"`
						} `json:"vehicle"`
					} `json:"line"`
				} `json:"transit_details,omitempty"`
			} `json:"steps"`
		} `json:"legs"`
	} `json:"routes"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// DirectionsTransit calls the Google Directions API in transit mode and
// returns the headline duration/distance for the best route. preferBus
// hints the API to prefer bus-only routing when true (model asks for
// "bus commute"); set false for general transit (any mix of bus/MRT).
//
// departureTime is a Unix timestamp, defaulting to "now" — pass a
// future timestamp to model peak-hour commute. Singapore peak is 0830
// weekdays; we leave that choice to callers.
func (g *GoogleMaps) DirectionsTransit(ctx context.Context, fromLat, fromLng, toLat, toLng float64, preferBus bool, departureTime int64) (*DirectionsTransit, error) {
	if g.apiKey == "" {
		return nil, fmt.Errorf("google maps api key not configured")
	}
	if departureTime <= 0 {
		departureTime = time.Now().Unix()
	}
	url := fmt.Sprintf(
		"https://maps.googleapis.com/maps/api/directions/json?origin=%.6f,%.6f&destination=%.6f,%.6f&mode=transit&departure_time=%d&key=%s",
		fromLat, fromLng, toLat, toLng, departureTime, g.apiKey,
	)
	if preferBus {
		// transit_mode=bus tells Directions to bias toward bus routes.
		// It's a hint, not a hard filter — Directions will still mix in
		// MRT if a pure-bus route is dramatically slower.
		url += "&transit_mode=bus"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "sg-property-engine/0.1 (+https://github.com/Cyprinus12138/sg-property-engine)")
	started := time.Now()
	resp, err := g.http.Do(req)
	if err != nil {
		slog.Warn("googlemaps directions failed", "err", err, "duration_ms", time.Since(started).Milliseconds())
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("googlemaps directions %d: %s", resp.StatusCode, string(body))
	}
	var r gmDirectionsResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Status != "OK" {
		// ZERO_RESULTS / NOT_FOUND / etc — surface to the caller so the
		// tool can return a soft failure rather than a hard error.
		return nil, fmt.Errorf("googlemaps directions status=%s msg=%s", r.Status, r.ErrorMessage)
	}
	if len(r.Routes) == 0 || len(r.Routes[0].Legs) == 0 {
		return nil, fmt.Errorf("googlemaps directions: no routes")
	}
	leg := r.Routes[0].Legs[0]
	vehicleSet := map[string]bool{}
	for _, st := range leg.Steps {
		if st.TravelMode == "TRANSIT" && st.TransitDetails != nil {
			vt := st.TransitDetails.Line.Vehicle.Type
			if vt != "" {
				vehicleSet[vt] = true
			}
		}
	}
	vehicles := make([]string, 0, len(vehicleSet))
	for v := range vehicleSet {
		vehicles = append(vehicles, v)
	}
	dur := leg.Duration.Value / 60
	if dur == 0 && leg.Duration.Value > 0 {
		dur = 1
	}
	out := &DirectionsTransit{
		DurationMinutes: dur,
		DurationText:    leg.Duration.Text,
		DistanceMeters:  leg.Distance.Value,
		StartAddress:    leg.StartAddress,
		EndAddress:      leg.EndAddress,
		Summary:         r.Routes[0].Summary,
		TransitVehicles: vehicles,
	}
	slog.Info("googlemaps directions",
		"from", fmt.Sprintf("%.4f,%.4f", fromLat, fromLng),
		"to", fmt.Sprintf("%.4f,%.4f", toLat, toLng),
		"prefer_bus", preferBus,
		"duration_min", out.DurationMinutes,
		"distance_m", out.DistanceMeters,
		"vehicles", vehicles,
		"duration_ms", time.Since(started).Milliseconds())
	return out, nil
}
