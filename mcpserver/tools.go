package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/sg-property-engine/client"
)

type tools struct{ c *client.Client }

// Page is the result shape of every list tool.
type Page[T any] struct {
	Rows   []T     `json:"rows"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit,omitempty"`
	Offset int     `json:"offset,omitempty"`
	Anchor *Anchor `json:"anchor,omitempty" jsonschema:"the point a near search was centred on"`
}

func page[T any](rows []T, total, limit, offset int, a *Anchor) Page[T] {
	if rows == nil {
		rows = []T{}
	}
	return Page[T]{Rows: rows, Total: total, Limit: limit, Offset: offset, Anchor: a}
}

// List wraps a plain array result; structured tool output must be an object.
type List[T any] struct {
	Rows []T `json:"rows"`
}

func list[T any](rows []T) List[T] {
	if rows == nil {
		rows = []T{}
	}
	return List[T]{Rows: rows}
}

func readOnly(title string) *mcp.ToolAnnotations {
	closed := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &closed}
}

func addTools(s *mcp.Server, t *tools) {
	mcp.AddTool(s, &mcp.Tool{Name: "search_properties", Annotations: readOnly("Search properties"),
		Description: "Find private projects and HDB blocks by name, street or block. Use this to turn a partial or informal name into the exact one other tools take."},
		t.searchProperties)
	mcp.AddTool(s, &mcp.Tool{Name: "get_project", Annotations: readOnly("Get project"),
		Description: "One private project: district, tenure, property types, market segment, postal code, coordinates, transaction count and date range, and the nearest MRT/LRT stations with walking minutes."},
		t.getProject)
	mcp.AddTool(s, &mcp.Tool{Name: "find_projects", Annotations: readOnly("Find projects"),
		Description: "Private projects filtered by name prefix, district, property type and location (near, has_poi)."},
		t.findProjects)
	mcp.AddTool(s, &mcp.Tool{Name: "hdb_resale_transactions", Annotations: readOnly("HDB resale transactions"),
		Description: "HDB resale transactions, newest month first unless sorted otherwise. Filter by town, flat type, block, street, month range, price, floor area, storey range and location."},
		t.hdbTransactions)
	mcp.AddTool(s, &mcp.Tool{Name: "hdb_resale_stats", Annotations: readOnly("HDB resale stats"),
		Description: "Aggregate HDB resale prices grouped by month, town or flat type: median psf, median price, average price or count. Same filters as hdb_resale_transactions."},
		t.hdbStats)
	mcp.AddTool(s, &mcp.Tool{Name: "private_transactions", Annotations: readOnly("Private transactions"),
		Description: "URA private residential sale transactions (caveats lodged, about the last five years), newest first. Filter by project, district, property type, market segment, date, price, psf and location."},
		t.uraTransactions)
	mcp.AddTool(s, &mcp.Tool{Name: "private_rentals", Annotations: readOnly("Private rentals"),
		Description: "URA private residential rental contracts, newest first. Filter by project, district, property type, date and location."},
		t.uraRentals)
	mcp.AddTool(s, &mcp.Tool{Name: "private_rental_trend", Annotations: readOnly("Private rental trend"),
		Description: "Monthly 25th percentile, median and 75th percentile rent for private residential leases, by district, property type and bedroom count."},
		t.uraRentalTrend)
	mcp.AddTool(s, &mcp.Tool{Name: "development_pipeline", Annotations: readOnly("Development pipeline"),
		Description: "Upcoming and under-construction private projects from URA: developer, total units, units sold, expected TOP."},
		t.uraPipeline)
	mcp.AddTool(s, &mcp.Tool{Name: "developer_sales", Annotations: readOnly("Developer sales"),
		Description: "Monthly developer sales snapshots for new launches: units launched and sold to date and in the month, lowest/median/highest price."},
		t.uraDeveloperSales)
	mcp.AddTool(s, &mcp.Tool{Name: "nearest_mrt", Annotations: readOnly("Nearest MRT"),
		Description: "Nearest MRT/LRT stations to a point, project, POI or HDB block, with line codes, distance and walking minutes."},
		t.nearestMRT)
	mcp.AddTool(s, &mcp.Tool{Name: "find_pois", Annotations: readOnly("Find POIs"),
		Description: "Points of interest (MRT/LRT exits, schools, bus stops, parks, hawker centres, hospitals, kindergartens) by type, name prefix and location."},
		t.findPOIs)
	mcp.AddTool(s, &mcp.Tool{Name: "poi_types", Annotations: readOnly("POI types"),
		Description: "The POI types in the database with their counts, for use as poi_type / has_poi.type."},
		t.poiTypes)
	mcp.AddTool(s, &mcp.Tool{Name: "hottest_projects", Annotations: readOnly("Hottest projects"),
		Description: "Private projects ranked by transaction volume over the latest 90 days of URA data, with median price, median psf and psf change versus the prior 90 days."},
		t.hottest)
	mcp.AddTool(s, &mcp.Tool{Name: "district_median", Annotations: readOnly("District median"),
		Description: "Median private sale price and psf in a district over a trailing window, optionally within a floor-area band. sample_count 0 means no data, not a zero price."},
		t.districtMedian)
	mcp.AddTool(s, &mcp.Tool{Name: "cea_salesperson", Annotations: readOnly("CEA salesperson"),
		Description: "Look up a property salesperson in the CEA public register by registration number: name, agency, registration period, whether currently active."},
		t.ceaLookup)
	mcp.AddTool(s, &mcp.Tool{Name: "cea_practice_stats", Annotations: readOnly("CEA practice stats"),
		Description: "A salesperson's last 12 months of recorded transactions from CEA: deal count, monthly activity, towns, representation and transaction types."},
		t.ceaStats)
	mcp.AddTool(s, &mcp.Tool{Name: "data_freshness", Annotations: readOnly("Data freshness"),
		Description: "When each data source was last synced and whether that run succeeded. Use it before concluding that data is missing."},
		t.dataFreshness)
}

