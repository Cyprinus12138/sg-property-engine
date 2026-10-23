package ingest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/sources"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

type POIIngestor struct {
	OneMap   *sources.OneMap
	LTA      *sources.LTA
	DG       *sources.DataGov
	Store    *store.Store
	Resolver *GeoResolver
	Themes   []string
}

// POI type identifiers used in the `type` column of the pois table and in
// DATAENGINE_POI_THEMES. OneMap-backed themes are mapped to their queryName
// via themeQueryMap below; LTA-backed types come from embedded GeoJSON /
// DataMall and have no OneMap theme.
const (
	POITypeKindergarten = "kindergartens"
	POITypeHawker       = "hawker"
	POITypePark         = "park"
	POITypeHospital     = "hospital"
	POITypeMRTExit      = "mrt_exit"
	POITypeLRTExit      = "lrt_exit"
	POITypeBusStop      = "bus_stop"
	POITypeSchool       = "school"
)

// data.gov.sg "General Information of Schools" dataset. Contains name,
// level, address and postal code but no coordinates — we resolve them via
// GeoResolver during ingest.
const schoolsResourceID = "d_688b934f82c1059ed0a6993d2a829089"

// data.gov.sg "Train Station Exit" dataset (LTA GeoJSON). Updated by LTA
// periodically; previously shipped as an embedded snapshot in assets/.
const mrtExitsDatasetID = "d_b39d3a0871985372d7e1637193335da5"

// data.gov.sg "Train Station Chinese Names" — 184-row CSV mapping
// station codes ("EW15") to canonical station names ("Tanjong Pagar")
// + line names ("East West Line"). LTA's own JSON API has no
// TrainStationCodes endpoint (verified 404), so this is the source of
// truth. As of the last refresh it's missing TEL stations; those will
// flow through without codes/lines (FE-side fallback handles it).
const mrtStationCodesDatasetID = "d_d312a5b127e1ae74299b8ae664cedd4e"

// schoolGeocodeThrottle paces the per-school OneMap/Google lookups on
// first ingest (subsequent runs hit geo_cache). ~500 schools at 150ms is
// ~75s — well below the scheduler timeout.
const schoolGeocodeThrottle = 150 * time.Millisecond

// OneMap theme queryNames. Call OneMap's getAllThemesInfo endpoint to
// discover additional queryNames — any unmapped theme is passed through
// as-is, so `DATAENGINE_POI_THEMES=kindergartens,libraries` works directly.
const (
	oneMapThemeKindergartens = "kindergartens"
	oneMapThemeHawker        = "ssot_hawkercentres"
	oneMapThemePark          = "nationalparks"
	oneMapThemeHospital      = "moh_hospitals"
)

// themeQueryMap maps the friendly POI type names used in DATAENGINE_POI_THEMES
// to OneMap theme queryNames.
var themeQueryMap = map[string]string{
	POITypeKindergarten: oneMapThemeKindergartens,
	POITypeHawker:       oneMapThemeHawker,
	POITypePark:         oneMapThemePark,
	POITypeHospital:     oneMapThemeHospital,
}

// Source identifiers written to the pois.source column.
const (
	sourcePrefixOneMapTheme = "onemap_theme:"
	mrtExitsSource          = "lta_geojson:mrt_exits"
	busStopsSource          = "lta_datamall:bus_stops"
	schoolsSource           = "datagov:schools"
)

// lrtStationMarker is the substring LTA uses in STATION_NA to distinguish
// LRT stations from MRT stations in the Train Station Exits GeoJSON.
const lrtStationMarker = "LRT"