// ── search & projects ───────────────────────────────────

type searchIn struct {
	Query string `json:"query" jsonschema:"name, street or block, e.g. sail marina or 123 bishan"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results; default 10, max 50"`
}

func (t *tools) searchProperties(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, List[client.SearchRow], error) {
	if len([]rune(in.Query)) < 2 {
		return nil, List[client.SearchRow]{}, errors.New("query: at least 2 characters")
	}
	rows, err := t.c.SearchProperties(ctx, in.Query, in.Limit)
	return nil, list(rows), err
}

type projectIn struct {
	Name string `json:"name" jsonschema:"project name; case and punctuation do not matter"`
}

func (t *tools) getProject(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, *client.ProjectDetail, error) {
	p, err := t.c.Project(ctx, in.Name)
	if errors.Is(err, client.ErrNotFound) {
		return nil, nil, fmt.Errorf("no project named %q; use search_properties to find the exact name", in.Name)
	}
	if err != nil {
		return nil, nil, err
	}
	if p.NearbyMRT == nil {
		p.NearbyMRT = []client.MRTStation{}
	}
	return nil, p, nil
}

type findProjectsIn struct {
	NamePrefix   string  `json:"name_prefix,omitempty" jsonschema:"project name prefix"`
	District     string  `json:"district,omitempty" jsonschema:"postal district 1-28"`
	PropertyType string  `json:"property_type,omitempty" jsonschema:"e.g. Condominium, Apartment, Executive Condominium, Terrace"`
	Near         *Near   `json:"near,omitempty"`
	HasPOI       *HasPOI `json:"has_poi,omitempty"`
	Sort         string  `json:"sort,omitempty" jsonschema:"distance_asc (needs near); default by name"`
	Limit        int     `json:"limit,omitempty" jsonschema:"page size; default 20, max 100"`
	Offset       int     `json:"offset,omitempty"`
}

func (t *tools) findProjects(ctx context.Context, _ *mcp.CallToolRequest, in findProjectsIn) (*mcp.CallToolResult, Page[client.Project], error) {
	var zero Page[client.Project]
	q, a, err := t.listQuery(ctx, in.Near, in.HasPOI, in.Limit, in.Offset)
	if err != nil {
		return nil, zero, err
	}
	d, err := normDistrict(in.District)
	if err != nil {
		return nil, zero, err
	}
	setIf(q, "name", in.NamePrefix)
	setIf(q, "district", d)
	setIf(q, "property_type", normPropertyType(in.PropertyType))
	if err := setSort(q, in.Sort, a, "distance_asc"); err != nil {
		return nil, zero, err
	}
	p, err := t.c.Projects(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, p.Limit, p.Offset, a), nil
}

// ── HDB ─────────────────────────────────────────────────