// supplementalStationCodes patches gaps in the data.gov.sg CSV. The
// CSV (`Train Station Chinese Names`, dataset d_d312a5b127e1...) hasn't
// been refreshed for TEL (Thomson-East Coast Line, opened 2020-2024)
// or a handful of NSL/DTL extensions. Keeping these inline rather than
// in a separate asset because (a) the list is small and (b) LTA's
// numbering is stable — when a new station opens the addition is one
// line here.
//
// Key is the canonical station name (Title Case as it appears in the
// CSV column convention). Value is one or more station codes
// (interchanges add their codes from elsewhere via the regular CSV
// path — interchange entries here only need to list the TEL/new-line
// code). Future-station codes that haven't opened yet are deliberately
// excluded; add them as service dates land.
var supplementalStationCodes = map[string][]string{
	// Thomson-East Coast Line — TEL1 → TEL4 operational stages.
	"Woodlands North":    {"TE1"},
	"Woodlands":          {"TE2"}, // CSV may list as NS9; both codes are valid here
	"Woodlands South":    {"TE3"},
	"Springleaf":         {"TE4"},
	"Lentor":             {"TE5"},
	"Mayflower":          {"TE6"},
	"Bright Hill":        {"TE7"},
	"Upper Thomson":      {"TE8"},
	"Caldecott":          {"TE9"},  // CSV has Caldecott with CC17; TE9 is the TEL addition
	"Stevens":            {"TE11"}, // CSV has Stevens with DT10
	"Napier":             {"TE12"},
	"Orchard Boulevard":  {"TE13"},
	"Orchard":            {"TE14"}, // CSV has Orchard with NS22
	"Great World":        {"TE15"},
	"Havelock":           {"TE16"},
	"Outram Park":        {"TE17"}, // CSV has EW16+NE3
	"Maxwell":            {"TE18"},
	"Shenton Way":        {"TE19"},
	"Marina Bay":         {"TE20"}, // CSV has NS27+CE2
	"Marina South":       {"TE21"},
	"Gardens by the Bay": {"TE22"},
	"Tanjong Rhu":        {"TE23"},
	"Katong Park":        {"TE24"},
	"Tanjong Katong":     {"TE25"},
	"Marine Parade":      {"TE26"},
	"Marine Terrace":     {"TE27"},
	"Siglap":             {"TE28"},
	"Bayshore":           {"TE29"},
	// NSL extension (post-CSV vintage).
	"Canberra": {"NS12"},
	// DTL extension (Hume opened 2025).
	"Hume": {"DT4"},
}

// stationCodePattern detects when a station-name field actually carries
// a station code (the upstream LTA GeoJSON has a known data-quality
// issue where STATION_NA carries the code for a handful of stations,
// e.g. CC9 / DT18 / DT4). Two letters + 1-3 digits, optionally with a
// trailing letter for branches. We use this to recover those rows by
// looking up the real station name via the inverted CSV index.
var stationCodePattern = regexp.MustCompile(`^[A-Z]{2}\d{1,3}[A-Z]?$`)

// lineFromCodePrefix maps an LTA station-code prefix (the first two letters
// of e.g. "EW15" or "TE17") to the canonical line abbreviation used in
// nearby_mrt.lines and in user-facing chip copy. Prefixes are stable; new
// lines (JRL, CRL) are pre-listed here so future stations enrich cleanly
// the moment LTA Datamall starts returning their codes.
var lineFromCodePrefix = map[string]string{
	"NS": "NSL",                // North-South Line
	"EW": "EWL",                // East-West Line
	"CG": "EWL",                // Changi Airport branch
	"NE": "NEL",                // North-East Line
	"CC": "CCL",                // Circle Line
	"CE": "CCL",                // Circle Line extension to Marina Bay
	"DT": "DTL",                // Downtown Line
	"TE": "TEL",                // Thomson-East Coast Line
	"BP": "BPL",                // Bukit Panjang LRT
	"SE": "SLRT", "SW": "SLRT", // Sengkang LRT (East / West loops)
	"PE": "PLRT", "PW": "PLRT", // Punggol LRT (East / West loops)
	"JE": "JRL", "JS": "JRL", "JW": "JRL", // Jurong Region Line (future)
	"CR": "CRL", "CP": "CRL", // Cross Island Line + Punggol branch (future)
}