type hdbFilter struct {
	Town       string   `json:"town,omitempty" jsonschema:"HDB town, e.g. BISHAN, ANG MO KIO"`
	FlatType   string   `json:"flat_type,omitempty" jsonschema:"e.g. 3 ROOM, 4 ROOM, 5 ROOM, EXECUTIVE"`
	FromMonth  string   `json:"from_month,omitempty" jsonschema:"earliest month, YYYY-MM"`
	ToMonth    string   `json:"to_month,omitempty" jsonschema:"latest month, YYYY-MM"`
	MinPrice   *float64 `json:"min_price,omitempty" jsonschema:"SGD"`
	MaxPrice   *float64 `json:"max_price,omitempty" jsonschema:"SGD"`
	MinAreaSqm *float64 `json:"min_area_sqm,omitempty"`
	MaxAreaSqm *float64 `json:"max_area_sqm,omitempty"`
	Near       *Near    `json:"near,omitempty"`
	HasPOI     *HasPOI  `json:"has_poi,omitempty"`
}

func (f hdbFilter) apply(q url.Values) error {
	from, err := normDate("from_month", f.FromMonth)
	if err != nil {
		return err
	}
	to, err := normDate("to_month", f.ToMonth)
	if err != nil {
		return err
	}
	setIf(q, "town", normUpper(f.Town))
	setIf(q, "flat_type", normFlatType(f.FlatType))
	setIf(q, "min_month", from)
	setIf(q, "max_month", to)
	setNum(q, "min_price", f.MinPrice)
	setNum(q, "max_price", f.MaxPrice)
	setNum(q, "min_area", f.MinAreaSqm)
	setNum(q, "max_area", f.MaxAreaSqm)
	return nil
}

type hdbTxnIn struct {
	hdbFilter
	Block       string `json:"block,omitempty" jsonschema:"block number, e.g. 123 or 301A"`
	Street      string `json:"street,omitempty" jsonschema:"street name prefix, e.g. BISHAN ST 12"`
	StoreyRange string `json:"storey_range,omitempty" jsonschema:"as HDB publishes it, e.g. 07 TO 09"`
	Sort        string `json:"sort,omitempty" jsonschema:"price_desc, price_asc or distance_asc (needs near); default newest month first"`
	Limit       int    `json:"limit,omitempty" jsonschema:"page size; default 20, max 100"`
	Offset      int    `json:"offset,omitempty"`
}

func (t *tools) hdbTransactions(ctx context.Context, _ *mcp.CallToolRequest, in hdbTxnIn) (*mcp.CallToolResult, Page[client.HDBTxn], error) {
	var zero Page[client.HDBTxn]
	q, a, err := t.listQuery(ctx, in.Near, in.HasPOI, in.Limit, in.Offset)
	if err != nil {
		return nil, zero, err
	}
	if err := in.apply(q); err != nil {
		return nil, zero, err
	}
	setIf(q, "block", normUpper(in.Block))
	setIf(q, "street", normUpper(in.Street))
	setIf(q, "storey", normUpper(in.StoreyRange))
	if err := setSort(q, in.Sort, a, "price_desc", "price_asc", "distance_asc"); err != nil {
		return nil, zero, err
	}
	p, err := t.c.HDBTransactions(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, p.Limit, p.Offset, a), nil
}

type hdbStatsIn struct {
	hdbFilter
	GroupBy string `json:"group_by" jsonschema:"month, town or flat_type"`
	Metric  string `json:"metric,omitempty" jsonschema:"median_psf (default), median_price, avg_price or count"`
}

// StatsOut is hdb_resale_stats' result.
type StatsOut struct {
	GroupBy string               `json:"group_by"`
	Metric  string               `json:"metric"`
	Buckets []client.StatsBucket `json:"buckets"`
	Anchor  *Anchor              `json:"anchor,omitempty"`
}

func (t *tools) hdbStats(ctx context.Context, _ *mcp.CallToolRequest, in hdbStatsIn) (*mcp.CallToolResult, StatsOut, error) {
	var zero StatsOut
	if err := oneOf("group_by", in.GroupBy, "month", "town", "flat_type"); err != nil {
		return nil, zero, err
	}
	if in.Metric != "" {
		if err := oneOf("metric", in.Metric, "median_psf", "median_price", "avg_price", "count"); err != nil {
			return nil, zero, err
		}
	}
	a, err := resolveAnchor(ctx, t.c, in.Near)
	if err != nil {
		return nil, zero, err
	}
	q := url.Values{"group_by": {in.GroupBy}}
	setIf(q, "metric", in.Metric)
	if err := addGeo(q, a, in.HasPOI); err != nil {
		return nil, zero, err
	}
	if err := in.apply(q); err != nil {
		return nil, zero, err
	}
	r, err := t.c.HDBStats(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	if r.Buckets == nil {
		r.Buckets = []client.StatsBucket{}
	}
	return nil, StatsOut{GroupBy: r.GroupBy, Metric: r.Metric, Buckets: r.Buckets, Anchor: a}, nil
}

// ── URA ─────────────────────────────────────────────────

type uraTxnIn struct {
	Project       string   `json:"project,omitempty" jsonschema:"exact project name; use search_properties if unsure"`
	District      string   `json:"district,omitempty" jsonschema:"postal district 1-28"`
	PropertyType  string   `json:"property_type,omitempty" jsonschema:"e.g. Condominium, Apartment, Executive Condominium, Terrace"`
	MarketSegment string   `json:"market_segment,omitempty" jsonschema:"CCR, RCR or OCR"`
	FromDate      string   `json:"from_date,omitempty" jsonschema:"earliest contract month, YYYY-MM"`
	ToDate        string   `json:"to_date,omitempty" jsonschema:"latest contract month, YYYY-MM"`
	MinPrice      *float64 `json:"min_price,omitempty" jsonschema:"SGD"`
	MaxPrice      *float64 `json:"max_price,omitempty" jsonschema:"SGD"`
	MinPSF        *float64 `json:"min_psf,omitempty" jsonschema:"SGD per sq ft"`
	MaxPSF        *float64 `json:"max_psf,omitempty" jsonschema:"SGD per sq ft"`
	Near          *Near    `json:"near,omitempty"`
	HasPOI        *HasPOI  `json:"has_poi,omitempty"`
	Sort          string   `json:"sort,omitempty" jsonschema:"distance_asc (needs near); default newest first"`
	Limit         int      `json:"limit,omitempty" jsonschema:"page size; default 20, max 100"`
	Offset        int      `json:"offset,omitempty"`
}

func (t *tools) uraTransactions(ctx context.Context, _ *mcp.CallToolRequest, in uraTxnIn) (*mcp.CallToolResult, Page[client.URATxn], error) {
	var zero Page[client.URATxn]
	q, a, err := t.listQuery(ctx, in.Near, in.HasPOI, in.Limit, in.Offset)
	if err != nil {
		return nil, zero, err
	}
	if err := uraCommon(q, in.Project, in.District, in.PropertyType, in.FromDate, in.ToDate); err != nil {
		return nil, zero, err
	}
	seg, err := normSegment(in.MarketSegment)
	if err != nil {
		return nil, zero, err
	}
	setIf(q, "market_segment", seg)
	setNum(q, "min_price", in.MinPrice)
	setNum(q, "max_price", in.MaxPrice)
	setNum(q, "min_psf", in.MinPSF)
	setNum(q, "max_psf", in.MaxPSF)
	if err := setSort(q, in.Sort, a, "distance_asc"); err != nil {
		return nil, zero, err
	}
	p, err := t.c.URATransactions(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, p.Limit, p.Offset, a), nil
}

type uraRentalIn struct {
	Project      string  `json:"project,omitempty" jsonschema:"exact project name; use search_properties if unsure"`
	District     string  `json:"district,omitempty" jsonschema:"postal district 1-28"`
	PropertyType string  `json:"property_type,omitempty" jsonschema:"e.g. Non-landed Properties, Executive Condominium"`
	FromDate     string  `json:"from_date,omitempty" jsonschema:"earliest lease month, YYYY-MM"`
	ToDate       string  `json:"to_date,omitempty" jsonschema:"latest lease month, YYYY-MM"`
	Near         *Near   `json:"near,omitempty"`
	HasPOI       *HasPOI `json:"has_poi,omitempty"`
	Limit        int     `json:"limit,omitempty" jsonschema:"page size; default 20, max 100"`
	Offset       int     `json:"offset,omitempty"`
}

func (t *tools) uraRentals(ctx context.Context, _ *mcp.CallToolRequest, in uraRentalIn) (*mcp.CallToolResult, Page[client.URARental], error) {
	var zero Page[client.URARental]
	q, a, err := t.listQuery(ctx, in.Near, in.HasPOI, in.Limit, in.Offset)
	if err != nil {
		return nil, zero, err
	}
	if err := uraCommon(q, in.Project, in.District, in.PropertyType, in.FromDate, in.ToDate); err != nil {
		return nil, zero, err
	}
	p, err := t.c.URARentals(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, p.Limit, p.Offset, a), nil
}

type rentalTrendIn struct {
	District     string   `json:"district,omitempty" jsonschema:"postal district 1-28"`
	PropertyType string   `json:"property_type,omitempty" jsonschema:"e.g. Non-landed Properties, Executive Condominium"`
	Bedrooms     *float64 `json:"bedrooms,omitempty" jsonschema:"bedroom count"`
	FromDate     string   `json:"from_date,omitempty" jsonschema:"YYYY-MM"`
	ToDate       string   `json:"to_date,omitempty" jsonschema:"YYYY-MM"`
}

func (t *tools) uraRentalTrend(ctx context.Context, _ *mcp.CallToolRequest, in rentalTrendIn) (*mcp.CallToolResult, *client.RentalTrend, error) {
	q := url.Values{}
	if err := uraCommon(q, "", in.District, in.PropertyType, in.FromDate, in.ToDate); err != nil {
		return nil, nil, err
	}
	setNum(q, "bedroom_num", in.Bedrooms)
	r, err := t.c.URARentalsTrend(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	if r.Months == nil {
		r.Months = []client.RentalTrendMonth{}
	}
	return nil, r, nil
}

type pipelineIn struct {
	District string `json:"district,omitempty" jsonschema:"postal district 1-28"`
	Status   string `json:"status,omitempty" jsonschema:"pipeline status as URA publishes it"`
}

func (t *tools) uraPipeline(ctx context.Context, _ *mcp.CallToolRequest, in pipelineIn) (*mcp.CallToolResult, Page[client.URAPipelineRow], error) {
	var zero Page[client.URAPipelineRow]
	d, err := normDistrict(in.District)
	if err != nil {
		return nil, zero, err
	}
	q := url.Values{}
	setIf(q, "district", d)
	setIf(q, "status", in.Status)
	p, err := t.c.URAPipeline(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, 0, 0, nil), nil
}

type devSalesIn struct {
	Project       string `json:"project,omitempty" jsonschema:"exact project name"`
	District      string `json:"district,omitempty" jsonschema:"postal district 1-28"`
	MarketSegment string `json:"market_segment,omitempty" jsonschema:"CCR, RCR or OCR"`
	Developer     string `json:"developer,omitempty" jsonschema:"developer name as URA publishes it"`
	FromPeriod    string `json:"from_period,omitempty" jsonschema:"YYYY-MM"`
	ToPeriod      string `json:"to_period,omitempty" jsonschema:"YYYY-MM"`
	Limit         int    `json:"limit,omitempty" jsonschema:"page size; default 20, max 100"`
	Offset        int    `json:"offset,omitempty"`
}

func (t *tools) uraDeveloperSales(ctx context.Context, _ *mcp.CallToolRequest, in devSalesIn) (*mcp.CallToolResult, Page[client.URADeveloperSale], error) {
	var zero Page[client.URADeveloperSale]
	q, _, err := t.listQuery(ctx, nil, nil, in.Limit, in.Offset)
	if err != nil {
		return nil, zero, err
	}
	d, err := normDistrict(in.District)
	if err != nil {
		return nil, zero, err
	}
	seg, err := normSegment(in.MarketSegment)
	if err != nil {
		return nil, zero, err
	}
	from, err := normDate("from_period", in.FromPeriod)
	if err != nil {
		return nil, zero, err
	}
	to, err := normDate("to_period", in.ToPeriod)
	if err != nil {
		return nil, zero, err
	}
	setIf(q, "project", normUpper(in.Project))
	setIf(q, "district", d)
	setIf(q, "market_segment", seg)
	setIf(q, "developer", in.Developer)
	setIf(q, "min_period", from)
	setIf(q, "max_period", to)
	p, err := t.c.URADeveloperSales(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, p.Limit, p.Offset, nil), nil
}

// ── geo ─────────────────────────────────────────────────

type nearestIn struct {
	Near    Near `json:"near" jsonschema:"the point to measure from; radius_m is ignored"`
	Limit   int  `json:"limit,omitempty" jsonschema:"stations to return; default 3, max 20"`
	WithinM int  `json:"within_m,omitempty" jsonschema:"also include every station within this many metres; default 1000, max 5000"`
}

// NearestOut is nearest_mrt's result.
type NearestOut struct {
	Anchor   Anchor              `json:"anchor"`
	Stations []client.MRTStation `json:"stations"`
}

func (t *tools) nearestMRT(ctx context.Context, _ *mcp.CallToolRequest, in nearestIn) (*mcp.CallToolResult, NearestOut, error) {
	in.Near.RadiusM = 0
	a, err := resolveAnchor(ctx, t.c, &in.Near)
	if err != nil {
		return nil, NearestOut{}, err
	}
	if a == nil {
		return nil, NearestOut{}, errors.New("near: give lat+lng, project, poi_type(+poi_name), or block+street")
	}
	r, err := t.c.NearestStations(ctx, a.Lat, a.Lng, in.Limit, in.WithinM)
	if err != nil {
		return nil, NearestOut{}, err
	}
	out := NearestOut{Anchor: *a, Stations: r.Stations}
	if out.Stations == nil {
		out.Stations = []client.MRTStation{}
	}
	return nil, out, nil
}

type findPOIsIn struct {
	Type       string `json:"type,omitempty" jsonschema:"POI type, e.g. mrt_exit, school, park; see poi_types"`
	Subtype    string `json:"subtype,omitempty"`
	NamePrefix string `json:"name_prefix,omitempty"`
	Near       *Near  `json:"near,omitempty"`
	Limit      int    `json:"limit,omitempty" jsonschema:"page size; default 20, max 100"`
	Offset     int    `json:"offset,omitempty"`
}

func (t *tools) findPOIs(ctx context.Context, _ *mcp.CallToolRequest, in findPOIsIn) (*mcp.CallToolResult, Page[client.POI], error) {
	var zero Page[client.POI]
	q, a, err := t.listQuery(ctx, in.Near, nil, in.Limit, in.Offset)
	if err != nil {
		return nil, zero, err
	}
	setIf(q, "type", in.Type)
	setIf(q, "subtype", in.Subtype)
	setIf(q, "name", in.NamePrefix)
	p, err := t.c.POIs(ctx, q)
	if err != nil {
		return nil, zero, err
	}
	return nil, page(p.Rows, p.Total, p.Limit, p.Offset, a), nil
}

type noIn struct{}

func (t *tools) poiTypes(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, List[client.POITypeCount], error) {
	rows, err := t.c.POITypes(ctx)
	return nil, list(rows), err
}

// ── explore ─────────────────────────────────────────────

type hottestIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"default 15, max 50"`
}

func (t *tools) hottest(ctx context.Context, _ *mcp.CallToolRequest, in hottestIn) (*mcp.CallToolResult, List[client.HottestProject], error) {
	rows, err := t.c.Hottest(ctx, in.Limit)
	return nil, list(rows), err
}

type districtMedianIn struct {
	District   string   `json:"district" jsonschema:"postal district 1-28"`
	WindowDays int      `json:"window_days,omitempty" jsonschema:"trailing window ending at the latest URA contract date; default 90"`
	MinAreaSqm *float64 `json:"min_area_sqm,omitempty"`
	MaxAreaSqm *float64 `json:"max_area_sqm,omitempty"`
}

func (t *tools) districtMedian(ctx context.Context, _ *mcp.CallToolRequest, in districtMedianIn) (*mcp.CallToolResult, *client.DistrictMedianResp, error) {
	d, err := normDistrict(in.District)
	if err != nil {
		return nil, nil, err
	}
	if d == "" {
		return nil, nil, errors.New("district is required")
	}
	q := url.Values{"district": {d}}
	if in.WindowDays > 0 {
		q.Set("window_days", strconv.Itoa(in.WindowDays))
	}
	setNum(q, "min_area_sqm", in.MinAreaSqm)
	setNum(q, "max_area_sqm", in.MaxAreaSqm)
	r, err := t.c.DistrictMedian(ctx, q)
	return nil, r, err
}

// ── CEA ─────────────────────────────────────────────────

type ceaIn struct {
	RegistrationNo string `json:"registration_no" jsonschema:"CEA registration number, e.g. R012345A"`
}

func (t *tools) ceaLookup(ctx context.Context, _ *mcp.CallToolRequest, in ceaIn) (*mcp.CallToolResult, *client.CEASalesperson, error) {
	r, err := t.c.CEALookup(ctx, normUpper(in.RegistrationNo))
	if errors.Is(err, client.ErrNotFound) {
		return nil, nil, fmt.Errorf("%s is not in the CEA register", normUpper(in.RegistrationNo))
	}
	return nil, r, err
}

type ceaStatsIn struct {
	RegistrationNo string `json:"registration_no" jsonschema:"CEA registration number"`
	Mode           string `json:"mode,omitempty" jsonschema:"sale (default) or rent"`
}

func (t *tools) ceaStats(ctx context.Context, _ *mcp.CallToolRequest, in ceaStatsIn) (*mcp.CallToolResult, *client.CEAPracticeStats, error) {
	mode := in.Mode
	if mode == "" {
		mode = "sale"
	}
	if err := oneOf("mode", mode, "sale", "rent"); err != nil {
		return nil, nil, err
	}
	r, err := t.c.CEAPracticeStats(ctx, normUpper(in.RegistrationNo), mode)
	if err != nil {
		return nil, nil, err
	}
	if r.Monthly == nil {
		r.Monthly = []client.CEAStatsMonthly{}
	}
	if r.Towns == nil {
		r.Towns = []client.CEAStatsTown{}
	}
	if r.Representation == nil {
		r.Representation = []client.CEAStatsCategory{}
	}
	if r.TxTypes == nil {
		r.TxTypes = []client.CEAStatsCategory{}
	}
	return nil, r, nil
}

// ── ops ─────────────────────────────────────────────────

// SourceFreshness is one data source's latest sync.
type SourceFreshness struct {
	Source       string  `json:"source"`
	Status       string  `json:"status" jsonschema:"success, error, running, skipped, or never_run"`
	LastRunAt    *string `json:"last_run_at,omitempty"`
	RowsUpserted int     `json:"rows_upserted,omitempty"`
	Error        string  `json:"error,omitempty"`
}

func (t *tools) dataFreshness(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, List[SourceFreshness], error) {
	runs, known, err := t.c.SyncStatus(ctx)
	if err != nil {
		return nil, List[SourceFreshness]{}, err
	}
	by := map[string]client.SyncRun{}
	for _, r := range runs {
		by[r.Source] = r
	}
	out := []SourceFreshness{}
	seen := map[string]bool{}
	add := func(src string) {
		if seen[src] {
			return
		}
		seen[src] = true
		r, ok := by[src]
		if !ok {
			out = append(out, SourceFreshness{Source: src, Status: "never_run"})
			return
		}
		at := r.StartedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		ts := at.Format("2006-01-02T15:04:05Z07:00")
		out = append(out, SourceFreshness{Source: src, Status: r.Status, LastRunAt: &ts, RowsUpserted: r.RowsUpserted, Error: r.Error})
	}
	for _, s := range known {
		add(s)
	}
	for _, r := range runs {
		add(r.Source)
	}
	return nil, list(out), nil
}

// ── shared ──────────────────────────────────────────────

// listQuery starts a list request: resolves the anchor, adds geo filters
// and the clamped page.
func (t *tools) listQuery(ctx context.Context, n *Near, h *HasPOI, limit, offset int) (url.Values, *Anchor, error) {
	if offset < 0 {
		return nil, nil, errors.New("offset must be >= 0")
	}
	a, err := resolveAnchor(ctx, t.c, n)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{"limit": {strconv.Itoa(clampLimit(limit))}}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if err := addGeo(q, a, h); err != nil {
		return nil, nil, err
	}
	return q, a, nil
}

func uraCommon(q url.Values, project, district, propertyType, from, to string) error {
	d, err := normDistrict(district)
	if err != nil {
		return err
	}
	f, err := normDate("from_date", from)
	if err != nil {
		return err
	}
	tt, err := normDate("to_date", to)
	if err != nil {
		return err
	}
	setIf(q, "project", normUpper(project))
	setIf(q, "district", d)
	setIf(q, "property_type", normPropertyType(propertyType))
	setIf(q, "min_date", f)
	setIf(q, "max_date", tt)
	return nil
}

func setSort(q url.Values, sort string, a *Anchor, allowed ...string) error {
	if sort == "" {
		return nil
	}
	if err := oneOf("sort", sort, allowed...); err != nil {
		return err
	}
	if sort == "distance_asc" && a == nil {
		return errors.New("sort distance_asc needs near")
	}
	q.Set("sort", sort)
	return nil
}

func oneOf(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s %q: want one of %v", field, v, allowed)
}