func (p *POIIngestor) Run(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error) {
	// Three fixed steps (MRT exits, bus stops, schools) plus one per theme.
	// Scale progress across the entire run so the operator sees monotonic
	// movement.
	totalSteps := len(p.Themes) + 3
	step := 0
	advance := func(stage string) {
		report.Report(stage, float64(step)/float64(totalSteps)*100)
		step++
	}

	totalIn, totalUp := 0, 0
	for _, theme := range p.Themes {
		advance("theme " + theme)
		in, up, err := p.runTheme(ctx, theme)
		if errors.Is(err, store.ErrEmptyUpstream) {
			slog.Warn("poi theme skipped", "theme", theme, "reason", "upstream_empty")
			continue
		}
		if err != nil {
			slog.Warn("poi theme failed", "theme", theme, "err", err)
			continue
		}
		totalIn += in
		totalUp += up
	}
	advance("mrt exits")
	in, up, err := p.runMRTExits(ctx)
	if errors.Is(err, store.ErrEmptyUpstream) {
		slog.Warn("poi mrt exits skipped", "reason", "upstream_empty")
	} else if err != nil {
		slog.Warn("poi mrt exits failed", "err", err)
	} else {
		totalIn += in
		totalUp += up
	}
	advance("bus stops")
	if p.LTA != nil && p.LTA.HasKey() {
		in, up, err = p.runBusStops(ctx)
		if errors.Is(err, store.ErrEmptyUpstream) {
			slog.Warn("poi bus stops skipped", "reason", "upstream_empty")
		} else if err != nil {
			slog.Warn("poi bus stops failed", "err", err)
		} else {
			totalIn += in
			totalUp += up
		}
	} else {
		slog.Info("poi bus stops skipped", "reason", "LTA_ACCOUNT_KEY not set")
	}
	advance("schools")
	in, up, err = p.runSchools(ctx)
	if errors.Is(err, store.ErrEmptyUpstream) {
		slog.Warn("poi schools skipped", "reason", "upstream_empty")
	} else if err != nil {
		slog.Warn("poi schools failed", "err", err)
	} else {
		totalIn += in
		totalUp += up
	}
	return totalIn, totalUp, nil
}

func (p *POIIngestor) runTheme(ctx context.Context, theme string) (int, int, error) {
	queryName, ok := themeQueryMap[theme]
	if !ok {
		queryName = theme
	}
	source := sourcePrefixOneMapTheme + queryName
	results, err := p.OneMap.RetrieveTheme(ctx, queryName)
	if err != nil {
		return 0, 0, err
	}
	rows := make([]store.POIRow, 0, len(results))
	for _, r := range results {
		lat, lng, ok := parseLatLng(r.LatLng)
		if !ok || r.Name == "" {
			continue
		}
		rows = append(rows, store.POIRow{
			Type:    theme,
			Name:    r.Name,
			Subtype: r.AddressType,
			Lat:     lat,
			Lng:     lng,
			Attrs: map[string]any{
				"description": r.Description,
				"postal":      r.AddressPostalCode,
				"street":      r.AddressStreetName,
				"block":       r.AddressBlockHouse,
				"building":    r.AddressBuildingName,
				"hyperlink":   r.HyperLink,
				"queryName":   queryName,
			},
			Source: source,
		})
	}
	slog.Info("poi theme fetched", "theme", theme, "queryName", queryName, "rows", len(rows))
	upstream, changed, pruned, err := p.Store.SyncPOIsBySource(ctx, source, rows)
	slog.Info("poi theme synced", "theme", theme, "upstream", upstream, "changed", changed, "pruned", pruned)
	return upstream, changed + pruned, err
}

// runMRTExits downloads the LTA Train Station Exit GeoJSON from data.gov.sg
// and writes each exit as a POI row. When LTA Datamall is configured we also
// fetch TrainStationCodes and attach the station code(s) + line membership
// to each exit's attrs — this is what lets nearby_mrt surface interchange
// visibility ("Outram Park · EWL / NEL / TEL"). Falls back gracefully to
// the old shape (station name + exit letter only) when the LTA key is
// absent or the Datamall call fails.
func (p *POIIngestor) runMRTExits(ctx context.Context) (int, int, error) {
	if p.DG == nil {
		return 0, 0, fmt.Errorf("mrt exits: DataGov client not configured")
	}
	body, err := p.DG.DownloadDataset(ctx, mrtExitsDatasetID)
	if err != nil {
		return 0, 0, fmt.Errorf("mrt exits download: %w", err)
	}
	defer body.Close()

	var fc struct {
		Features []struct {
			Geometry struct {
				Coordinates [2]float64 `json:"coordinates"`
			} `json:"geometry"`
			Properties struct {
				StationName string `json:"STATION_NA"`
				ExitCode    string `json:"EXIT_CODE"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.NewDecoder(body).Decode(&fc); err != nil {
		return 0, 0, fmt.Errorf("decode mrt exits geojson: %w", err)
	}

	// Build the station-code lookup once, before the exits loop. Best-effort:
	// any failure leaves the index empty and rows enrich without
	// codes/lines (still strictly better than the pre-LTA shape — no
	// regression for properties relying on existing data).
	idx := p.buildStationCodeLookup(ctx)
	if len(idx.byName) > 0 {
		slog.Info("mrt station code lookup ready",
			"stations", len(idx.byName), "codes", len(idx.byCode))
	}

	rows := make([]store.POIRow, 0, len(fc.Features))
	for _, f := range fc.Features {
		station := strings.TrimSpace(f.Properties.StationName)
		exit := strings.TrimSpace(f.Properties.ExitCode)
		if station == "" {
			continue
		}
		lng, lat := f.Geometry.Coordinates[0], f.Geometry.Coordinates[1]
		if lat == 0 || lng == 0 {
			continue
		}
		// Code-as-name recovery: the upstream GeoJSON has a known
		// data-quality issue where STATION_NA carries the station code
		// (e.g. "CC9", "DT18") instead of the station name. When that
		// happens, look the canonical name up via the reverse index
		// and overwrite the station field so downstream consumers see
		// "Paya Lebar" not "CC9".
		var directCodes []string
		if stationCodePattern.MatchString(station) {
			code := strings.ToUpper(station)
			if realName, ok := idx.byCode[code]; ok && realName != "" {
				station = realName
			}
			directCodes = []string{code}
		}
		poiType := POITypeMRTExit
		if strings.Contains(strings.ToUpper(station), lrtStationMarker) {
			poiType = POITypeLRTExit
		}
		name := station
		if exit != "" {
			name = station + " " + exit
		}
		attrs := map[string]any{
			"station": station,
			"exit":    exit,
		}
		// Attach codes/lines. Three paths, in order of preference:
		//   1. Direct (the upstream string itself was a code) — use as-is.
		//   2. CSV / supplemental lookup by normalised station name.
		//   3. None found — leaves attrs without codes/lines, FE hides
		//      the badge and shows just the station name + walk time.
		if len(directCodes) > 0 {
			attrs["codes"] = directCodes
			lines := []string{}
			for _, c := range directCodes {
				if line := lineForCode(c); line != "" && !containsString(lines, line) {
					lines = append(lines, line)
				}
			}
			attrs["lines"] = lines
		} else if cl, ok := idx.byName[normalizeStationName(station)]; ok && len(cl.codes) > 0 {
			attrs["codes"] = cl.codes
			attrs["lines"] = cl.lines
		}
		rows = append(rows, store.POIRow{
			Type:    poiType,
			Name:    name,
			Subtype: exit,
			Lat:     lat,
			Lng:     lng,
			Attrs:   attrs,
			Source:  mrtExitsSource,
		})
	}
	slog.Info("poi mrt exits fetched", "rows", len(rows))
	upstream, changed, pruned, err := p.Store.SyncPOIsBySource(ctx, mrtExitsSource, rows)
	slog.Info("poi mrt exits synced", "upstream", upstream, "changed", changed, "pruned", pruned)
	return upstream, changed + pruned, err
}

// stationCodes is the lookup-table value for buildStationCodeLookup.
// codes is the deduped slice of station codes for this station (3 entries
// for an interchange like Outram Park, 1 for a regular station). lines
// is the deduped slice of canonical line abbreviations derived from each
// code's prefix.
type stationCodes struct {
	codes []string
	lines []string
}

// stationCodeIndex bundles the forward (name→{codes, lines}) lookup
// with a reverse (code→canonical-name) lookup used to recover the
// upstream GeoJSON's code-as-name rows.
type stationCodeIndex struct {
	byName map[string]stationCodes // normalised name → codes/lines
	byCode map[string]string       // upstream station code → canonical Title-Case name
}

// buildStationCodeLookup downloads the data.gov.sg "Train Station
// Chinese Names" CSV and builds a normalised-name → {codes, lines}
// lookup. Falls through to supplementalStationCodes for stations not
// in the (outdated) CSV (TEL, post-2018 extensions). Returns an empty
// index on any failure so callers can use `_, ok := m[key]` safely.
//
// Interchanges appear as multiple rows in the CSV (e.g. Outram Park =
// EW16 + NE3 + TE17). We group by station name and collect; line
// abbreviation derivation uses lineFromCodePrefix.
//
// CSV header: stn_code, mrt_station_english, mrt_station_chinese,
//
//	mrt_line_english, mrt_line_chinese
func (p *POIIngestor) buildStationCodeLookup(ctx context.Context) stationCodeIndex {
	idx := stationCodeIndex{
		byName: map[string]stationCodes{},
		byCode: map[string]string{},
	}
	byName := make(map[string]*stationCodes, 128)

	// Populate from the data.gov.sg CSV when reachable. Any failure here
	// is best-effort: we fall through to the supplemental merge below so
	// the hardcoded fallback still applies. Critically, an empty index
	// is NOT acceptable as a silent outcome — when the CSV is the only
	// source of ~160 stations' codes, a dropped download wipes every
	// nearby_mrt line badge. data.gov.sg rate-limits the download
	// endpoint hard during the nightly batch (429), so this used to fail
	// every run; callDownloadEndpoint now retries 429, but keep the log
	// loud so a regression is visible rather than silently badge-less.
	p.fillStationCodesFromCSV(ctx, idx.byCode, byName)

	// Merge supplemental entries — stations missing from the CSV
	// (TEL, post-2018 extensions). The CSV-derived entry wins on key
	// collision so any future CSV refresh implicitly supersedes the
	// hardcoded list.
	for displayName, codes := range supplementalStationCodes {
		key := normalizeStationName(displayName)
		entry, ok := byName[key]
		if !ok {
			entry = &stationCodes{}
			byName[key] = entry
		}
		for _, code := range codes {
			code = strings.TrimSpace(code)
			if code == "" {
				continue
			}
			if !containsString(entry.codes, code) {
				entry.codes = append(entry.codes, code)
			}
			if line := lineForCode(code); line != "" && !containsString(entry.lines, line) {
				entry.lines = append(entry.lines, line)
			}
			if _, ok := idx.byCode[strings.ToUpper(code)]; !ok {
				idx.byCode[strings.ToUpper(code)] = displayName
			}
		}
	}
	for k, v := range byName {
		idx.byName[k] = *v
	}
	return idx
}

// fillStationCodesFromCSV downloads the data.gov.sg "Train Station
// Chinese Names" CSV and merges its stn_code / mrt_station_english pairs
// into byName (name→{codes,lines}) and byCode (code→canonical name).
// Best-effort: every failure path logs and returns so the caller can
// still apply the supplemental fallback. A failure means the ~160 CSV
// stations lose their badges, so it's a Warn (loud), not silent.
func (p *POIIngestor) fillStationCodesFromCSV(ctx context.Context, byCode map[string]string, byName map[string]*stationCodes) {
	if p.DG == nil {
		slog.Info("mrt station code lookup skipped", "reason", "DataGov client not configured")
		return
	}
	body, err := p.DG.DownloadDataset(ctx, mrtStationCodesDatasetID)
	if err != nil {
		slog.Warn("mrt station code lookup failed (enriching from supplemental list only)", "err", err)
		return
	}
	defer body.Close()
	reader := csv.NewReader(body)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		slog.Warn("mrt station codes csv header read failed", "err", err)
		return
	}
	codeIdx, nameIdx := findCSVColumns(header, "stn_code", "mrt_station_english")
	if codeIdx < 0 || nameIdx < 0 {
		slog.Warn("mrt station codes csv missing expected columns", "header", header)
		return
	}
	loaded := 0
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("mrt station codes csv row read failed", "err", err)
			continue
		}
		if codeIdx >= len(row) || nameIdx >= len(row) {
			continue
		}
		code := strings.TrimSpace(row[codeIdx])
		rawName := strings.TrimSpace(row[nameIdx])
		key := normalizeStationName(rawName)
		if code == "" || key == "" {
			continue
		}
		entry, ok := byName[key]
		if !ok {
			entry = &stationCodes{}
			byName[key] = entry
		}
		if !containsString(entry.codes, code) {
			entry.codes = append(entry.codes, code)
		}
		if line := lineForCode(code); line != "" && !containsString(entry.lines, line) {
			entry.lines = append(entry.lines, line)
		}
		// Reverse index: code → canonical name. Used when upstream
		// emits a code as the station name (data-quality issue in
		// data.gov.sg's MRT Station Exit GeoJSON for ~3 stations).
		byCode[strings.ToUpper(code)] = rawName
		loaded++
	}
	if loaded == 0 {
		slog.Warn("mrt station codes csv yielded no rows")
		return
	}
	slog.Info("mrt station codes csv loaded", "rows", loaded, "stations", len(byName))
}

// findCSVColumns returns the column indices for `code` and `name` in a
// CSV header row. Case-insensitive match on column name; -1 when a
// column is missing.
func findCSVColumns(header []string, codeCol, nameCol string) (int, int) {
	codeIdx, nameIdx := -1, -1
	for i, h := range header {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case strings.ToLower(codeCol):
			codeIdx = i
		case strings.ToLower(nameCol):
			nameIdx = i
		}
	}
	return codeIdx, nameIdx
}

// normalizeStationName canonicalises a station name from either the
// data.gov.sg GeoJSON ("TANJONG PAGAR MRT STATION") or LTA Datamall
// ("Tanjong Pagar") into the same key. Strategy: uppercase, trim,
// strip " MRT STATION" / " LRT STATION" suffixes, collapse internal
// whitespace.
func normalizeStationName(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, " MRT STATION")
	n = strings.TrimSuffix(n, " LRT STATION")
	// Some upstreams pluralise oddly or include "(EXIT X)" — strip parens.
	if i := strings.Index(n, " ("); i > 0 {
		n = n[:i]
	}
	return strings.Join(strings.Fields(n), " ")
}

// lineForCode looks up the canonical line abbreviation for a station
// code. The two-letter prefix is the line; everything after is the
// numeric position on that line (with branches sometimes appending a
// letter). Returns "" for unknown prefixes — those just don't get a
// line attached, which the nearby_mrt renderer handles.
func lineForCode(code string) string {
	if len(code) < 2 {
		return ""
	}
	return lineFromCodePrefix[strings.ToUpper(code[:2])]
}

// containsString is a tiny dedupe helper; pulling in slices.Contains
// would require a Go 1.21+ check across files that already use 1.21
// features, but keeping this local makes the dependency obvious and
// matches the rest of this package's style.
func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// runBusStops pulls the full bus stop directory from LTA DataMall and writes
// each stop as a `bus_stop` POI.
func (p *POIIngestor) runBusStops(ctx context.Context) (int, int, error) {
	stops, err := p.LTA.BusStops(ctx)
	if err != nil {
		return 0, 0, err
	}
	rows := make([]store.POIRow, 0, len(stops))
	for _, s := range stops {
		if s.Latitude == 0 || s.Longitude == 0 || s.BusStopCode == "" {
			continue
		}
		name := s.Description
		if name == "" {
			name = s.BusStopCode
		}
		rows = append(rows, store.POIRow{
			Type:    POITypeBusStop,
			Name:    name,
			Subtype: s.BusStopCode,
			Lat:     s.Latitude,
			Lng:     s.Longitude,
			Attrs: map[string]any{
				"code": s.BusStopCode,
				"road": s.RoadName,
			},
			Source: busStopsSource,
		})
	}
	slog.Info("poi bus stops fetched", "rows", len(rows))
	upstream, changed, pruned, err := p.Store.SyncPOIsBySource(ctx, busStopsSource, rows)
	slog.Info("poi bus stops synced", "upstream", upstream, "changed", changed, "pruned", pruned)
	return upstream, changed + pruned, err
}

// runSchools downloads the full schools dataset CSV from data.gov.sg, then
// resolves each school's coordinates via GeoResolver (OneMap → Google Maps
// fallback, with geo_cache). Schools have no coordinates in the source
// data, so this is the only POI path that calls the resolver directly.
//
// Mirrors the HDB backfill's CSV download shape (see ingest/hdb.go) — the
// initiate-download endpoint is much friendlier than paging datastore_search.
func (p *POIIngestor) runSchools(ctx context.Context) (int, int, error) {
	if p.DG == nil {
		return 0, 0, fmt.Errorf("schools: DataGov client not configured")
	}
	if p.Resolver == nil {
		return 0, 0, fmt.Errorf("schools: GeoResolver not configured")
	}
	body, err := p.DG.DownloadDatasetCSV(ctx, schoolsResourceID)
	if err != nil {
		return 0, 0, fmt.Errorf("schools download: %w", err)
	}
	defer body.Close()

	r := csv.NewReader(body)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return 0, 0, fmt.Errorf("schools csv header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}
	get := func(rec []string, col string) string {
		i, ok := idx[col]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []store.POIRow
	resolved, skipped := 0, 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("schools csv read: %w", err)
		}
		name := get(rec, "school_name")
		if name == "" {
			continue
		}
		levelRaw := strings.ToUpper(get(rec, "mainlevel_code"))
		level := "OTHER"
		switch {
		case strings.Contains(levelRaw, "PRIMARY"):
			level = "PRIMARY"
		case strings.Contains(levelRaw, "SECONDARY"):
			level = "SECONDARY"
		case strings.Contains(levelRaw, "MIXED"):
			level = "MIXED"
		}
		postal := get(rec, "postal_code")
		address := get(rec, "address")

		// Prefer postal code as the resolver query — it's unique and
		// hits OneMap's address index cleanly. Fall back to the school
		// name if no postal is published.
		query := postal
		if query == "" {
			query = name
		}
		lat, lng, resolvedPostal := p.Resolver.Resolve(ctx, query)
		time.Sleep(schoolGeocodeThrottle)
		if lat == 0 && lng == 0 {
			skipped++
			continue
		}
		resolved++
		if resolvedPostal == "" {
			resolvedPostal = postal
		}
		rows = append(rows, store.POIRow{
			Type:    POITypeSchool,
			Name:    name,
			Subtype: level,
			Lat:     lat,
			Lng:     lng,
			Attrs: map[string]any{
				"level":       level,
				"level_raw":   levelRaw,
				"postal":      resolvedPostal,
				"address":     address,
				"resource_id": schoolsResourceID,
			},
			Source: schoolsSource,
		})
	}
	slog.Info("poi schools fetched", "rows", len(rows), "resolved", resolved, "unresolved", skipped)
	upstream, changed, pruned, err := p.Store.SyncPOIsBySource(ctx, schoolsSource, rows)
	slog.Info("poi schools synced", "upstream", upstream, "changed", changed, "pruned", pruned)
	return upstream, changed + pruned, err
}

// parseLatLng parses OneMap's "lat,lng" string field from theme results.
func parseLatLng(s string) (float64, float64, bool) {
	if s == "" {
		return 0, 0, false
	}
	i := strings.IndexByte(s, ',')
	if i < 0 {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
	lng, err2 := strconv.ParseFloat(strings.TrimSpace(s[i+1:]), 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lat, lng, true
}
